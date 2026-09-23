package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"
)

// WindowPlan is the validated result of a POST /plan_windows call. TotalCount
// is the worker-reported total_count field, which the caller checks against the
// token limit before any inference. Windows holds the validated plan converted
// to UTF-8 byte offsets with Text slices of the original input.
type WindowPlan struct {
	TotalCount int
	Windows    []Window
}

// planWindowWire mirrors one window object in the /plan_windows response.
// Pointer-backed fields distinguish an absent field from a zero value so a
// missing required field is detected rather than confused with a zero value.
type planWindowWire struct {
	Start      *int `json:"start"`
	End        *int `json:"end"`
	TokenCount *int `json:"token_count"`
}

// planResponseWire mirrors the top-level /plan_windows response. Pointer-backed
// fields distinguish absent fields from zero values.
type planResponseWire struct {
	Model           *string           `json:"model"`
	TotalCount      *int              `json:"total_count"`
	MaxWindowTokens *int              `json:"max_window_tokens"`
	Windows         *[]planWindowWire `json:"windows"`
}

// PlanWindows requests a tokenizer-derived window plan for text from the
// worker's POST /plan_windows endpoint. The request body is strictly
// {"text":..., "overlap_tokens":...}. A negative overlap_tokens is rejected
// locally before any HTTP request. The response is decoded strictly and
// validated: unknown/trailing/missing/null fields, a wrong model, negative
// values, a non-positive max_window_tokens, a capacity not greater than the
// requested overlap, a non-positive or over-capacity token_count, offsets
// beyond the text, empty or reversed windows, non-progressing windows (start or
// end), gaps, missing overlap between adjacent windows when overlap_tokens > 0,
// and a plan that does not cover the whole non-empty text all fail closed with
// ErrInvalidResponse. For empty text only a consistent empty plan is accepted.
// For non-empty text, total_count 0 with no windows is the valid all-whitespace
// case; any other empty-plan mismatch is rejected.
//
// The worker reports Unicode code-point offsets; they are converted to UTF-8
// byte offsets and each Window.Text is a slice of the original input. Errors
// never carry input text or response bodies.
func (c *Client) PlanWindows(ctx context.Context, text string, overlapTokens int) (*WindowPlan, error) {
	if overlapTokens < 0 {
		return nil, fmt.Errorf("%w: overlap=%d", ErrInvalidWindowConfig, overlapTokens)
	}
	if !utf8.ValidString(text) {
		return nil, ErrInvalidInput
	}

	body, err := json.Marshal(map[string]any{"text": text, "overlap_tokens": overlapTokens})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrInvalidInput)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.planEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request", ErrModelUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")

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

	var raw planResponseWire
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: decode response", ErrInvalidResponse)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}

	return validatePlan(raw, text, overlapTokens)
}

// validatePlan checks the decoded plan response against the strict contract and
// converts code-point offsets to UTF-8 byte offsets. It returns a safe
// ErrInvalidResponse wrapper that never carries input text or response bodies.
func validatePlan(raw planResponseWire, text string, overlapTokens int) (*WindowPlan, error) {
	if raw.Model == nil {
		return nil, fmt.Errorf("%w: missing model", ErrInvalidResponse)
	}
	if *raw.Model != rubertModelID {
		return nil, fmt.Errorf("%w: wrong model", ErrInvalidResponse)
	}
	if raw.TotalCount == nil {
		return nil, fmt.Errorf("%w: missing total_count", ErrInvalidResponse)
	}
	if raw.MaxWindowTokens == nil {
		return nil, fmt.Errorf("%w: missing max_window_tokens", ErrInvalidResponse)
	}
	if raw.Windows == nil {
		return nil, fmt.Errorf("%w: missing windows", ErrInvalidResponse)
	}

	total := *raw.TotalCount
	if total < 0 {
		return nil, fmt.Errorf("%w: negative total_count", ErrInvalidResponse)
	}
	capacity := *raw.MaxWindowTokens
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: non-positive max_window_tokens", ErrInvalidResponse)
	}
	if capacity <= overlapTokens {
		return nil, fmt.Errorf("%w: capacity not greater than overlap", ErrInvalidResponse)
	}

	boundaries := runeBoundaries(text)
	runeCount := len(boundaries) - 1

	if text == "" {
		if len(*raw.Windows) != 0 {
			return nil, fmt.Errorf("%w: non-empty plan for empty text", ErrInvalidResponse)
		}
		if total != 0 {
			return nil, fmt.Errorf("%w: inconsistent total_count for empty text", ErrInvalidResponse)
		}
		return &WindowPlan{TotalCount: total}, nil
	}

	if len(*raw.Windows) == 0 {
		// Non-empty text with total_count 0 and no windows is the valid
		// all-whitespace case: the tokenizer produced no tokens.
		if total == 0 {
			return &WindowPlan{TotalCount: total}, nil
		}
		return nil, fmt.Errorf("%w: empty plan mismatch", ErrInvalidResponse)
	}

	if total == 0 {
		return nil, fmt.Errorf("%w: total_count zero with non-empty windows", ErrInvalidResponse)
	}

	windows := make([]Window, 0, len(*raw.Windows))
	var prevStart, prevEnd int
	for i, w := range *raw.Windows {
		if w.Start == nil || w.End == nil || w.TokenCount == nil {
			return nil, fmt.Errorf("%w: window missing required field", ErrInvalidResponse)
		}
		start, end, k := *w.Start, *w.End, *w.TokenCount
		if start < 0 || end < 0 {
			return nil, fmt.Errorf("%w: negative window offset", ErrInvalidResponse)
		}
		if end <= start {
			return nil, fmt.Errorf("%w: empty or reversed window", ErrInvalidResponse)
		}
		if end > runeCount {
			return nil, fmt.Errorf("%w: window offset beyond text", ErrInvalidResponse)
		}
		if k <= 0 {
			return nil, fmt.Errorf("%w: non-positive token_count", ErrInvalidResponse)
		}
		if k > capacity {
			return nil, fmt.Errorf("%w: token_count exceeds max_window_tokens", ErrInvalidResponse)
		}
		if i == 0 {
			if start != 0 {
				return nil, fmt.Errorf("%w: first window does not start at 0", ErrInvalidResponse)
			}
		} else {
			if start <= prevStart {
				return nil, fmt.Errorf("%w: non-progressing window", ErrInvalidResponse)
			}
			if end <= prevEnd {
				return nil, fmt.Errorf("%w: non-progressing window end", ErrInvalidResponse)
			}
			if overlapTokens > 0 {
				if start >= prevEnd {
					return nil, fmt.Errorf("%w: windows do not overlap", ErrInvalidResponse)
				}
			} else if start > prevEnd {
				return nil, fmt.Errorf("%w: gap between windows", ErrInvalidResponse)
			}
		}
		prevStart = start
		prevEnd = end
		windows = append(windows, Window{
			Text:  text[boundaries[start]:boundaries[end]],
			Start: boundaries[start],
			End:   boundaries[end],
		})
	}
	if prevEnd != runeCount {
		return nil, fmt.Errorf("%w: plan does not cover full text", ErrInvalidResponse)
	}
	return &WindowPlan{TotalCount: total, Windows: windows}, nil
}
