package loadbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Request is the JSON body sent to POST /process.
type Request struct {
	Payload   string `json:"payload"`
	PayloadID string `json:"payload_id"`
}

// Client is a minimal POST /process HTTP client with strict response
// validation. It uses only the standard library.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client that posts to baseURL/process with the given
// per-request timeout.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Do sends one POST /process request and returns the result string. It
// validates the HTTP status, the strict response JSON shape (exactly one
// string field named result) and returns an error otherwise. It never returns
// or logs the payload, the result or any plaintext on error.
func (c *Client) Do(ctx context.Context, req Request) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/process", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	dec := json.NewDecoder(resp.Body)
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return "", fmt.Errorf("invalid JSON response: %w", err)
	}
	// Strict JSON boundary: after the one response object, the body must
	// contain no further JSON value. Trailing whitespace is allowed, but a
	// second JSON value or any non-whitespace trailing content is rejected.
	// A single decoder is used for both the object and the EOF check so the
	// decoder's internal buffer cannot hide trailing bytes from the check.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("response contains trailing content after the JSON object")
	}
	if len(raw) != 1 {
		return "", fmt.Errorf("response has %d fields, want exactly 1", len(raw))
	}
	val, ok := raw["result"]
	if !ok {
		return "", fmt.Errorf("response missing result field")
	}
	var result string
	if err := json.Unmarshal(val, &result); err != nil {
		return "", fmt.Errorf("result is not a string")
	}
	return result, nil
}
