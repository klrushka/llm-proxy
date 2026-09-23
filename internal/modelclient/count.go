package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"
)

// MaxTokens is the maximum number of tokens accepted for a single input. It is
// the acceptance boundary for long-text windowing: inputs whose tokenizer count
// exceeds this limit are rejected.
const MaxTokens = 100_000

// rubertModelID is the exact primary model id the token-count contract is bound
// to. The worker must report this exact value in the /count_tokens response.
const rubertModelID = "redmadrobot-rnd/rubert-base-pii-ner"

// ErrTokenLimitExceeded is returned when a token count exceeds MaxTokens. It is
// a safe sentinel that never carries input text or response bodies.
var ErrTokenLimitExceeded = errors.New("modelclient: token limit exceeded")

// CheckTokenLimit reports whether count is within the accepted token limit. It
// returns nil for 0 <= count <= MaxTokens, ErrTokenLimitExceeded for
// count > MaxTokens, and ErrInvalidResponse for a negative count. It never
// carries input text.
func CheckTokenLimit(count int) error {
	if count < 0 {
		return fmt.Errorf("%w: negative token count", ErrInvalidResponse)
	}
	if count > MaxTokens {
		return ErrTokenLimitExceeded
	}
	return nil
}

// CountTokens returns the number of tokens the selected model's tokenizer
// produces for text, as counted by the Python worker's RuBERT tokenizer. It
// uses the same base URL as Infer but a separate /count_tokens endpoint. The
// count is bound to the model tokenizer, never to characters or words.
//
// The response is decoded strictly: unknown fields, trailing JSON, a missing
// model or count, a wrong or non-string model, a non-integer count, or a
// negative count all fail closed with ErrInvalidResponse. Transport failures,
// non-2xx responses, caller cancellation and timeouts match
// ErrModelUnavailable. Errors never carry input text or response bodies.
func (c *Client) CountTokens(ctx context.Context, text string) (int, error) {
	if !utf8.ValidString(text) {
		return 0, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return 0, fmt.Errorf("%w: encode request", ErrInvalidInput)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.countEndpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("%w: build request", ErrModelUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := c.acquireGlobal(ctx); err != nil {
		return 0, err
	}
	defer c.releaseGlobal()
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, wrapTransport(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("%w: status %d", ErrModelUnavailable, resp.StatusCode)
	}

	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()

	var raw struct {
		Model *string `json:"model"`
		Count *int    `json:"count"`
	}
	if err := dec.Decode(&raw); err != nil {
		return 0, fmt.Errorf("%w: decode response", ErrInvalidResponse)
	}
	if err := ensureEOF(dec); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if raw.Model == nil {
		return 0, fmt.Errorf("%w: missing model", ErrInvalidResponse)
	}
	if *raw.Model != rubertModelID {
		return 0, fmt.Errorf("%w: wrong model", ErrInvalidResponse)
	}
	if raw.Count == nil {
		return 0, fmt.Errorf("%w: missing count", ErrInvalidResponse)
	}
	if *raw.Count < 0 {
		return 0, fmt.Errorf("%w: negative count", ErrInvalidResponse)
	}
	return *raw.Count, nil
}
