package modelclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"
)

// Window is one chunk of the original input. Text is the window's slice of the
// original input. Start and End are the window's global UTF-8 byte offsets into
// the original input, start inclusive and end exclusive. Entities holds the
// model candidates detected in this window; their offsets are local to the
// window on this step. Global offset reconstruction is a later task (8.2).
type Window struct {
	Text     string
	Start    int
	End      int
	Entities []Entity
}

// ErrInvalidWindowConfig matches invalid windowing parameters: a non-positive
// window size, a negative overlap, or an overlap not strictly smaller than the
// window size. It never carries input text or window content.
var ErrInvalidWindowConfig = errors.New("modelclient: invalid window config")

// WindowedInfer processes text in overlapping windows through the model client
// with bounded parallelism. It preserves the existing Infer contract for each
// window and returns one Window per chunk in deterministic window order,
// regardless of completion order.
//
// windowSize must be positive, overlap must be non-negative and strictly
// smaller than windowSize, and limit must be positive. Window boundaries never
// split a UTF-8 rune, the last window never duplicates a fully covered suffix,
// and the union of windows covers the whole text continuously without gaps
// (overlap makes duplicate byte coverage expected). A window may exceed the
// byte windowSize only when a single rune is itself longer than the limit.
// NER calls run with at most limit concurrent Infer calls. On cancellation no
// new work is started and no goroutine leaks. Any window error fails the whole
// call closed: no partial result is returned and window text or responses are
// never included in the error.
func (c *Client) WindowedInfer(ctx context.Context, text string, windowSize, overlap, limit int) ([]Window, error) {
	if windowSize <= 0 || overlap < 0 || overlap >= windowSize {
		return nil, fmt.Errorf("%w: windowSize=%d overlap=%d", ErrInvalidWindowConfig, windowSize, overlap)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit=%d", ErrInvalidWindowConfig, limit)
	}
	if !utf8.ValidString(text) {
		return nil, ErrInvalidInput
	}

	windows := splitWindows(text, windowSize, overlap)
	if len(windows) == 0 {
		return nil, nil
	}

	results := make([]Window, len(windows))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	permits := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, w := range windows {
		// Acquire a permit or stop starting new work on cancellation. The
		// acquired flag records whether this iteration holds a permit so we
		// only ever release a permit we actually acquired.
		acquired := false
		select {
		case permits <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		}
		if !acquired {
			// Cancelled while waiting for a permit: do not start new work.
			break
		}
		if ctx.Err() != nil {
			// The select may have taken the permit path even though ctx is
			// already done; release our own permit and stop.
			<-permits
			break
		}

		wg.Add(1)
		go func(i int, w Window) {
			defer wg.Done()
			defer func() { <-permits }()

			entities, err := c.Infer(ctx, w.Text)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				cancel()
				return
			}
			w.Entities = entities
			results[i] = w
		}(i, w)
	}

	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// splitWindows splits text into overlapping windows of at most windowSize
// bytes. Boundaries never split a UTF-8 rune. The last window never duplicates
// a fully covered suffix, and the union of windows covers the whole text.
func splitWindows(text string, windowSize, overlap int) []Window {
	if text == "" {
		return nil
	}
	var windows []Window
	start := 0
	for start < len(text) {
		end := start + windowSize
		if end > len(text) {
			end = len(text)
		}
		// Back off end to a rune boundary so we never split a rune.
		for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		// If the whole window fell inside one rune, extend to the rune end so
		// the window stays positive-sized and never splits the rune.
		if end == start {
			_, size := utf8.DecodeRuneInString(text[start:])
			end = start + size
		}
		windows = append(windows, Window{Text: text[start:end], Start: start, End: end})
		if end >= len(text) {
			break
		}
		// The next window starts overlap bytes before this window's end so
		// entities spanning the boundary are seen in both windows. Clamp to
		// end when overlap would move start backward (window smaller than
		// overlap), guaranteeing forward progress and full coverage.
		next := end - overlap
		if next <= start {
			next = end
		}
		// Align start forward to a rune boundary.
		for next < len(text) && !utf8.RuneStart(text[next]) {
			next++
		}
		start = next
	}
	return windows
}
