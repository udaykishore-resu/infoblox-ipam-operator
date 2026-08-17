// Command mock-infoblox is a minimal, in-memory stand-in for the Infoblox
// Universal DDI Portal API (csp.infoblox.com/api/ddi/v1). It exists purely
// so this project can be demoed and integration-tested end-to-end without
// a real Infoblox account or network access — same request/response shapes
// as the real DDI v1 IPAM API, enough to exercise the operator's full
// allocate / drift-check / release lifecycle.
//
// It is NOT a substitute for the real API in production and intentionally
// implements only the subset of behavior internal/infoblox.Client uses.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// randomID returns a short random hex string, used as a stand-in for the
// UUIDs the real Infoblox API assigns. Stdlib-only so this mock server has
// zero third-party dependencies.
func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ipSpacePool defines the base CIDR each named IP space allocates from, and
// a simple monotonic counter used to hand out non-overlapping blocks. This
// is deliberately naive (no real CIDR-math reuse of released blocks) —
// sufficient for demo purposes, not a general-purpose allocator.
type ipSpacePool struct {
	baseNet   net.IP
	baseCIDR  int
	nextOctet int // demo-grade: bump the 3rd octet for each new /N block
}

type addressBlock struct {
	ID      string            `json:"id"`
	Address string            `json:"address"`
	CIDR    int32             `json:"cidr"`
	Space   string            `json:"space"`
	Tags    map[string]string `json:"tags,omitempty"`
	Comment string            `json:"comment,omitempty"`
}

// dnsRecord mirrors internal/infoblox.DNSRecord's wire shape.
type dnsRecord struct {
	ID      string            `json:"id"`
	Zone    string            `json:"zone"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Value   string            `json:"value"`
	TTL     int32             `json:"ttl"`
	Tags    map[string]string `json:"tags,omitempty"`
	Comment string            `json:"comment,omitempty"`
}

type server struct {
	mu         sync.Mutex
	blocks     map[string]*addressBlock
	dnsRecords map[string]*dnsRecord
	knownZones map[string]bool
	pools      map[string]*ipSpacePool
	logger     *slog.Logger
}

func newServer(logger *slog.Logger) *server {
	return &server{
		blocks:     make(map[string]*addressBlock),
		dnsRecords: make(map[string]*dnsRecord),
		knownZones: map[string]bool{
			// Pre-seeded demo zones, matching config/samples/dnsrecordclaim_sample.yaml
			"example.com":        true,
			"internal.corp.demo": true,
		},
		pools: map[string]*ipSpacePool{
			// Pre-seeded demo IP spaces, matching config/samples/ipspaceclaim_sample.yaml
			"prod-eks-us-east-1":  {baseNet: net.ParseIP("10.44.0.0"), baseCIDR: 16, nextOctet: 12},
			"staging-gke-central": {baseNet: net.ParseIP("10.60.0.0"), baseCIDR: 16, nextOctet: 0},
		},
		logger: logger,
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ddi/v1/ipam/address_block", s.handleCollection)
	mux.HandleFunc("/api/ddi/v1/ipam/address_block/", s.handleItem)
	mux.HandleFunc("/api/ddi/v1/dns/record", s.handleDNSCollection)
	mux.HandleFunc("/api/ddi/v1/dns/record/", s.handleDNSItem)
	// /admin/* endpoints are NOT part of the real Infoblox API — they exist
	// only so the demo script can simulate an out-of-band change ("someone
	// edited it in the Infoblox portal") to trigger drift detection.
	mux.HandleFunc("/admin/blocks", s.handleAdminList)
	mux.HandleFunc("/admin/blocks/", s.handleAdminMutate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return withLogging(s.logger, mux)
}

func (s *server) handleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.create(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Space         string            `json:"space"`
		CIDR          int32             `json:"cidr"`
		NextAvailable bool              `json:"next_available"`
		Address       string            `json:"address"`
		Tags          map[string]string `json:"tags"`
		Comment       string            `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.pools[req.Space]
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("unknown ip space %q", req.Space))
		return
	}

	block := &addressBlock{
		ID:      "ipam/address_block/" + randomID(),
		Space:   req.Space,
		Tags:    req.Tags,
		Comment: req.Comment,
	}

	if req.Address != "" {
		// Fixed allocation.
		parts := strings.SplitN(req.Address, "/", 2)
		block.Address = parts[0]
		if len(parts) == 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				block.CIDR = int32(n)
			}
		}
	} else {
		// next_available: scan forward from the pool's current pointer,
		// skipping any octet already in use by a live block in this space.
		// This is what actually makes allocation collision-safe — merely
		// incrementing a counter (the previous behavior) trusts the
		// counter's memory of what it already handed out, but says nothing
		// about what's *actually* allocated right now. That distinction
		// matters here because this is an in-memory demo server: if the
		// process restarts, the counter resets to its seed value while any
		// blocks a caller still believes are live were wiped along with it.
		// Scanning against the current block set means a restarted server
		// can't silently double-allocate an octet that's still in use —
		// though note it *can* re-hand-out an octet whose original block
		// was itself lost in the same restart, since there is no
		// persistence. That's an inherent limitation of an in-memory demo
		// fixture, not something this fix claims to solve.
		octets := pool.baseNet.To4()
		candidate := pool.nextOctet
		found := -1
		for i := 0; i < 256; i++ {
			o := (candidate + i) % 256
			taken := false
			for _, b := range s.blocks {
				if b.Space == req.Space && b.Address == fmt.Sprintf("%d.%d.%d.0", octets[0], octets[1], o) {
					taken = true
					break
				}
			}
			if !taken {
				found = o
				break
			}
		}
		if found == -1 {
			writeErr(w, http.StatusConflict, fmt.Sprintf("ip space %q exhausted (demo allocator caps at 256 blocks)", req.Space))
			return
		}
		block.Address = fmt.Sprintf("%d.%d.%d.0", octets[0], octets[1], found)
		block.CIDR = req.CIDR
		pool.nextOctet = found + 1
	}

	s.blocks[block.ID] = block
	s.logger.Info("allocated address block", "id", block.ID, "cidr", fmt.Sprintf("%s/%d", block.Address, block.CIDR), "space", req.Space)

	writeJSON(w, http.StatusCreated, block)
}

func (s *server) handleItem(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimPrefix(r.URL.Path, "/api/ddi/v1/")

	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		block, ok := s.blocks[ref]
		if !ok {
			writeErr(w, http.StatusNotFound, "address block not found")
			return
		}
		writeJSON(w, http.StatusOK, block)

	case http.MethodDelete:
		if _, ok := s.blocks[ref]; !ok {
			writeErr(w, http.StatusNotFound, "address block not found")
			return
		}
		delete(s.blocks, ref)
		s.logger.Info("released address block", "id", ref)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- DNS record endpoints ---

func (s *server) handleDNSCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.knownZones[req.Zone] {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("unknown dns zone %q", req.Zone))
		return
	}

	rec := &dnsRecord{
		ID:      "dns/record/" + randomID(),
		Zone:    req.Zone,
		Name:    req.Name,
		Type:    req.Type,
		Value:   req.Value,
		TTL:     req.TTL,
		Tags:    req.Tags,
		Comment: req.Comment,
	}
	if rec.TTL <= 0 {
		rec.TTL = 300
	}

	s.dnsRecords[rec.ID] = rec
	s.logger.Info("created dns record", "id", rec.ID, "fqdn", rec.Name+"."+rec.Zone, "type", rec.Type, "value", rec.Value)

	writeJSON(w, http.StatusCreated, rec)
}

func (s *server) handleDNSItem(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimPrefix(r.URL.Path, "/api/ddi/v1/")

	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		rec, ok := s.dnsRecords[ref]
		if !ok {
			writeErr(w, http.StatusNotFound, "dns record not found")
			return
		}
		writeJSON(w, http.StatusOK, rec)

	case http.MethodDelete:
		if _, ok := s.dnsRecords[ref]; !ok {
			writeErr(w, http.StatusNotFound, "dns record not found")
			return
		}
		delete(s.dnsRecords, ref)
		s.logger.Info("deleted dns record", "id", ref)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- demo-only admin endpoints, used to simulate drift ---

func (s *server) handleAdminList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]*addressBlock, 0, len(s.blocks))
	for _, b := range s.blocks {
		list = append(list, b)
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) handleAdminMutate(w http.ResponseWriter, r *http.Request) {
	ref := "ipam/address_block/" + strings.TrimPrefix(r.URL.Path, "/admin/blocks/")

	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodDelete:
		// Simulates a network engineer deleting the allocation directly in
		// the Infoblox portal, outside of Kubernetes — this is what the
		// operator's drift detector should catch on its next poll.
		delete(s.blocks, ref)
		s.logger.Warn("ADMIN: simulated out-of-band deletion (drift)", "id", ref)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	addr := os.Getenv("MOCK_INFOBLOX_ADDR")
	if addr == "" {
		addr = ":9090"
	}

	s := newServer(logger)
	logger.Info("mock-infoblox listening", "addr", addr, "seeded_spaces", []string{"prod-eks-us-east-1", "staging-gke-central"})
	if err := http.ListenAndServe(addr, s.routes()); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
