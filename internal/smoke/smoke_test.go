package smoke

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer returns an httptest server that serves the given handler.
func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// healthyHandler serves /health/live, /health/ready and a runtime chat that
// restores the synthetic values in the result.
func healthyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.HandleFunc("/v1/runtime/chat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(RuntimeResponse{Result: "Уважаемый клиент " + syntheticValues[0] + ", телефон " + syntheticValues[1] + ", email " + syntheticValues[2]})
	})
	return mux
}

func TestRunHTTPSuccess(t *testing.T) {
	srv := newTestServer(t, healthyHandler())
	client := NewClient(srv.URL, 5*time.Second, false)
	summary, err := client.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(summary, "live smoke ok") {
		t.Errorf("summary = %q, want success marker", summary)
	}
}

func TestRunTLSWithOptIn(t *testing.T) {
	srv := httptest.NewTLSServer(healthyHandler())
	t.Cleanup(srv.Close)

	// Without opt-in the self-signed certificate must fail closed.
	noOptIn := NewClient(srv.URL, 5*time.Second, false)
	if _, err := noOptIn.Run(context.Background()); err == nil {
		t.Fatal("Run() without opt-in succeeded, want fail-closed TLS error")
	}

	// With opt-in the self-signed certificate is accepted.
	withOptIn := NewClient(srv.URL, 5*time.Second, true)
	if _, err := withOptIn.Run(context.Background()); err != nil {
		t.Fatalf("Run() with opt-in error = %v", err)
	}
}

func TestRunNon2xxHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want non-2xx health error")
	}
}

func TestRunNon2xxRuntime(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/runtime/chat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want non-2xx runtime error")
	}
}

func TestRunMalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/runtime/chat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":`))
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want malformed JSON error")
	}
}

func TestRunTrailingJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/runtime/chat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"x"} {"extra":1}`))
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want trailing JSON error")
	}
}

func TestRunWrongContract(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/runtime/chat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":123}`))
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want wrong-contract error")
	}
}

func TestRunMissingRestoredValues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/runtime/chat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Only one of the three synthetic values is restored; the others are
		// missing, so the smoke must fail.
		_ = json.NewEncoder(w).Encode(RuntimeResponse{Result: "телефон " + syntheticValues[1]})
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want missing-restored-values error")
	}
}

func TestRunOversizedBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/runtime/chat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"` + strings.Repeat("x", maxBodyBytes+1024) + `"}`))
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want oversized-body error")
	}
}

func TestRunOversizedHealthBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", maxBodyBytes+1024)))
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want oversized health-body error")
	}
}

func TestRunOversizedTrailingWhitespace(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/runtime/chat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// A complete valid JSON value followed by more than maxBodyBytes of
		// trailing whitespace must be rejected by the raw size limit, not
		// accepted by a JSON decoder that skips trailing whitespace.
		_, _ = w.Write([]byte(`{"result":"ok"}` + strings.Repeat(" ", maxBodyBytes+1024)))
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want oversized trailing-whitespace error")
	}
}

func TestRunRedirectNotFollowed(t *testing.T) {
	var targetCalled bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	})
	srv := newTestServer(t, mux)
	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() succeeded, want non-2xx redirect error")
	}
	if targetCalled {
		t.Fatal("redirect target was called, want no follow-up request")
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "empty base URL", cfg: Config{BaseURL: "", Timeout: time.Second}, wantErr: true},
		{name: "bad scheme", cfg: Config{BaseURL: "ftp://x", Timeout: time.Second}, wantErr: true},
		{name: "no host", cfg: Config{BaseURL: "http://", Timeout: time.Second}, wantErr: true},
		{name: "zero timeout", cfg: Config{BaseURL: "http://x", Timeout: 0}, wantErr: true},
		{name: "self-signed on http", cfg: Config{BaseURL: "http://x", Timeout: time.Second, AllowSelfSigned: true}, wantErr: true},
		{name: "self-signed on https", cfg: Config{BaseURL: "https://x", Timeout: time.Second, AllowSelfSigned: true}, wantErr: false},
		{name: "valid http", cfg: Config{BaseURL: "http://x", Timeout: time.Second}, wantErr: false},
		{name: "valid https", cfg: Config{BaseURL: "https://x", Timeout: time.Second}, wantErr: false},
		{name: "userinfo", cfg: Config{BaseURL: "http://user:pass@x", Timeout: time.Second}, wantErr: true},
		{name: "query", cfg: Config{BaseURL: "http://x?q=1", Timeout: time.Second}, wantErr: true},
		{name: "fragment", cfg: Config{BaseURL: "http://x#frag", Timeout: time.Second}, wantErr: true},
		{name: "invalid parse", cfg: Config{BaseURL: "http://[::1", Timeout: time.Second}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestSelfSignedCertRejectedWithoutOptIn builds a real self-signed TLS server
// and proves the client fails closed without the opt-in flag.
func TestSelfSignedCertRejectedWithoutOptIn(t *testing.T) {
	cert, err := selfSignedCert()
	if err != nil {
		t.Fatalf("selfSignedCert() error = %v", err)
	}
	srv := httptest.NewUnstartedServer(healthyHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL, 5*time.Second, false)
	if _, err := client.Run(context.Background()); err == nil {
		t.Fatal("Run() without opt-in succeeded against self-signed cert, want fail-closed")
	}
}

// selfSignedCert returns a freshly generated self-signed certificate.
func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}
