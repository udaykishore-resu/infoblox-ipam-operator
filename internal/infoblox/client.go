// Package infoblox provides a minimal, purpose-built client for the Infoblox
// Universal DDI v1 API (https://csp.infoblox.com/api/ddi/v1/...), scoped to
// exactly what the IPAM operator needs: address block allocation, lookup,
// and release. It intentionally does not attempt to be a full SDK.
package infoblox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to the Infoblox Universal DDI Portal API.
type Client struct {
	baseURL    string // e.g. https://csp.infoblox.com
	apiToken   string
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default HTTP client (useful for tests / custom transport).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// NewClient constructs a Client. apiToken should be a service-account token
// per Infoblox's "service account users" pattern, never a personal login.
func NewClient(baseURL, apiToken string, opts ...Option) *Client {
	c := &Client{
		baseURL:  baseURL,
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// AddressBlock mirrors the subset of the DDI v1 ipam/address_block resource
// that the operator reads or writes.
type AddressBlock struct {
	ID      string            `json:"id,omitempty"`
	Address string            `json:"address"`
	CIDR    int32             `json:"cidr"`
	Space   string            `json:"space"` // ip_space resource ref
	Tags    map[string]string `json:"tags,omitempty"`
	Comment string            `json:"comment,omitempty"`
}

// AllocateNextAvailable requests the next available address block of the
// given prefix length from the named IP space, tagging it with the supplied
// extensible attributes for traceability back to the owning K8s object.
func (c *Client) AllocateNextAvailable(ctx context.Context, ipSpaceRef string, cidrSize int32, tags map[string]string, comment string) (*AddressBlock, error) {
	reqBody := map[string]any{
		"space":          ipSpaceRef,
		"cidr":           cidrSize,
		"next_available": true,
		"tags":           tags,
		"comment":        comment,
	}

	var out AddressBlock
	if err := c.do(ctx, http.MethodPost, "/api/ddi/v1/ipam/address_block", reqBody, &out); err != nil {
		return nil, fmt.Errorf("allocate address block in space %q: %w", ipSpaceRef, err)
	}
	return &out, nil
}

// AllocateFixed requests a specific, already-known CIDR — used when
// migrating a pre-existing static allocation under operator management.
func (c *Client) AllocateFixed(ctx context.Context, ipSpaceRef, cidr string, tags map[string]string, comment string) (*AddressBlock, error) {
	reqBody := map[string]any{
		"space":   ipSpaceRef,
		"address": cidr,
		"tags":    tags,
		"comment": comment,
	}

	var out AddressBlock
	if err := c.do(ctx, http.MethodPost, "/api/ddi/v1/ipam/address_block", reqBody, &out); err != nil {
		return nil, fmt.Errorf("allocate fixed block %q in space %q: %w", cidr, ipSpaceRef, err)
	}
	return &out, nil
}

// Get fetches the current state of an address block by its Infoblox resource
// ref (e.g. "ipam/address_block/<uuid>"). Used for drift detection.
func (c *Client) Get(ctx context.Context, ref string) (*AddressBlock, error) {
	var out AddressBlock
	if err := c.do(ctx, http.MethodGet, "/api/ddi/v1/"+ref, nil, &out); err != nil {
		return nil, fmt.Errorf("get address block %q: %w", ref, err)
	}
	return &out, nil
}

// Release deletes the address block, returning it to the pool. Idempotent:
// a 404 from Infoblox is treated as already-released, not an error.
func (c *Client) Release(ctx context.Context, ref string) error {
	err := c.do(ctx, http.MethodDelete, "/api/ddi/v1/"+ref, nil, nil)
	if err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("release address block %q: %w", ref, err)
	}
	return nil
}

// APIError wraps a non-2xx response from Infoblox with enough context to
// let callers make retry/backoff decisions (e.g. 429 vs 4xx vs 5xx).
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("infoblox api error: status=%d body=%s", e.StatusCode, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("unmarshal response body: %w", err)
		}
	}
	return nil
}
