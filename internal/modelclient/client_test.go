package modelclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// entityJSON builds a single entity object for a worker response.
func entityJSON(label string, start, end int, confidence float64, model string) string {
	return `{"label":` + strconv.Quote(label) +
		`,"start":` + strconv.Itoa(start) +
		`,"end":` + strconv.Itoa(end) +
		`,"confidence":` + strconv.FormatFloat(confidence, 'g', -1, 64) +
		`,"model":` + strconv.Quote(model) + `}`
}

// responseJSON wraps entity objects in a top-level entities array.
func responseJSON(entities ...string) string {
	return `{"entities":[` + strings.Join(entities, ",") + `]}`
}

// newTestClient returns a Client pointed at srv with a generous timeout.
func newTestClient(t *testing.T, srv *httptest.Server, mode Mode) *Client {
	t.Helper()
	c, err := New(srv.URL, mode, 5*time.Second)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

func TestInferCyrillicConversion(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		start int
		end   int
		wantS int
		wantE int
	}{
		{"cyrillic name", "Анна", 0, 4, 0, 8},
		{"cyrillic full name", "Анна Смирнова", 0, 13, 0, 25},
		{"ascii", "Ivan", 0, 4, 0, 4},
		{"mixed ascii cyrillic", "Иван Ivan", 0, 9, 0, 13},
		{"emoji middle rune", "A🙂Б", 1, 2, 1, 5},
		{"emoji full span", "A🙂Б", 0, 3, 0, 7},
		{"zero length span", "Анна", 2, 2, 4, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", tt.start, tt.end, 0.9, "rubert")))
			}))
			defer srv.Close()

			got, err := newTestClient(t, srv, ModeFull).Infer(context.Background(), tt.text)
			if err != nil {
				t.Fatalf("Infer() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("len(entities) = %d, want 1", len(got))
			}
			if got[0].Start != tt.wantS || got[0].End != tt.wantE {
				t.Errorf("Start/End = %d/%d, want %d/%d", got[0].Start, got[0].End, tt.wantS, tt.wantE)
			}
		})
	}
}

func TestInferDropsInvalidKeepsValidSibling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("FULL_NAME", 0, 4, 0.9, "rubert"), // valid
			entityJSON("BAD", -1, 4, 0.9, "rubert"),      // start < 0
			entityJSON("BAD", 5, 2, 0.9, "rubert"),       // end < start
			entityJSON("BAD", 0, 99, 0.9, "rubert"),      // end > runeCount
			entityJSON("", 0, 4, 0.9, "rubert"),          // empty label
			entityJSON("BAD", 0, 4, 1.5, "rubert"),       // confidence > 1
			entityJSON("BAD", 0, 4, -0.1, "rubert"),      // confidence < 0
			entityJSON("BAD", 0, 4, 0.9, "other"),        // unknown model
		))
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv, ModeFull).Infer(context.Background(), "Анна")
	if err != nil {
		t.Fatalf("Infer() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(entities) = %d, want 1", len(got))
	}
	if got[0].Label != "FULL_NAME" || got[0].Model != "rubert" {
		t.Errorf("kept entity = %+v, want FULL_NAME/rubert", got[0])
	}
}

func TestInferFullModePreservesBothSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("FULL_NAME", 0, 4, 0.9, "rubert"),
			entityJSON("ru_pii_person", 0, 4, 0.8, "gliner"),
		))
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv, ModeFull).Infer(context.Background(), "Анна")
	if err != nil {
		t.Fatalf("Infer() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(entities) = %d, want 2", len(got))
	}
	models := map[string]bool{}
	for _, e := range got {
		models[e.Model] = true
	}
	if !models["rubert"] || !models["gliner"] {
		t.Errorf("models = %v, want both rubert and gliner", models)
	}
}

func TestInferFastModeNoRequest(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv, ModeFast).Infer(context.Background(), "Анна")
	if err != nil {
		t.Fatalf("Infer() error = %v", err)
	}
	if got != nil {
		t.Errorf("entities = %v, want nil", got)
	}
	if hit {
		t.Error("fast mode made an HTTP request")
	}
}

func TestInferStructuralFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"invalid json", `{not json`},
		{"trailing json", `{"entities":[]} extra`},
		{"missing entities", `{}`},
		{"unknown top level field", `{"entities":[],"extra":1}`},
		{"entity missing field", `{"entities":[{"label":"X","start":0,"end":1,"confidence":0.9}]}`},
		{"entity unknown field", `{"entities":[{"label":"X","start":0,"end":1,"confidence":0.9,"model":"rubert","extra":1}]}`},
		{"wrong type start", `{"entities":[{"label":"X","start":"0","end":1,"confidence":0.9,"model":"rubert"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv, ModeFull).Infer(context.Background(), "Анна")
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Infer() error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestInferCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newTestClient(t, srv, ModeFull).Infer(ctx, "Анна")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("Infer() error = %v, want ErrModelUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Infer() error = %v, want context.Canceled", err)
	}
}

func TestInferTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	c, err := New(srv.URL, ModeFull, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = c.Infer(context.Background(), "Анна")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("Infer() error = %v, want ErrModelUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Infer() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestInferNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv, ModeFull).Infer(context.Background(), "Анна")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("Infer() error = %v, want ErrModelUnavailable", err)
	}
}

func TestInferConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c, err := New(url, ModeFull, time.Second)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = c.Infer(context.Background(), "Анна")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("Infer() error = %v, want ErrModelUnavailable", err)
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		mode    Mode
		timeout time.Duration
	}{
		{"unknown mode", "http://127.0.0.1:8000", Mode("bogus"), time.Second},
		{"zero timeout", "http://127.0.0.1:8000", ModeFull, 0},
		{"negative timeout", "http://127.0.0.1:8000", ModeFull, -time.Second},
		{"bad scheme", "ftp://example.com", ModeFull, time.Second},
		{"empty host", "http://", ModeFull, time.Second},
		{"unparseable", "not a url", ModeFull, time.Second},
		{"query rejected", "http://127.0.0.1:8000?x=1", ModeFull, time.Second},
		{"fragment rejected", "http://127.0.0.1:8000#frag", ModeFull, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.baseURL, tt.mode, tt.timeout); err == nil {
				t.Fatal("New() expected error")
			}
		})
	}
}

func TestInferBasePathReachesInferEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"base path no trailing slash", "http://example.com/worker", "/worker/infer"},
		{"base path trailing slash", "http://example.com/worker/", "/worker/infer"},
		{"root no trailing slash", "http://example.com", "/infer"},
		{"root trailing slash", "http://example.com/", "/infer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				fmt.Fprint(w, responseJSON())
			}))
			defer srv.Close()

			// Point the client at the test server but keep the base path from
			// the table by substituting the host.
			u, err := url.Parse(tt.baseURL)
			if err != nil {
				t.Fatalf("url.Parse(%q) error = %v", tt.baseURL, err)
			}
			u.Host = srv.Listener.Addr().String()
			c, err := New(u.String(), ModeFull, 5*time.Second)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			if _, err := c.Infer(context.Background(), "Анна"); err != nil {
				t.Fatalf("Infer() error = %v", err)
			}
			if gotPath != tt.want {
				t.Errorf("request path = %q, want %q", gotPath, tt.want)
			}
		})
	}
}

func TestInferInvalidUTF8NoRequest(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv, ModeFull).Infer(context.Background(), string([]byte{0xff, 0xfe}))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Infer() error = %v, want ErrInvalidInput", err)
	}
	if hit {
		t.Error("invalid UTF-8 input made an HTTP request")
	}
}

func TestErrorsDoNotLeakInputOrBody(t *testing.T) {
	const inputMarker = "INPUT_MARKER_12345"
	const bodyMarker = "BODY_MARKER_67890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"entities":[`+entityJSON("X", 0, 1, 0.9, "rubert")+`],"`+bodyMarker+`":1}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv, ModeFull).Infer(context.Background(), inputMarker)
	if err == nil {
		t.Fatal("Infer() expected error")
	}
	if strings.Contains(err.Error(), inputMarker) {
		t.Errorf("error leaks input marker: %q", err.Error())
	}
	if strings.Contains(err.Error(), bodyMarker) {
		t.Errorf("error leaks body marker: %q", err.Error())
	}
}
