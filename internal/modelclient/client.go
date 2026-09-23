// Package modelclient is the Go client for the Python model worker's
// POST /infer endpoint. It owns transport, timeout, cancellation, strict
// response decoding and offset normalization. Orchestration and privacy
// decisions stay in Go; this package only turns worker spans into safe
// Entity values with UTF-8 byte offsets.
package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"
	"unicode/utf8"
)

// Mode selects how the client obtains model candidates.
type Mode string

// Model modes. These exact wire values mirror config.ModelMode.
const (
	ModeFull Mode = "full"
	ModeFast Mode = "fast"
)

// Known model sources returned by the worker. The worker contract enum is
// exactly these two values.
const (
	modelRubert = "rubert"
	modelGliner = "gliner"
)

// globalInferLimit is the fixed cap on concurrent expensive worker POSTs across all
// requests sharing one Client instance. It is a safety backpressure bound
// independent of any per-request concurrency limit.
const globalInferLimit = 4

// Package-local safe sentinels. They never carry input text, response bodies,
// or any plaintext. Later degraded-mode wiring maps them to process-level
// classifications.
var (
	// ErrModelUnavailable matches any transport-level failure: connection
	// errors, non-2xx responses, caller cancellation and timeouts. It wraps
	// the underlying error so context identity stays reachable via errors.Is.
	ErrModelUnavailable = errors.New("modelclient: model worker unavailable")
	// ErrInvalidResponse matches a structurally invalid worker response:
	// bad JSON, trailing JSON, unknown fields, missing required fields, or
	// wrong JSON types.
	ErrInvalidResponse = errors.New("modelclient: invalid worker response")
	// ErrInvalidInput matches locally rejected input (non-UTF-8 text) before
	// any HTTP request is made.
	ErrInvalidInput = errors.New("modelclient: invalid input")
)

// Entity is one detected span. Start and End are UTF-8 byte offsets into the
// original input, start inclusive and end exclusive.
type Entity struct {
	Label      string
	Start      int
	End        int
	Confidence float64
	Model      string
}

// Client calls the model worker's POST /infer, POST /count_tokens and
// POST /plan_windows endpoints.
type Client struct {
	endpoint      string
	countEndpoint string
	planEndpoint  string
	mode          Mode
	http          *http.Client
	timeout       time.Duration
	globalSem     chan struct{}
}

// Mode returns the immutable model mode selected when the client was built.
func (c *Client) Mode() Mode { return c.mode }

// Option configures a Client.
type Option func(*http.Client)

// WithTransport sets the HTTP transport used for worker calls, for example an
// instrumented one. A nil transport keeps the default.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *http.Client) { c.Transport = rt }
}

// Operation maps a worker request to its fixed operation name (infer,
// count_tokens or plan_windows) from the endpoint path; any other path maps
// to other. It never returns request data.
func Operation(r *http.Request) string {
	switch path.Base(r.URL.Path) {
	case "infer":
		return "infer"
	case "count_tokens":
		return "count_tokens"
	case "plan_windows":
		return "plan_windows"
	}
	return "other"
}

// New validates its inputs and returns a Client. It rejects an unknown mode,
// a non-positive timeout, and a base URL that is not an http(s) URL with a
// non-empty host. A base URL containing a query or fragment is rejected as
// invalid configuration. It does not rely on upstream config for validation.
func New(baseURL string, mode Mode, timeout time.Duration, opts ...Option) (*Client, error) {
	if mode != ModeFull && mode != ModeFast {
		return nil, fmt.Errorf("modelclient: unknown mode %q", mode)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("modelclient: timeout must be greater than zero")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("modelclient: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("modelclient: base URL scheme must be http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("modelclient: base URL host is required")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("modelclient: base URL must not contain a query or fragment")
	}
	endpoint, err := url.JoinPath(u.String(), "infer")
	if err != nil {
		return nil, fmt.Errorf("modelclient: resolve inference endpoint: %w", err)
	}
	countEndpoint, err := url.JoinPath(u.String(), "count_tokens")
	if err != nil {
		return nil, fmt.Errorf("modelclient: resolve count endpoint: %w", err)
	}
	planEndpoint, err := url.JoinPath(u.String(), "plan_windows")
	if err != nil {
		return nil, fmt.Errorf("modelclient: resolve plan endpoint: %w", err)
	}
	httpClient := &http.Client{Timeout: timeout}
	for _, opt := range opts {
		opt(httpClient)
	}
	return &Client{
		endpoint:      endpoint,
		countEndpoint: countEndpoint,
		planEndpoint:  planEndpoint,
		mode:          mode,
		http:          httpClient,
		timeout:       timeout,
		globalSem:     make(chan struct{}, globalInferLimit),
	}, nil
}

// Infer returns model candidates for text. In fast mode it makes no HTTP
// request and returns no candidates. In full mode it calls the worker and
// returns entities from both model sources with UTF-8 byte offsets.
func (c *Client) Infer(ctx context.Context, text string) ([]Entity, error) {
	if !utf8.ValidString(text) {
		return nil, ErrInvalidInput
	}
	if c.mode == ModeFast {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrInvalidInput)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrModelUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := c.acquireGlobal(ctx); err != nil {
		return nil, err
	}
	defer c.releaseGlobal()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrapTransport(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: status %d", ErrModelUnavailable, resp.StatusCode)
	}

	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()

	var raw struct {
		Entities *[]entityWire `json:"entities"`
	}
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: decode response", ErrInvalidResponse)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if raw.Entities == nil {
		return nil, fmt.Errorf("%w: missing entities", ErrInvalidResponse)
	}

	return normalize(*raw.Entities, text)
}

// entityWire mirrors one entity object in inference_response.schema.json.
// Pointer-backed fields distinguish absent fields from zero values so a
// missing required field is detected rather than confused with a zero value.
type entityWire struct {
	Label      *string  `json:"label"`
	Start      *int     `json:"start"`
	End        *int     `json:"end"`
	Confidence *float64 `json:"confidence"`
	Model      *string  `json:"model"`
}

// ensureEOF rejects trailing JSON after the top-level object. It never
// returns the raw decoder error so response body content is not exposed.
func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return errors.New("invalid trailing data")
	}
	return errors.New("trailing data after response")
}

// normalize validates each entity semantically and converts code-point
// offsets to UTF-8 byte offsets. A missing or null required field is a
// structural contract failure and fails the whole call with
// ErrInvalidResponse. Present-but-semantic-invalid values are dropped while
// valid siblings are preserved.
func normalize(wires []entityWire, text string) ([]Entity, error) {
	// Precompute one rune-index-to-byte-offset boundary table in O(len(text)).
	// boundaries[i] is the UTF-8 byte offset of rune index i; boundaries has
	// runeCount+1 entries so boundaries[runeCount] == len(text).
	boundaries := runeBoundaries(text)
	runeCount := len(boundaries) - 1

	entities := make([]Entity, 0, len(wires))
	for _, w := range wires {
		if w.Label == nil || w.Start == nil || w.End == nil || w.Confidence == nil || w.Model == nil {
			return nil, fmt.Errorf("%w: entity missing required field", ErrInvalidResponse)
		}
		if *w.Model != modelRubert && *w.Model != modelGliner {
			return nil, fmt.Errorf("%w: unknown model source", ErrInvalidResponse)
		}
		e, ok := validateEntity(w, boundaries, runeCount)
		if !ok {
			continue
		}
		entities = append(entities, e)
	}
	return entities, nil
}

// runeBoundaries returns the byte offset of each rune index, plus a final
// entry equal to len(text). It is O(len(text)). For empty text it returns
// [0]; for valid non-empty UTF-8 text it returns runeCount+1 entries.
func runeBoundaries(text string) []int {
	boundaries := make([]int, 0, utf8.RuneCountInString(text)+1)
	for i := range text {
		boundaries = append(boundaries, i)
	}
	return append(boundaries, len(text))
}

// validateEntity checks one wire entity and converts its code-point offsets
// to byte offsets. It returns ok=false for any semantic-invalid entity.
// Required fields are guaranteed non-nil by the caller.
func validateEntity(w entityWire, boundaries []int, runeCount int) (Entity, bool) {
	label := *w.Label
	start := *w.Start
	end := *w.End
	confidence := *w.Confidence
	model := *w.Model

	if label == "" {
		return Entity{}, false
	}
	if start < 0 || end < start || end > runeCount {
		return Entity{}, false
	}
	if confidence < 0 || confidence > 1 {
		return Entity{}, false
	}

	return Entity{
		Label:      label,
		Start:      boundaries[start],
		End:        boundaries[end],
		Confidence: confidence,
		Model:      model,
	}, true
}

// transportError wraps a transport error so it matches ErrModelUnavailable
// while preserving context identity (context.Canceled / DeadlineExceeded).
// Its Error() text is fixed and safe: it never embeds the underlying error
// message, which could carry request/response body, Authorization, ciphertext,
// keys, CVV or PIN. errors.Is still classifies both the sentinel and the
// original cause.
type transportError struct {
	cause error
}

func (e *transportError) Error() string {
	return ErrModelUnavailable.Error()
}

func (e *transportError) Unwrap() error {
	return e.cause
}

func (e *transportError) Is(target error) bool {
	return target == ErrModelUnavailable
}

// wrapTransport wraps a transport error so it matches ErrModelUnavailable
// while preserving context identity (context.Canceled / DeadlineExceeded).
// The returned error has a fixed safe Error() text that never embeds the
// underlying message, while errors.Is still classifies the original cause.
func wrapTransport(err error) error {
	return &transportError{cause: err}
}
