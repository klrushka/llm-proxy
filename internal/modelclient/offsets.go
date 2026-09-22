package modelclient

import (
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// ErrInvalidWindow matches a structurally inconsistent window or entity during
// global offset reconstruction: negative offsets, End < Start, a window whose
// byte length does not match its global span, invalid UTF-8, a local offset
// outside the window, or a local offset not on a UTF-8 rune boundary. It never
// carries window text or entity values.
var ErrInvalidWindow = errors.New("modelclient: invalid window")

// ReconstructGlobalOffsets converts the local UTF-8 byte offsets of each
// window's entities into global UTF-8 byte offsets of the original input.
// Global offsets are computed relative to Window.Start: global = Window.Start +
// local. Label, Confidence and Model are preserved unchanged.
//
// The input windows and their entities are never mutated. Entities from
// overlapping windows are all preserved; semantic overlap resolution and merge
// are the responsibility of the existing merge layer, not this function.
//
// The result is returned in a deterministic global order independent of the
// input window order: ascending Start, then ascending End, then stable
// tie-breakers by Label, Model and Confidence.
//
// A nil or empty input returns a nil result with no error. Any inconsistent
// window or entity fails the whole call closed with ErrInvalidWindow; no
// partial result is returned and the error never contains window text or
// entity values.
func ReconstructGlobalOffsets(windows []Window) ([]Entity, error) {
	if len(windows) == 0 {
		return nil, nil
	}

	entities := make([]Entity, 0)
	for _, w := range windows {
		if err := validateWindow(w); err != nil {
			return nil, err
		}
		for _, e := range w.Entities {
			if err := validateLocalEntity(w, e); err != nil {
				return nil, err
			}
			entities = append(entities, Entity{
				Label:      e.Label,
				Start:      w.Start + e.Start,
				End:        w.Start + e.End,
				Confidence: e.Confidence,
				Model:      e.Model,
			})
		}
	}

	sort.SliceStable(entities, func(i, j int) bool {
		a, b := entities[i], entities[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.End != b.End {
			return a.End < b.End
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Confidence < b.Confidence
	})

	return entities, nil
}

// validateWindow checks the window's own structural consistency. It returns a
// safe ErrInvalidWindow wrapper that never carries window text.
func validateWindow(w Window) error {
	if w.Start < 0 {
		return fmt.Errorf("%w: negative window start", ErrInvalidWindow)
	}
	if w.End < w.Start {
		return fmt.Errorf("%w: window end before start", ErrInvalidWindow)
	}
	if w.End-w.Start != len(w.Text) {
		return fmt.Errorf("%w: window byte length mismatch", ErrInvalidWindow)
	}
	if !utf8.ValidString(w.Text) {
		return fmt.Errorf("%w: window text is not valid UTF-8", ErrInvalidWindow)
	}
	return nil
}

// validateLocalEntity checks that an entity's local offsets are inside the
// window and on UTF-8 rune boundaries. It returns a safe ErrInvalidWindow
// wrapper that never carries entity values.
func validateLocalEntity(w Window, e Entity) error {
	if e.Start < 0 || e.End < e.Start {
		return fmt.Errorf("%w: invalid entity offsets", ErrInvalidWindow)
	}
	if e.Start > len(w.Text) || e.End > len(w.Text) {
		return fmt.Errorf("%w: entity offset outside window", ErrInvalidWindow)
	}
	if !isRuneBoundary(w.Text, e.Start) || !isRuneBoundary(w.Text, e.End) {
		return fmt.Errorf("%w: entity offset not on rune boundary", ErrInvalidWindow)
	}
	return nil
}

// isRuneBoundary reports whether off is a valid UTF-8 rune boundary in text:
// the start of the string, the end of the string, or the start of a rune.
func isRuneBoundary(text string, off int) bool {
	if off == 0 || off == len(text) {
		return true
	}
	return utf8.RuneStart(text[off])
}
