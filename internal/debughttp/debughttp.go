// Package debughttp writes raw public HTTP request and response bodies only
// when its explicitly configured debug middleware is installed.
package debughttp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FilePath is the fixed location for the sensitive debug journal.
const FilePath = "/var/log/pii-service/debug.jsonl"

// Logger serializes debug events to its writer.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

// Open opens path as a non-symlink append-only debug file with mode 0600.
// The parent directory must already exist and be managed by deployment.
func Open(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("debug log is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("inspect debug log")
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
		return nil, errors.New("debug log directory unavailable")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.New("open debug log")
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, errors.New("set debug log permissions")
	}
	info, err := f.Stat()
	if err != nil || info.Mode().Perm() != 0o600 {
		_ = f.Close()
		return nil, errors.New("verify debug log permissions")
	}
	return f, nil
}

// New returns a concurrent-safe JSONL logger.
func New(w io.Writer) *Logger { return &Logger{w: w} }

type event struct {
	Timestamp    time.Time `json:"timestamp"`
	Method       string    `json:"method"`
	Route        string    `json:"route"`
	Status       int       `json:"status"`
	RequestBody  string    `json:"request_body"`
	ResponseBody string    `json:"response_body"`
}

func (l *Logger) log(e event) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.Write(line)
	return err
}

// Middleware records one event for each allowlisted public data route. route
// must return only a fixed route template; an empty or unrecognized template
// skips logging.
func (l *Logger) Middleware(route func(*http.Request) string, writeError func()) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tmpl := route(r)
			if !dataRoute(r.Method, tmpl) {
				next.ServeHTTP(w, r)
				return
			}
			var request bytes.Buffer
			r.Body = &captureReadCloser{ReadCloser: r.Body, dst: &request}
			response := &captureWriter{ResponseWriter: w}
			defer func() {
				status := response.status
				if p := recover(); p != nil {
					status = http.StatusInternalServerError
					if err := l.log(event{time.Now().UTC(), r.Method, tmpl, status, request.String(), response.body.String()}); err != nil && writeError != nil {
						writeError()
					}
					panic(p)
				}
				if status == 0 {
					status = http.StatusOK
				}
				if err := l.log(event{time.Now().UTC(), r.Method, tmpl, status, request.String(), response.body.String()}); err != nil && writeError != nil {
					writeError()
				}
			}()
			next.ServeHTTP(response, r)
		})
	}
}

func dataRoute(method, route string) bool {
	if method == http.MethodPost {
		switch route {
		case "/process", "/v1/pii/detect", "/v1/pii/tokenize", "/v1/pii/detokenize", "/v1/runtime/chat":
			return true
		}
	}
	return method == http.MethodDelete && route == "/v1/pii/scopes/{scope_id}"
}

type captureReadCloser struct {
	io.ReadCloser
	dst *bytes.Buffer
}

func (r *captureReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	_, _ = r.dst.Write(p[:n])
	return n, err
}

type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *captureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	_, _ = w.body.Write(p[:n])
	return n, err
}

// Unwrap lets http.ResponseController reach the original response writer.
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *captureWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *captureWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(io.TeeReader(r, &w.body))
	}
	return io.Copy(w, r)
}
