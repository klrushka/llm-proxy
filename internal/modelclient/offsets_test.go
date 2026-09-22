package modelclient

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestReconstructGlobalOffsetsCyrillic verifies global offset reconstruction
// for Cyrillic text where each rune is two bytes.
func TestReconstructGlobalOffsetsCyrillic(t *testing.T) {
	text := "Анна Смирнова"
	// "Анна" is 8 bytes, space is 1 byte, "Смирнова" is 16 bytes.
	windows := []Window{
		{Text: text, Start: 0, End: len(text), Entities: []Entity{
			{Label: "FULL_NAME", Start: 0, End: 8, Confidence: 0.9, Model: "rubert"},
		}},
	}
	got, err := ReconstructGlobalOffsets(windows)
	if err != nil {
		t.Fatalf("ReconstructGlobalOffsets() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(entities) = %d, want 1", len(got))
	}
	if got[0].Start != 0 || got[0].End != 8 {
		t.Errorf("Start/End = %d/%d, want 0/8", got[0].Start, got[0].End)
	}
	if got[0].Label != "FULL_NAME" || got[0].Confidence != 0.9 || got[0].Model != "rubert" {
		t.Errorf("entity = %+v, want preserved Label/Confidence/Model", got[0])
	}
}

// TestReconstructGlobalOffsetsEmoji verifies reconstruction when a window
// contains multi-byte emoji runes.
func TestReconstructGlobalOffsetsEmoji(t *testing.T) {
	text := "A🙂Б🙂В"
	// Byte layout: A(1) 🙂(4) Б(2) 🙂(4) В(2) = 13 bytes.
	windows := []Window{
		{Text: text, Start: 0, End: len(text), Entities: []Entity{
			{Label: "EMOJI", Start: 1, End: 5, Confidence: 0.8, Model: "gliner"},
		}},
	}
	got, err := ReconstructGlobalOffsets(windows)
	if err != nil {
		t.Fatalf("ReconstructGlobalOffsets() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(entities) = %d, want 1", len(got))
	}
	if got[0].Start != 1 || got[0].End != 5 {
		t.Errorf("Start/End = %d/%d, want 1/5", got[0].Start, got[0].End)
	}
}

// TestReconstructGlobalOffsetsSecondAndThirdWindow verifies that entities from
// the second and third windows get correct global offsets relative to their
// window Start.
func TestReconstructGlobalOffsetsSecondAndThirdWindow(t *testing.T) {
	// Three windows of the same text with different global starts.
	windows := []Window{
		{Text: "Анна", Start: 0, End: 8, Entities: []Entity{
			{Label: "FIRST_NAME", Start: 0, End: 8, Confidence: 0.9, Model: "rubert"},
		}},
		{Text: "Смирнова", Start: 9, End: 25, Entities: []Entity{
			{Label: "LAST_NAME", Start: 0, End: 16, Confidence: 0.85, Model: "rubert"},
		}},
		{Text: "Ивановна", Start: 26, End: 42, Entities: []Entity{
			{Label: "MIDDLE_NAME", Start: 0, End: 16, Confidence: 0.8, Model: "gliner"},
		}},
	}
	got, err := ReconstructGlobalOffsets(windows)
	if err != nil {
		t.Fatalf("ReconstructGlobalOffsets() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(entities) = %d, want 3", len(got))
	}
	want := []Entity{
		{Label: "FIRST_NAME", Start: 0, End: 8, Confidence: 0.9, Model: "rubert"},
		{Label: "LAST_NAME", Start: 9, End: 25, Confidence: 0.85, Model: "rubert"},
		{Label: "MIDDLE_NAME", Start: 26, End: 42, Confidence: 0.8, Model: "gliner"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entities = %+v, want %+v", got, want)
	}
}

// TestReconstructGlobalOffsetsOverlapPreservesBoth verifies that entities from
// overlapping windows are all preserved and not merged or dropped.
func TestReconstructGlobalOffsetsOverlapPreservesBoth(t *testing.T) {
	// Two windows overlapping on the same bytes, each reporting an entity.
	windows := []Window{
		{Text: "Анна Смирнова", Start: 0, End: 25, Entities: []Entity{
			{Label: "FULL_NAME", Start: 0, End: 25, Confidence: 0.9, Model: "rubert"},
		}},
		{Text: "Смирнова", Start: 9, End: 25, Entities: []Entity{
			{Label: "LAST_NAME", Start: 0, End: 16, Confidence: 0.7, Model: "gliner"},
		}},
	}
	got, err := ReconstructGlobalOffsets(windows)
	if err != nil {
		t.Fatalf("ReconstructGlobalOffsets() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(entities) = %d, want 2 (both preserved)", len(got))
	}
	// Both entities must be present with their own global offsets.
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Label] = true
	}
	if !seen["FULL_NAME"] || !seen["LAST_NAME"] {
		t.Errorf("entities = %+v, want both FULL_NAME and LAST_NAME preserved", got)
	}
}

// TestReconstructGlobalOffsetsDeterministicOrder verifies that the result order
// is deterministic regardless of the input window order.
func TestReconstructGlobalOffsetsDeterministicOrder(t *testing.T) {
	// Same entities in different window orders must produce identical output.
	build := func() []Window {
		return []Window{
			{Text: "Анна", Start: 0, End: 8, Entities: []Entity{
				{Label: "FIRST_NAME", Start: 0, End: 8, Confidence: 0.9, Model: "rubert"},
			}},
			{Text: "Смирнова", Start: 9, End: 25, Entities: []Entity{
				{Label: "LAST_NAME", Start: 0, End: 16, Confidence: 0.85, Model: "rubert"},
			}},
			{Text: "Ивановна", Start: 26, End: 42, Entities: []Entity{
				{Label: "MIDDLE_NAME", Start: 0, End: 16, Confidence: 0.8, Model: "gliner"},
			}},
		}
	}

	got1, err := ReconstructGlobalOffsets(build())
	if err != nil {
		t.Fatalf("ReconstructGlobalOffsets() error = %v", err)
	}
	// Shuffle the window order.
	shuffled := build()
	shuffled[0], shuffled[2] = shuffled[2], shuffled[0]
	got2, err := ReconstructGlobalOffsets(shuffled)
	if err != nil {
		t.Fatalf("ReconstructGlobalOffsets() shuffled error = %v", err)
	}
	if !reflect.DeepEqual(got1, got2) {
		t.Errorf("order differs by input order:\n got1 = %+v\n got2 = %+v", got1, got2)
	}
	// Result must be sorted by Start.
	for i := 1; i < len(got1); i++ {
		if got1[i].Start < got1[i-1].Start {
			t.Fatalf("result not sorted by Start: %+v", got1)
		}
	}
}

// TestReconstructGlobalOffsetsTieBreakers verifies deterministic ordering when
// Start and End are equal: Label, then Model, then Confidence.
func TestReconstructGlobalOffsetsTieBreakers(t *testing.T) {
	windows := []Window{
		{Text: "Анна", Start: 0, End: 8, Entities: []Entity{
			{Label: "B", Start: 0, End: 8, Confidence: 0.5, Model: "gliner"},
			{Label: "A", Start: 0, End: 8, Confidence: 0.9, Model: "rubert"},
			{Label: "A", Start: 0, End: 8, Confidence: 0.7, Model: "rubert"},
		}},
	}
	got, err := ReconstructGlobalOffsets(windows)
	if err != nil {
		t.Fatalf("ReconstructGlobalOffsets() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(entities) = %d, want 3", len(got))
	}
	// Expected: A/rubert/0.7, A/rubert/0.9, B/gliner/0.5.
	want := []Entity{
		{Label: "A", Start: 0, End: 8, Confidence: 0.7, Model: "rubert"},
		{Label: "A", Start: 0, End: 8, Confidence: 0.9, Model: "rubert"},
		{Label: "B", Start: 0, End: 8, Confidence: 0.5, Model: "gliner"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entities = %+v, want %+v", got, want)
	}
}

// TestReconstructGlobalOffsetsNoMutation verifies that the input windows and
// entities are not mutated.
func TestReconstructGlobalOffsetsNoMutation(t *testing.T) {
	windows := []Window{
		{Text: "Анна", Start: 0, End: 8, Entities: []Entity{
			{Label: "FIRST_NAME", Start: 0, End: 8, Confidence: 0.9, Model: "rubert"},
		}},
		{Text: "Смирнова", Start: 9, End: 25, Entities: []Entity{
			{Label: "LAST_NAME", Start: 0, End: 16, Confidence: 0.85, Model: "rubert"},
		}},
	}
	before := make([]Window, len(windows))
	copy(before, windows)
	for i := range windows {
		before[i].Entities = append([]Entity(nil), windows[i].Entities...)
	}

	if _, err := ReconstructGlobalOffsets(windows); err != nil {
		t.Fatalf("ReconstructGlobalOffsets() error = %v", err)
	}
	if !reflect.DeepEqual(windows, before) {
		t.Errorf("input windows mutated:\n got  = %+v\n want = %+v", windows, before)
	}
}

// TestReconstructGlobalOffsetsNilAndEmpty verifies that nil and empty input
// return an empty result without error.
func TestReconstructGlobalOffsetsNilAndEmpty(t *testing.T) {
	for _, in := range [][]Window{nil, {}} {
		got, err := ReconstructGlobalOffsets(in)
		if err != nil {
			t.Fatalf("ReconstructGlobalOffsets() error = %v", err)
		}
		if got != nil {
			t.Errorf("entities = %v, want nil", got)
		}
	}
}

// TestReconstructGlobalOffsetsInvalidCases verifies fail-closed behavior for
// every inconsistent window or entity case. Each window uses a marker text so
// the test also proves the error never leaks window text.
func TestReconstructGlobalOffsetsInvalidCases(t *testing.T) {
	const marker = "WINDOW_MARKER_12345"
	markerLen := len(marker)
	tests := []struct {
		name    string
		windows []Window
	}{
		{"negative window start", []Window{{Text: marker, Start: -1, End: markerLen - 1}}},
		{"window end before start", []Window{{Text: marker, Start: markerLen, End: 0}}},
		{"window byte length mismatch", []Window{{Text: marker, Start: 0, End: markerLen - 1}}},
		{"window invalid utf8", []Window{{Text: string([]byte{0xff, 0xfe}), Start: 0, End: 2}}},
		{"entity negative start", []Window{{Text: marker, Start: 0, End: markerLen, Entities: []Entity{{Start: -1, End: 2}}}}},
		{"entity end before start", []Window{{Text: marker, Start: 0, End: markerLen, Entities: []Entity{{Start: 4, End: 2}}}}},
		{"entity start outside window", []Window{{Text: marker, Start: 0, End: markerLen, Entities: []Entity{{Start: markerLen + 1, End: markerLen + 2}}}}},
		{"entity end outside window", []Window{{Text: marker, Start: 0, End: markerLen, Entities: []Entity{{Start: 0, End: markerLen + 1}}}}},
		{"entity start not rune boundary", []Window{{Text: "А" + marker, Start: 0, End: 2 + markerLen, Entities: []Entity{{Start: 1, End: 2 + markerLen}}}}},
		{"entity end not rune boundary", []Window{{Text: "А" + marker, Start: 0, End: 2 + markerLen, Entities: []Entity{{Start: 0, End: 1}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReconstructGlobalOffsets(tt.windows)
			if err == nil {
				t.Fatal("ReconstructGlobalOffsets() expected error")
			}
			if got != nil {
				t.Fatalf("entities = %v, want nil on failure (fail closed)", got)
			}
			if !errors.Is(err, ErrInvalidWindow) {
				t.Fatalf("error = %v, want ErrInvalidWindow", err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error leaks window text: %q", err.Error())
			}
		})
	}
}
