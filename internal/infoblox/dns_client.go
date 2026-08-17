package infoblox

import (
	"context"
	"fmt"
	"net/http"
)

// DNSRecord mirrors the subset of the DDI v1 dns/record resource that the
// operator reads or writes. Kept alongside AddressBlock in the same client
// deliberately — both are thin views onto the same DDI v1 API and the same
// service-account credentials, so one Client covers both CRDs.
type DNSRecord struct {
	ID      string            `json:"id,omitempty"`
	Zone    string            `json:"zone"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Value   string            `json:"value"`
	TTL     int32             `json:"ttl"`
	Tags    map[string]string `json:"tags,omitempty"`
	Comment string            `json:"comment,omitempty"`
}

// CreateDNSRecord creates a DNS record in the given zone. Unlike IPAM
// allocation there's no "next available" concept here — the caller always
// specifies the exact name/value, since DNS records are identified by name,
// not carved out of a pool.
func (c *Client) CreateDNSRecord(ctx context.Context, zone, name, recordType, value string, ttl int32, tags map[string]string, comment string) (*DNSRecord, error) {
	if ttl <= 0 {
		ttl = 300
	}
	reqBody := map[string]any{
		"zone":    zone,
		"name":    name,
		"type":    recordType,
		"value":   value,
		"ttl":     ttl,
		"tags":    tags,
		"comment": comment,
	}

	var out DNSRecord
	if err := c.do(ctx, http.MethodPost, "/api/ddi/v1/dns/record", reqBody, &out); err != nil {
		return nil, fmt.Errorf("create dns record %s.%s: %w", name, zone, err)
	}
	return &out, nil
}

// GetDNSRecord fetches the current state of a DNS record by its Infoblox
// resource ref (e.g. "dns/record/<uuid>"). Used for drift detection, same
// role as Client.Get plays for address blocks.
func (c *Client) GetDNSRecord(ctx context.Context, ref string) (*DNSRecord, error) {
	var out DNSRecord
	if err := c.do(ctx, http.MethodGet, "/api/ddi/v1/"+ref, nil, &out); err != nil {
		return nil, fmt.Errorf("get dns record %q: %w", ref, err)
	}
	return &out, nil
}

// DeleteDNSRecord deletes the record. Idempotent: a 404 is treated as
// already-deleted, not an error — same contract as Client.Release, so the
// controller's finalizer-driven cleanup path is safe to retry either way.
func (c *Client) DeleteDNSRecord(ctx context.Context, ref string) error {
	err := c.do(ctx, http.MethodDelete, "/api/ddi/v1/"+ref, nil, nil)
	if err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete dns record %q: %w", ref, err)
	}
	return nil
}
