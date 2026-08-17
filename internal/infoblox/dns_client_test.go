package infoblox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newDNSTestServer is the DNS-record counterpart to newTestServer in
// client_test.go — a minimal in-process fake covering exactly what
// Client's DNS methods depend on.
func newDNSTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := map[string]*DNSRecord{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ddi/v1/dns/record", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Zone    string            `json:"zone"`
			Name    string            `json:"name"`
			Type    string            `json:"type"`
			Value   string            `json:"value"`
			TTL     int32             `json:"ttl"`
			Tags    map[string]string `json:"tags"`
			Comment string            `json:"comment"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Zone == "does-not-exist.example.com" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown zone"})
			return
		}

		rec := &DNSRecord{
			ID:      "dns/record/test-1",
			Zone:    req.Zone,
			Name:    req.Name,
			Type:    req.Type,
			Value:   req.Value,
			TTL:     req.TTL,
			Tags:    req.Tags,
			Comment: req.Comment,
		}
		store[rec.ID] = rec
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(rec)
	})

	mux.HandleFunc("/api/ddi/v1/dns/record/test-1", func(w http.ResponseWriter, r *http.Request) {
		ref := "dns/record/test-1"
		switch r.Method {
		case http.MethodGet:
			rec, ok := store[ref]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(rec)
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
	return srv
}

func TestCreateDNSRecord_Success(t *testing.T) {
	srv := newDNSTestServer(t)
	c := NewClient(srv.URL, "test-token")

	rec, err := c.CreateDNSRecord(context.Background(), "example.com", "checkout", "A", "10.44.12.5", 300,
		map[string]string{"team": "payments"}, "test record")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Zone != "example.com" || rec.Name != "checkout" || rec.Type != "A" {
		t.Errorf("unexpected record: %+v", rec)
	}
	if rec.ID == "" {
		t.Error("expected non-empty record ID")
	}
}

func TestCreateDNSRecord_DefaultsTTL(t *testing.T) {
	srv := newDNSTestServer(t)
	c := NewClient(srv.URL, "test-token")

	rec, err := c.CreateDNSRecord(context.Background(), "example.com", "checkout", "A", "10.44.12.5", 0, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.TTL != 300 {
		t.Errorf("expected default TTL 300, got %d", rec.TTL)
	}
}

func TestCreateDNSRecord_UnknownZone(t *testing.T) {
	srv := newDNSTestServer(t)
	c := NewClient(srv.URL, "test-token")

	_, err := c.CreateDNSRecord(context.Background(), "does-not-exist.example.com", "checkout", "A", "10.44.12.5", 300, nil, "")
	if err == nil {
		t.Fatal("expected error for unknown zone, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError in chain, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", apiErr.StatusCode)
	}
}

func TestGetDNSRecord_RoundTrip(t *testing.T) {
	srv := newDNSTestServer(t)
	c := NewClient(srv.URL, "test-token")

	created, err := c.CreateDNSRecord(context.Background(), "example.com", "checkout", "CNAME", "lb.example.com", 300, nil, "")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	got, err := c.GetDNSRecord(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Value != created.Value {
		t.Errorf("expected value %q, got %q", created.Value, got.Value)
	}
}

func TestDeleteDNSRecord_IdempotentOn404(t *testing.T) {
	srv := newDNSTestServer(t)
	c := NewClient(srv.URL, "test-token")

	created, err := c.CreateDNSRecord(context.Background(), "example.com", "checkout", "A", "10.44.12.5", 300, nil, "")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := c.DeleteDNSRecord(context.Background(), created.ID); err != nil {
		t.Fatalf("first delete should succeed: %v", err)
	}
	if err := c.DeleteDNSRecord(context.Background(), created.ID); err != nil {
		t.Fatalf("second delete should be a no-op, got error: %v", err)
	}
}
