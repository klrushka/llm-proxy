// Package smoke implements a reusable, standard-library-only live smoke client
// for an already-deployed PII service. It checks /health/live, /health/ready
// and the full POST /v1/runtime/chat product flow (mask -> LLM -> demask) using
// only synthetic Russian PII, and verifies that the final response restored the
// synthetic values. It never prints request/response bodies, credentials or any
// sensitive data, and it fails closed on any contract violation.
package smoke

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults for the smoke run.
const (
	// DefaultTimeout bounds the whole smoke run and every HTTP request.
	DefaultTimeout = 30 * time.Second
	// maxBodyBytes bounds the size of any response body read by the client.
	// It is deliberately small: the service returns tiny JSON objects.
	maxBodyBytes = 1 << 20 // 1 MiB
)

// Synthetic values used only for the live smoke. They are synthetic Russian PII
// and must never be replaced with real personal data.
const syntheticText = "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"

// syntheticValues are the exact personal values that must all be restored in
// the final runtime response after the full mask -> LLM -> demask path. It is a
// package-level slice (immutable by convention) because a slice cannot be a
// constant.
var syntheticValues = []string{"Иванов Иван", "+7 900 123-45-67", "ivanov@example.com"}

// Config configures a smoke run.
type Config struct {
	// BaseURL is the base URL of the deployed service, e.g.
	// http://127.0.0.1:8080 or https://pii.example.com.
	BaseURL string
	// Timeout bounds the whole run and every HTTP request.
	Timeout time.Duration
	// AllowSelfSigned permits a self-signed TLS certificate. It is applicable
	// only when BaseURL uses https; it is rejected for http.
	AllowSelfSigned bool
}

// Validate checks the configuration for a runnable smoke test.
func (c Config) Validate() error {
	if c.BaseURL == "" {
		return errors.New("base URL is required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		// Fixed error: never echo the supplied raw URL, which may carry
		// credentials.
		return errors.New("invalid base URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("base URL scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("base URL must include a host")
	}
	if u.User != nil {
		return errors.New("base URL must not include userinfo")
	}
	if u.RawQuery != "" {
		return errors.New("base URL must not include a query")
	}
	if u.Fragment != "" {
		return errors.New("base URL must not include a fragment")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if c.AllowSelfSigned && u.Scheme != "https" {
		return errors.New("allow-self-signed is applicable only to an https base URL")
	}
	return nil
}

// Client is a minimal live-smoke HTTP client with strict response validation.
// It uses only the standard library.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client that talks to baseURL with the given timeout. When
// allowSelfSigned is true the client accepts any TLS certificate; it is the
// caller's responsibility to enable it only for an https base URL. Redirects
// are disabled so a 3xx is treated as the original non-2xx response and no
// follow-up request is made.
func NewClient(baseURL string, timeout time.Duration, allowSelfSigned bool) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if allowSelfSigned {
		// nosemgrep: go-insecure-tls -- only enabled by the explicit HTTPS -allow-self-signed checker flag; never for http
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- explicit opt-in for self-signed checkers
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// RuntimeRequest is the JSON body sent to POST /v1/runtime/chat.
type RuntimeRequest struct {
	Text    string `json:"text"`
	ScopeID string `json:"scope_id"`
}

// RuntimeResponse is the JSON body returned by a successful
// POST /v1/runtime/chat. It contains exactly one field, result.
type RuntimeResponse struct {
	Result string `json:"result"`
}

// Run performs the full live smoke: health/live, health/ready and the runtime
// chat round trip. The overall timeout bounds the whole run; each HTTP request
// is additionally bounded by the client timeout. It returns a short,
// CI-friendly summary. It never prints or returns request/response bodies,
// credentials or sensitive data.
func (c *Client) Run(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.http.Timeout)
	defer cancel()

	if err := c.checkHealth(ctx, "/health/live"); err != nil {
		return "", fmt.Errorf("health/live: %w", err)
	}
	if err := c.checkHealth(ctx, "/health/ready"); err != nil {
		return "", fmt.Errorf("health/ready: %w", err)
	}

	result, err := c.runtimeChat(ctx)
	if err != nil {
		return "", fmt.Errorf("runtime chat: %w", err)
	}
	for _, v := range syntheticValues {
		if !strings.Contains(result, v) {
			return "", errors.New("runtime response did not restore all synthetic personal values")
		}
	}

	return "live smoke ok: health/live, health/ready, runtime chat round trip restored synthetic values", nil
}

// checkHealth performs a GET on the given health path and requires a 2xx
// status. It reads the body within the size bound and discards it.
func (c *Client) checkHealth(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if _, err := readBody(resp.Body); err != nil {
		return err
	}
	return nil
}

// runtimeChat performs the full POST /v1/runtime/chat round trip and returns
// the restored result. It validates the HTTP status, the strict response JSON
// shape (exactly one string field named result) and the body size bound.
func (c *Client) runtimeChat(ctx context.Context) (string, error) {
	body, err := json.Marshal(RuntimeRequest{Text: syntheticText, ScopeID: "live-smoke"})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/runtime/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	raw, err := readBody(resp.Body)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return "", fmt.Errorf("invalid JSON response: %w", err)
	}
	// Strict JSON boundary: after the one response object, the body must
	// contain no further JSON value. Trailing whitespace is allowed, but a
	// second JSON value or any non-whitespace trailing content is rejected.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("response contains trailing content after the JSON object")
	}
	if len(obj) != 1 {
		return "", fmt.Errorf("response has %d fields, want exactly 1", len(obj))
	}
	val, ok := obj["result"]
	if !ok {
		return "", errors.New("response missing result field")
	}
	var result string
	if err := json.Unmarshal(val, &result); err != nil {
		return "", errors.New("result is not a string")
	}
	return result, nil
}

// readBody reads at most maxBodyBytes+1 bytes from r and rejects the body when
// it exceeds maxBodyBytes. It returns the body bytes on success. The limit is
// enforced on the raw byte length before any parsing, so a complete JSON value
// followed by more than maxBodyBytes of trailing whitespace is rejected.
func readBody(r io.Reader) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(buf) > maxBodyBytes {
		return nil, errors.New("response body exceeds size limit")
	}
	return buf, nil
}
