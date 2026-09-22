package modelclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeTokenizerCount mirrors the Python fake tokenizer semantics used in the
// worker tests: each word contributes 1 + ceil(len/2) tokens, where len is the
// word length in Unicode code points. It is distinct from both len(text) and
// the word count, so a test can prove the client trusts the tokenizer's count
// rather than counting characters or words.
func fakeTokenizerCount(text string) int {
	if text == "" {
		return 0
	}
	total := 0
	for _, word := range strings.Fields(text) {
		total += 1 + (utf8.RuneCountInString(word)+1)/2
	}
	return total
}

// countServer returns a server that answers POST /count_tokens with the fake
// tokenizer count for the request text and the exact primary model id.
func countServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := jsonDecode(r, &body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"model":%q,"count":%d}`, rubertModelID, fakeTokenizerCount(body.Text))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCountTokensUsesTokenizerNotCharOrWordCount(t *testing.T) {
	srv := countServer(t)
	c := newTestClient(t, srv, ModeFull)

	text := "Иван Петров"
	got, err := c.CountTokens(context.Background(), text)
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	want := fakeTokenizerCount(text)
	if got != want {
		t.Fatalf("CountTokens() = %d, want %d", got, want)
	}
	if got == len(text) {
		t.Errorf("CountTokens() = %d equals character count, tokenizer not used", got)
	}
	if got == len(strings.Fields(text)) {
		t.Errorf("CountTokens() = %d equals word count, tokenizer not used", got)
	}
}

func TestCountTokensEmptyTextIsZero(t *testing.T) {
	srv := countServer(t)
	c := newTestClient(t, srv, ModeFull)

	got, err := c.CountTokens(context.Background(), "")
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if got != 0 {
		t.Fatalf("CountTokens() = %d, want 0", got)
	}
	if err := CheckTokenLimit(got); err != nil {
		t.Fatalf("CheckTokenLimit(0) error = %v, want nil", err)
	}
}

func TestCheckTokenLimitBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		count int
		want  error
	}{
		{"below limit", 99_999, nil},
		{"exactly limit", 100_000, nil},
		{"above limit", 100_001, ErrTokenLimitExceeded},
		{"far above limit", 200_000, ErrTokenLimitExceeded},
		{"negative count", -1, ErrInvalidResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckTokenLimit(tt.count)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("CheckTokenLimit(%d) error = %v, want nil", tt.count, err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("CheckTokenLimit(%d) error = %v, want %v", tt.count, err, tt.want)
			}
		})
	}
}

func TestCountTokensStructuralFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"invalid json", `{not json`},
		{"trailing json", `{"model":"` + rubertModelID + `","count":5} extra`},
		{"missing count", `{"model":"` + rubertModelID + `"}`},
		{"missing model", `{"count":5}`},
		{"wrong model", `{"model":"other/model","count":5}`},
		{"non-string model", `{"model":123,"count":5}`},
		{"unknown top level field", `{"model":"` + rubertModelID + `","count":5,"extra":1}`},
		{"wrong type count", `{"model":"` + rubertModelID + `","count":"5"}`},
		{"negative count", `{"model":"` + rubertModelID + `","count":-1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv, ModeFull).CountTokens(context.Background(), "Анна")
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("CountTokens() error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestCountTokensCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newTestClient(t, srv, ModeFull).CountTokens(ctx, "Анна")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("CountTokens() error = %v, want ErrModelUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("CountTokens() error = %v, want context.Canceled", err)
	}
}

func TestCountTokensTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	c, err := New(srv.URL, ModeFull, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = c.CountTokens(context.Background(), "Анна")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("CountTokens() error = %v, want ErrModelUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("CountTokens() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestCountTokensNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv, ModeFull).CountTokens(context.Background(), "Анна")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("CountTokens() error = %v, want ErrModelUnavailable", err)
	}
}

func TestCountTokensConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c, err := New(url, ModeFull, time.Second)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = c.CountTokens(context.Background(), "Анна")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("CountTokens() error = %v, want ErrModelUnavailable", err)
	}
}

func TestCountTokensInvalidUTF8NoRequest(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv, ModeFull).CountTokens(context.Background(), string([]byte{0xff, 0xfe}))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CountTokens() error = %v, want ErrInvalidInput", err)
	}
	if hit {
		t.Error("invalid UTF-8 input made an HTTP request")
	}
}

func TestCountTokensErrorsDoNotLeakInputOrBody(t *testing.T) {
	const inputMarker = "COUNT_INPUT_MARKER_12345"
	const bodyMarker = "COUNT_BODY_MARKER_67890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"`+rubertModelID+`","count":5,"`+bodyMarker+`":1}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv, ModeFull).CountTokens(context.Background(), inputMarker)
	if err == nil {
		t.Fatal("CountTokens() expected error")
	}
	if strings.Contains(err.Error(), inputMarker) {
		t.Errorf("error leaks input marker: %q", err.Error())
	}
	if strings.Contains(err.Error(), bodyMarker) {
		t.Errorf("error leaks body marker: %q", err.Error())
	}
}

func TestCountTokensBasePathReachesCountEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"base path no trailing slash", "http://example.com/worker", "/worker/count_tokens"},
		{"base path trailing slash", "http://example.com/worker/", "/worker/count_tokens"},
		{"root no trailing slash", "http://example.com", "/count_tokens"},
		{"root trailing slash", "http://example.com/", "/count_tokens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				fmt.Fprint(w, `{"model":"`+rubertModelID+`","count":0}`)
			}))
			defer srv.Close()

			u, err := url.Parse(tt.baseURL)
			if err != nil {
				t.Fatalf("url.Parse(%q) error = %v", tt.baseURL, err)
			}
			u.Host = srv.Listener.Addr().String()
			c, err := New(u.String(), ModeFull, 5*time.Second)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			if _, err := c.CountTokens(context.Background(), "Анна"); err != nil {
				t.Fatalf("CountTokens() error = %v", err)
			}
			if gotPath != tt.want {
				t.Errorf("request path = %q, want %q", gotPath, tt.want)
			}
		})
	}
}
