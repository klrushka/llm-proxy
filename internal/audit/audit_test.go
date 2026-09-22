package audit

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// allowedKeys is the exact allowlist of JSON keys the logger may emit. Any
// key outside this set is a privacy violation.
var allowedKeys = []string{
	"request_id",
	"operation",
	"has_personal_data",
	"detected_types",
	"entity_count",
	"personal_flags",
	"sources",
	"reason_codes",
	"duration_ms",
	"model_mode",
	"result",
}

// syntheticMarkers are plaintext values that must never appear in log output.
// They are the kind of values the detection pipeline would find but that the
// audit logger must never receive or emit. Each marker is a unique full
// plaintext fragment (name, email, card, CVV, PIN) that cannot accidentally
// collide with allowlisted metadata such as duration_ms or type names.
var syntheticMarkers = []string{
	"ТЕСТОВ ТЕСТ ТЕСТОВИЧ",
	"test@example.com",
	"1234 5678 9012 3456",
	"CVV: 739",
	"PIN: 8642",
}

func sampleEvent() Event {
	return Event{
		RequestID: "req-0001",
		Operation: OpTokenize,
		Entities: []Entity{
			{Type: "FULL_NAME", Personal: true, Sources: []string{"rubert"}, ReasonCodes: []string{"client_context", "linked_entities"}},
			{Type: "EMAIL", Personal: true, Sources: []string{"regex", "validator"}, ReasonCodes: []string{"field_label"}},
			{Type: "CARD_CVV", Personal: true, Sources: []string{"regex", "validator"}, ReasonCodes: []string{"positive_context"}},
			{Type: "CARD_PIN", Personal: true, Sources: []string{"regex", "validator"}, ReasonCodes: []string{"positive_context"}},
		},
		Duration:  time.Duration(1234) * time.Millisecond,
		ModelMode: ModeFull,
		Result:    ResultSuccess,
	}
}

func logOnce(t *testing.T, e Event) string {
	t.Helper()
	var buf bytes.Buffer
	l := New(&buf)
	if err := l.Log(e); err != nil {
		t.Fatalf("Log() error: %v", err)
	}
	return buf.String()
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, line)
	}
	return m
}

func TestLogContainsExpectedMetadata(t *testing.T) {
	line := logOnce(t, sampleEvent())
	m := decodeLine(t, line)

	if m["request_id"] != "req-0001" {
		t.Errorf("request_id = %v, want req-0001", m["request_id"])
	}
	if m["operation"] != "tokenize" {
		t.Errorf("operation = %v, want tokenize", m["operation"])
	}
	if m["has_personal_data"] != true {
		t.Errorf("has_personal_data = %v, want true", m["has_personal_data"])
	}
	if m["entity_count"] != float64(4) {
		t.Errorf("entity_count = %v, want 4", m["entity_count"])
	}
	if m["model_mode"] != "full" {
		t.Errorf("model_mode = %v, want full", m["model_mode"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
	if m["duration_ms"] != float64(1234) {
		t.Errorf("duration_ms = %v, want 1234", m["duration_ms"])
	}

	types := toStrings(t, m["detected_types"])
	wantTypes := []string{"CARD_CVV", "CARD_PIN", "EMAIL", "FULL_NAME"}
	if !reflect.DeepEqual(types, wantTypes) {
		t.Errorf("detected_types = %v, want %v", types, wantTypes)
	}

	sources := toStrings(t, m["sources"])
	wantSources := []string{"regex", "rubert", "validator"}
	if !reflect.DeepEqual(sources, wantSources) {
		t.Errorf("sources = %v, want %v", sources, wantSources)
	}

	reasons := toStrings(t, m["reason_codes"])
	wantReasons := []string{"client_context", "field_label", "linked_entities", "positive_context"}
	if !reflect.DeepEqual(reasons, wantReasons) {
		t.Errorf("reason_codes = %v, want %v", reasons, wantReasons)
	}

	flags := toBools(t, m["personal_flags"])
	if !reflect.DeepEqual(flags, []bool{true, true, true, true}) {
		t.Errorf("personal_flags = %v, want all true", flags)
	}
}

func TestLogHasPersonalDataFalseWithoutPersonalEntities(t *testing.T) {
	e := Event{
		RequestID: "req-0002",
		Operation: OpDetect,
		Entities: []Entity{
			{Type: "ADDRESS", Personal: false, Sources: []string{"gliner"}, ReasonCodes: []string{"organization_context"}},
		},
		Duration:  time.Duration(5) * time.Millisecond,
		ModelMode: ModeFast,
		Result:    ResultSuccess,
	}
	m := decodeLine(t, logOnce(t, e))
	if m["has_personal_data"] != false {
		t.Errorf("has_personal_data = %v, want false", m["has_personal_data"])
	}
	if flags := toBools(t, m["personal_flags"]); !reflect.DeepEqual(flags, []bool{false}) {
		t.Errorf("personal_flags = %v, want [false]", flags)
	}
}

func TestLogOrderIsDeterministic(t *testing.T) {
	e := sampleEvent()
	// Shuffle entity order and source/reason order to prove aggregation sorts.
	e.Entities = []Entity{
		{Type: "CARD_PIN", Personal: true, Sources: []string{"validator", "regex"}, ReasonCodes: []string{"positive_context"}},
		{Type: "FULL_NAME", Personal: true, Sources: []string{"rubert"}, ReasonCodes: []string{"linked_entities", "client_context"}},
		{Type: "EMAIL", Personal: true, Sources: []string{"validator", "regex"}, ReasonCodes: []string{"field_label"}},
		{Type: "CARD_CVV", Personal: true, Sources: []string{"regex", "validator"}, ReasonCodes: []string{"positive_context"}},
	}

	first := logOnce(t, e)
	for i := 0; i < 20; i++ {
		if got := logOnce(t, e); got != first {
			t.Fatalf("output not deterministic:\nfirst: %s\n got: %s", first, got)
		}
	}
}

func TestLogOmitsSyntheticPlaintextMarkers(t *testing.T) {
	// The logger receives only safe metadata (types, flags, sources, reasons).
	// The synthetic plaintext values are never passed to the API, so they must
	// not appear anywhere in the output.
	line := logOnce(t, sampleEvent())
	for _, marker := range syntheticMarkers {
		if strings.Contains(line, marker) {
			t.Errorf("log output contains synthetic plaintext marker %q:\n%s", marker, line)
		}
	}
}

func TestLogEmitsExactlyAllowedKeys(t *testing.T) {
	line := logOnce(t, sampleEvent())
	m := decodeLine(t, line)

	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	want := append([]string(nil), allowedKeys...)
	sort.Strings(got)
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("emitted keys = %v, want exactly %v", got, want)
	}
}

// TestEventAPIExposesNoPlaintextField proves the component API cannot carry
// plaintext: the exported Event struct has exactly the allowlisted metadata
// fields and no field for message, error, body, header, value, mapping,
// ciphertext or key.
func TestEventAPIExposesNoPlaintextField(t *testing.T) {
	typ := reflect.TypeOf(Event{})
	if typ.Kind() != reflect.Struct {
		t.Fatalf("Event is not a struct")
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		switch name {
		case "RequestID", "Operation", "Entities", "Duration", "ModelMode", "Result":
		default:
			t.Errorf("Event exposes unexpected field %q that could carry plaintext", name)
		}
	}
}

func TestLogConcurrentCallsProduceValidLines(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = l.Log(sampleEvent())
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d", len(lines), n)
	}
	for _, line := range lines {
		decodeLine(t, line)
	}
}

func toStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("value %v is not a JSON array", v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("array item %v is not a string", item)
		}
		out = append(out, s)
	}
	return out
}

func toBools(t *testing.T, v any) []bool {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("value %v is not a JSON array", v)
	}
	out := make([]bool, 0, len(raw))
	for _, item := range raw {
		b, ok := item.(bool)
		if !ok {
			t.Fatalf("array item %v is not a bool", item)
		}
		out = append(out, b)
	}
	return out
}
