package process

import (
	"encoding/json"
	"testing"
)

func TestRequestDecode(t *testing.T) {
	const body = `{"payload":"synthetic text","payload_id":"id-1"}`
	var req Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if req.Payload != "synthetic text" {
		t.Errorf("Payload = %q, want %q", req.Payload, "synthetic text")
	}
	if req.PayloadID != "id-1" {
		t.Errorf("PayloadID = %q, want %q", req.PayloadID, "id-1")
	}
}

func TestRequestRejectsNonStringField(t *testing.T) {
	const body = `{"payload":123,"payload_id":"id-1"}`
	var req Request
	if err := json.Unmarshal([]byte(body), &req); err == nil {
		t.Fatal("Unmarshal() expected error for non-string payload")
	}
}

func TestRequestAllowsExtraFields(t *testing.T) {
	const body = `{"payload":"synthetic text","payload_id":"id-1","extra":true}`
	var req Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if req.Payload != "synthetic text" {
		t.Errorf("Payload = %q, want %q", req.Payload, "synthetic text")
	}
	if req.PayloadID != "id-1" {
		t.Errorf("PayloadID = %q, want %q", req.PayloadID, "id-1")
	}
}

func TestResponseMarshalsOnlyResult(t *testing.T) {
	got, err := json.Marshal(Response{Result: "masked"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"result":"masked"}`
	if string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
}

func TestResponseHasNoOtherFields(t *testing.T) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"result":"masked"}`), &m); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(m) != 1 {
		t.Errorf("response has %d fields, want 1", len(m))
	}
	if _, ok := m["result"]; !ok {
		t.Error("response missing result field")
	}
}

func TestRecordStateWireValues(t *testing.T) {
	cases := []struct {
		state RecordState
		want  string
	}{
		{StateClaim, "claim"},
		{StateReady, "ready"},
		{StateExpired, "expired"},
	}
	for _, c := range cases {
		if string(c.state) != c.want {
			t.Errorf("state = %q, want %q", c.state, c.want)
		}
	}
}

func TestRecordStateValid(t *testing.T) {
	for _, s := range []RecordState{StateClaim, StateReady, StateExpired} {
		if !s.Valid() {
			t.Errorf("Valid() = false for %q", s)
		}
	}
}

func TestRecordStateRejectsInvalid(t *testing.T) {
	for _, s := range []RecordState{"", "RESTORED", "ready ", "Claim", "unknown"} {
		if s.Valid() {
			t.Errorf("Valid() = true for invalid state %q", s)
		}
	}
}
