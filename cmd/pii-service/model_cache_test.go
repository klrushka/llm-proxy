package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/detection"
)

func newSyntheticNERCache(t *testing.T) *nerCache {
	t.Helper()
	c, err := newNERCache("full|rubert|gliner|test-config")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNERCacheCoalescesAndCopiesOnlySpans(t *testing.T) {
	c := newSyntheticNERCache(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	compute := func(_ context.Context, _ string) ([]detection.Candidate, error) {
		calls.Add(1)
		close(entered)
		<-release
		return []detection.Candidate{{Type: detection.TypeEmail, Start: 0, End: 3, Sources: []detection.Source{detection.SourceRubert, detection.SourceGliner}}}, nil
	}
	var wg sync.WaitGroup
	results := make([][]detection.Candidate, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = c.detect(context.Background(), "synthetic text", compute, nil)
		}(i)
	}
	<-entered
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("NER calls = %d, want one", calls.Load())
	}
	results[0][0].Sources[0] = detection.Source("mutated")
	hit, err := c.detect(context.Background(), "synthetic text", compute, nil)
	if err != nil || hit[0].Sources[0] != detection.SourceRubert {
		t.Fatalf("cached spans mutated across callers: %v %v", hit, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cache hit reran NER: %d", calls.Load())
	}
}

func TestNERCacheDoesNotRetainErrorOrCanceledWaiter(t *testing.T) {
	c := newSyntheticNERCache(t)
	var calls atomic.Int32
	compute := func(_ context.Context, _ string) ([]detection.Candidate, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("synthetic worker failure")
		}
		return []detection.Candidate{{Type: detection.TypeEmail}}, nil
	}
	if _, err := c.detect(context.Background(), "synthetic", compute, nil); err == nil {
		t.Fatal("first failure was not returned")
	}
	if spans, err := c.detect(context.Background(), "synthetic", compute, nil); err != nil || len(spans) != 1 || calls.Load() != 2 {
		t.Fatalf("error was cached: spans=%v err=%v calls=%d", spans, err, calls.Load())
	}
	// A waiting caller can leave without affecting the owner or retaining its
	// context. The owner's result may still populate the cache.
	ownerEntered := make(chan struct{})
	ownerRelease := make(chan struct{})
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, _ = c.detect(context.Background(), "other synthetic", func(context.Context, string) ([]detection.Candidate, error) {
			close(ownerEntered)
			<-ownerRelease
			return nil, nil
		}, nil)
	}()
	<-ownerEntered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.detect(ctx, "other synthetic", compute, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}
	close(ownerRelease)
	<-ownerDone
}

func TestNERCacheTTLAndEntryLimit(t *testing.T) {
	c := newSyntheticNERCache(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	var calls atomic.Int32
	compute := func(context.Context, string) ([]detection.Candidate, error) {
		calls.Add(1)
		return nil, nil
	}
	_, _ = c.detect(context.Background(), "synthetic", compute, nil)
	_, _ = c.detect(context.Background(), "synthetic", compute, nil)
	if calls.Load() != 1 {
		t.Fatal("fresh entry missed")
	}
	now = now.Add(nerCacheTTL + time.Nanosecond)
	_, _ = c.detect(context.Background(), "synthetic", compute, nil)
	if calls.Load() != 2 {
		t.Fatal("expired entry was reused")
	}
	for i := 0; i < nerCacheMaxEntries+8; i++ {
		_, _ = c.detect(context.Background(), strings.Repeat("x", i+1), compute, nil)
	}
	if len(c.entries) > nerCacheMaxEntries || c.bytes > nerCacheMaxBytes {
		t.Fatalf("cache bounds exceeded: entries=%d bytes=%d", len(c.entries), c.bytes)
	}
}

func TestCachedNERStillTokenizesAndRestoresPerScope(t *testing.T) {
	const text = "Пишите test@example.com"
	start, end := runeSpanOf(t, text, "test@example.com")
	var inferCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inferCalls.Add(1)
		fmt.Fprint(w, responseJSON(
			entityJSON("EMAIL", start, end, 0.95, "rubert"),
			entityJSON("ru_pii_email", start, end, 0.95, "gliner"),
		))
	}))
	defer srv.Close()
	pipe := newPipelineWithModel(t, newModelDetector(t, srv), string(detection.TypeEmail))
	h := pipe.Handlers()
	a, err := h.Tokenize(context.Background(), api.TokenizeRequest{Text: text, ScopeID: "scope-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Tokenize(context.Background(), api.TokenizeRequest{Text: text, ScopeID: "scope-b"})
	if err != nil {
		t.Fatal(err)
	}
	if inferCalls.Load() != 1 || a.TokenizedText == text || b.TokenizedText == text || a.TokenizedText == b.TokenizedText {
		t.Fatalf("scoped tokenization/cache isolation failed: calls=%d", inferCalls.Load())
	}
	for _, tc := range []struct{ scope, masked string }{{"scope-a", a.TokenizedText}, {"scope-b", b.TokenizedText}} {
		got, err := h.Detokenize(context.Background(), api.DetokenizeRequest{Text: tc.masked, ScopeID: tc.scope, Mode: api.ModeStrict})
		if err != nil || got.RestoredText != text {
			t.Fatalf("restore for %s failed: %v", tc.scope, err)
		}
	}
}
