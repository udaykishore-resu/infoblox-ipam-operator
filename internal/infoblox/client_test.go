package infoblox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestServer spins up a tiny in-process fake of the Infoblox DDI v1 API
// covering exactly the request/response shapes internal/infoblox.Client
// depends on. It intentionally does not import cmd/mock-infoblox — the two
// serve different purposes (this one is minimal and assertion-friendly;
// that one is a fuller demo fixture) and keeping this package
// dependency-free means these tests run with plain `go test`, no network.
func newTestServer(t *testing.T) (*httptest.Server, map[string]*AddressBlock) {
	t.Helper()
	store := map[string]*AddressBlock{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ddi/v1/ipam/address_block", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Space         string            `json:"space"`
			CIDR          int32             `json:"cidr"`
			Address       string            `json:"address"`
			NextAvailable bool              `json:"next_available"`
			Tags          map[string]string `json:"tags"`
			Comment       string            `json:"comment"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Space == "does-not-exist" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown space"})
			return
		}

		block := &AddressBlock{
			ID:      "ipam/address_block/test-1",
			Space:   req.Space,
			Tags:    req.Tags,
			Comment: req.Comment,
		}
		if req.Address != "" {
			block.Address = "10.0.0.0"
			block.CIDR = 28
		} else {
			block.Address = "10.1.0.0"
			block.CIDR = req.CIDR
		}
		store[block.ID] = block

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(block)
	})

	mux.HandleFunc("/api/ddi/v1/ipam/address_block/test-1", func(w http.ResponseWriter, r *http.Request) {
		ref := "ipam/address_block/test-1"
		switch r.Method {
		case http.MethodGet:
			block, ok := store[ref]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(block)
		case http.MethodDelete:
			if _, ok := store[ref]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(store, ref)
			w.WriteHeader(http.StatusNoContent)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store
}

func TestAllocateNextAvailable_Success(t *testing.T) {
	srv, _ := newTestServer(t)
	c := NewClient(srv.URL, "test-token")

	block, err := c.AllocateNextAvailable(context.Background(), "prod-eks-us-east-1", 28,
		map[string]string{"team": "payments"}, "test allocation")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block.CIDR != 28 {
		t.Errorf("expected cidr 28, got %d", block.CIDR)
	}
	if block.ID == "" {
		t.Error("expected non-empty allocation ID")
	}
}

func TestAllocateNextAvailable_UnknownSpace(t *testing.T) {
	srv, _ := newTestServer(t)
	c := NewClient(srv.URL, "test-token")

	_, err := c.AllocateNextAvailable(context.Background(), "does-not-exist", 28, nil, "")
	if err == nil {
		t.Fatal("expected error for unknown ip space, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError in chain, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", apiErr.StatusCode)
	}
}

func TestGet_RoundTrip(t *testing.T) {
	srv, _ := newTestServer(t)
	c := NewClient(srv.URL, "test-token")

	created, err := c.AllocateNextAvailable(context.Background(), "prod-eks-us-east-1", 28, nil, "")
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	got, err := c.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Address != created.Address {
		t.Errorf("expected address %q, got %q", created.Address, got.Address)
	}
}

func TestRelease_IdempotentOn404(t *testing.T) {
	srv, _ := newTestServer(t)
	c := NewClient(srv.URL, "test-token")

	created, err := c.AllocateNextAvailable(context.Background(), "prod-eks-us-east-1", 28, nil, "")
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if err := c.Release(context.Background(), created.ID); err != nil {
		t.Fatalf("first release should succeed: %v", err)
	}
	// Second release of an already-released block must NOT error — this is
	// what makes the controller's delete-finalizer path safe to retry.
	if err := c.Release(context.Background(), created.ID); err != nil {
		t.Fatalf("second release should be a no-op, got error: %v", err)
	}
}
