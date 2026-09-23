package llmclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient builds a Client pointed at srv with a fixed model and no API
// key.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{URL: srv.URL, Model: "test-model", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

// decodeRequest decodes the outbound chat request body captured by a handler.
func decodeRequest(t *testing.T, r *http.Request) chatRequest {
	t.Helper()
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty model", Config{URL: "https://example.com", Timeout: time.Second}},
		{"non-http scheme", Config{URL: "ftp://example.com", Model: "m", Timeout: time.Second}},
		{"missing host", Config{URL: "https://", Model: "m", Timeout: time.Second}},
		{"malformed url", Config{URL: "not a url", Model: "m", Timeout: time.Second}},
		{"zero timeout", Config{URL: "https://example.com", Model: "m"}},
		{"userinfo", Config{URL: "https://user:pass@example.com", Model: "m", Timeout: time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("New() expected error")
			}
		})
	}
}

// TestNewRejectsUserinfoWithoutEchoingURL proves that a URL containing userinfo
// is rejected and that the error does not echo the raw URL (which could carry
// credentials).
func TestNewRejectsUserinfoWithoutEchoingURL(t *testing.T) {
	raw := "https://user:secret@example.com/v1/chat/completions"
	_, err := New(Config{URL: raw, Model: "m", Timeout: time.Second})
	if err == nil {
		t.Fatal("New() expected error for userinfo URL")
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error echoes raw URL or credentials: %q", err.Error())
	}
}

func TestCompleteSendsModelSystemAndUser(t *testing.T) {
	var got chatRequest
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		got = decodeRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"modified <EMAIL_00000000000000000000000000000000>"}}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	out, err := c.Complete(context.Background(), "<EMAIL_00000000000000000000000000000000>")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if out != "modified <EMAIL_00000000000000000000000000000000>" {
		t.Errorf("out = %q", out)
	}
	if got.Model != "test-model" {
		t.Errorf("model = %q, want test-model", got.Model)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	if got.Messages[0].Role != "system" || !strings.Contains(got.Messages[0].Content, "<EMAIL_") {
		t.Errorf("system message = %+v, want token-preserving instruction", got.Messages[0])
	}
	if got.Messages[1].Role != "user" || got.Messages[1].Content != "<EMAIL_00000000000000000000000000000000>" {
		t.Errorf("user message = %+v, want protected text", got.Messages[1])
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty when no API key", gotAuth)
	}
}

func TestCompleteSendsBearerWhenKeySet(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c, err := New(Config{URL: srv.URL, Model: "m", APIKey: "secret-key", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := c.Complete(context.Background(), "protected"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want Bearer secret-key", gotAuth)
	}
}

func TestCompleteRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "upstream error detail that must not leak")
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), "upstream error detail") {
		t.Errorf("error leaks upstream detail: %q", err.Error())
	}
}

func TestCompleteRejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{not json`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestCompleteRejectsTrailingJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]} trailing`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestCompleteRejectsMissingChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestCompleteRejectsEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestCompleteRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A valid JSON object whose content exceeds the bound.
		big := strings.Repeat("x", maxResponseBytes+1024)
		io.WriteString(w, `{"choices":[{"message":{"content":"`+big+`"}}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

// TestCompleteRejectsOversizedByWhitespace proves that a small valid JSON
// document followed by enough whitespace to exceed the byte limit is rejected
// by byte count, not accepted. This is a regression test for the defect where
// the limit was only enforced implicitly by decoder truncation.
func TestCompleteRejectsOversizedByWhitespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A small valid JSON document followed by whitespace that pushes the
		// total body past maxResponseBytes. The document itself is valid and
		// would decode cleanly if the limit were not enforced by byte count.
		pad := strings.Repeat(" ", maxResponseBytes+1024)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`+pad)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

func TestCompleteContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, "protected")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded reachable via errors.Is", err)
	}
}

func TestCompleteTransportErrorDoesNotLeakDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), "protected") {
		t.Errorf("error leaks protected text: %q", err.Error())
	}
}

// TestCompleteDoesNotFollowRedirect proves that a redirecting endpoint is not
// followed: the client returns the first redirect response (non-2xx) as
// ErrUnavailable and the redirect target receives zero requests. This is a
// regression test for the defect where the http.Client could forward the
// protected request body to a different destination.
func TestCompleteDoesNotFollowRedirect(t *testing.T) {
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	c := newTestClient(t, source)
	_, err := c.Complete(context.Background(), "protected")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if targetHits != 0 {
		t.Errorf("redirect target hits = %d, want 0", targetHits)
	}
}
