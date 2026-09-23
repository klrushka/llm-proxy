// Package llmclient is a small stdlib-only HTTP client for a generic
// chat-completions-compatible downstream LLM endpoint. It is the runnable LLM
// boundary for the runtime flow: it sends only protected text (opaque PII
// tokens) and returns the model's modified protected text so it can be
// restored afterwards.
//
// The outbound JSON carries the configured model, a short system instruction
// requiring opaque PII tokens such as <EMAIL_...> to be preserved byte-for-byte,
// and the protected text as the user message. The response is parsed from
// choices[0].message.content.
//
// The client never logs or returns the request text, protected text, tokens,
// API key, response body or upstream error details. It returns fixed safe
// sentinel errors only.
package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// systemInstruction is the short system prompt sent to the downstream LLM. It
// requires opaque PII tokens to be preserved byte-for-byte so they can be
// restored afterwards. It never contains real values.
const systemInstruction = "Preserve every opaque PII token such as <EMAIL_...> byte-for-byte. Do not reveal, replace, or alter any token. Rewrite only the surrounding text."

// maxResponseBytes bounds the accepted response body so an oversized or
// unbounded upstream response is rejected rather than buffered without limit.
const maxResponseBytes = 1 << 20 // 1 MiB

// Safe sentinel errors. Their Error() text is fixed and never embeds request
// text, protected text, tokens, the API key, a response body or upstream error
// detail.
var (
	// ErrUnavailable matches any transport-level failure: connection errors,
	// non-2xx responses, caller cancellation and timeouts. It wraps the
	// underlying error so context identity stays reachable via errors.Is.
	ErrUnavailable = errors.New("llmclient: downstream llm unavailable")
	// ErrInvalidResponse matches a structurally invalid response: bad JSON,
	// trailing JSON, missing or empty content, or an oversized body.
	ErrInvalidResponse = errors.New("llmclient: invalid downstream response")
	// ErrInvalidConfig matches locally rejected configuration before any HTTP
	// request is made.
	ErrInvalidConfig = errors.New("llmclient: invalid configuration")
)

// Client calls a chat-completions-compatible downstream LLM endpoint.
type Client struct {
	endpoint string
	model    string
	apiKey   string
	http     *http.Client
}

// Config configures a Client.
type Config struct {
	// URL is the chat-completions endpoint. It must be an http(s) URL with a
	// non-empty host.
	URL string
	// Model is the model identifier sent in the outbound JSON.
	Model string
	// APIKey is optional. When non-empty it is sent as an Authorization: Bearer
	// header; when empty no Authorization header is sent.
	APIKey string
	// Timeout bounds each request. It must be greater than zero.
	Timeout time.Duration
	// Transport is the optional HTTP transport, for example an instrumented
	// one. Nil uses the default transport.
	Transport http.RoundTripper
}

// New validates its inputs and returns a Client. It rejects a non-http(s) URL,
// a missing host, a URL containing userinfo, an empty model, and a non-positive
// timeout. It does not rely on upstream config for validation.
func New(cfg Config) (*Client, error) {
	if cfg.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidConfig)
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("%w: timeout must be greater than zero", ErrInvalidConfig)
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid URL", ErrInvalidConfig)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: URL scheme must be http or https", ErrInvalidConfig)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: URL host is required", ErrInvalidConfig)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: URL userinfo is not allowed", ErrInvalidConfig)
	}
	return &Client{
		endpoint: cfg.URL,
		model:    cfg.Model,
		apiKey:   cfg.APIKey,
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: cfg.Transport,
			// Redirects are not part of this proxy contract. Returning
			// ErrUseLastResponse makes the client return the first redirect
			// response without following it, so the protected request body is
			// never forwarded to a different destination and the non-2xx
			// handling returns the fixed safe ErrUnavailable.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// chatRequest is the outbound chat-completions request body.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

// chatMessage is one message in the outbound chat request.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the parsed chat-completions response body.
type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

// chatChoice is one choice in the response.
type chatChoice struct {
	Message chatMessage `json:"message"`
}

// Complete sends the protected text to the downstream LLM and returns the
// model's modified protected text. It applies the configured context/timeout,
// requires HTTP(S), and rejects non-2xx, malformed/trailing JSON, empty/missing
// content, and an oversized response. On any failure it returns a fixed safe
// sentinel error that never embeds request text, protected text, tokens, the
// API key, a response body or upstream error detail.
func (c *Client) Complete(ctx context.Context, protected string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemInstruction},
			{Role: "user", Content: protected},
		},
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode request", ErrInvalidConfig)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: build request", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", wrapTransport(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}

	// Read at most maxResponseBytes+1 bytes so an oversized body is detected by
	// byte count before decoding. A body larger than maxResponseBytes is
	// rejected outright; the bounded bytes are then decoded and trailing JSON is
	// rejected.
	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("%w: read response", ErrInvalidResponse)
	}
	if len(responseBody) > maxResponseBytes {
		return "", fmt.Errorf("%w: response too large", ErrInvalidResponse)
	}

	dec := json.NewDecoder(bytes.NewReader(responseBody))
	var raw chatResponse
	if err := dec.Decode(&raw); err != nil {
		return "", fmt.Errorf("%w: decode response", ErrInvalidResponse)
	}
	if err := ensureEOF(dec); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if len(raw.Choices) == 0 {
		return "", fmt.Errorf("%w: missing choices", ErrInvalidResponse)
	}
	content := raw.Choices[0].Message.Content
	if content == "" {
		return "", fmt.Errorf("%w: empty content", ErrInvalidResponse)
	}
	return content, nil
}

// ensureEOF rejects trailing JSON after the top-level object. It never returns
// the raw decoder error so response body content is not exposed.
func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return errors.New("invalid trailing data")
	}
	return errors.New("trailing data after response")
}

// transportError wraps a transport error so it matches ErrUnavailable while
// preserving context identity (context.Canceled / DeadlineExceeded). Its
// Error() text is fixed and safe: it never embeds the underlying error message,
// which could carry request/response body, Authorization, protected text,
// tokens or the API key. errors.Is still classifies both the sentinel and the
// original cause.
type transportError struct {
	cause error
}

func (e *transportError) Error() string {
	return ErrUnavailable.Error()
}

func (e *transportError) Unwrap() error {
	return e.cause
}

func (e *transportError) Is(target error) bool {
	return target == ErrUnavailable
}

// wrapTransport wraps a transport error so it matches ErrUnavailable while
// preserving context identity. The returned error has a fixed safe Error() text
// that never embeds the underlying message, while errors.Is still classifies
// the original cause.
func wrapTransport(err error) error {
	return &transportError{cause: err}
}
