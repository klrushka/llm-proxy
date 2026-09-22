package loadbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewPoolDistinctRecords(t *testing.T) {
	p := NewPool(10)
	if len(p.records) != 10 {
		t.Fatalf("pool size = %d, want 10", len(p.records))
	}
	seen := make(map[string]bool)
	for _, r := range p.records {
		if r.ID == "" || r.Original == "" {
			t.Fatalf("record has empty id or original: %+v", r)
		}
		if seen[r.ID] {
			t.Errorf("duplicate payload_id %q", r.ID)
		}
		seen[r.ID] = true
		if !strings.Contains(r.Original, "@example.com") {
			t.Errorf("original %q missing synthetic email", r.Original)
		}
	}
}

func TestPoolNextDeterministicMix(t *testing.T) {
	p := NewPool(4)
	// Prewarm masks so Next can return expected results.
	for i := range p.records {
		p.records[i].Mask = "mask-" + p.records[i].ID
	}

	// First 8 calls: alternate mask path (send original, expect mask) and
	// restore path (send mask, expect original), cycling through the pool.
	want := []struct {
		payload string
		expect  string
	}{
		{p.records[0].Original, p.records[0].Mask},
		{p.records[0].Mask, p.records[0].Original},
		{p.records[1].Original, p.records[1].Mask},
		{p.records[1].Mask, p.records[1].Original},
		{p.records[2].Original, p.records[2].Mask},
		{p.records[2].Mask, p.records[2].Original},
		{p.records[3].Original, p.records[3].Mask},
		{p.records[3].Mask, p.records[3].Original},
	}
	for i, w := range want {
		req, expect := p.Next()
		if req.Payload != w.payload || expect != w.expect {
			t.Errorf("Next() #%d = (%q, %q), want (%q, %q)", i, req.Payload, expect, w.payload, w.expect)
		}
	}
}

func TestPoolPrewarm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := decodeJSON(r, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"masked-` + req.PayloadID + `"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	p := NewPool(3)
	if err := p.Prewarm(context.Background(), c); err != nil {
		t.Fatalf("Prewarm() error = %v", err)
	}
	for _, r := range p.records {
		if r.Mask != "masked-"+r.ID {
			t.Errorf("record %s mask = %q, want %q", r.ID, r.Mask, "masked-"+r.ID)
		}
	}
}

func TestPoolPrewarmFailsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	p := NewPool(2)
	if err := p.Prewarm(context.Background(), c); err == nil {
		t.Fatal("Prewarm() expected error on server failure")
	}
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
