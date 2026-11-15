package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRouter — табличные тесты роутера. Сеть не нужна:
// httptest.NewRecorder() подменяет ResponseWriter, а router.ServeHTTP — весь стек middleware.
func TestRouter(t *testing.T) {
	t.Parallel()

	router := NewRouter(false) // без access-лога

	tests := []struct {
		name            string
		method          string
		path            string
		wantStatus      int
		wantBody        string
		wantContentType string
	}{
		{
			name:            "root",
			method:          http.MethodGet,
			path:            "/",
			wantStatus:      http.StatusOK,
			wantBody:        `"200 OK bro"`,
			wantContentType: "application/json",
		},
		{
			name:            "ping",
			method:          http.MethodGet,
			path:            "/ping",
			wantStatus:      http.StatusOK,
			wantBody:        `"status":"ok"`,
			wantContentType: "application/json",
		},
		{
			name:            "num",
			method:          http.MethodGet,
			path:            "/42",
			wantStatus:      http.StatusOK,
			wantBody:        `"Получен 42"`,
			wantContentType: "application/json",
		},
		{
			name:            "num zero",
			method:          http.MethodGet,
			path:            "/0",
			wantStatus:      http.StatusOK,
			wantBody:        `"Получен 0"`,
			wantContentType: "application/json",
		},
		{
			name:            "num is not a number",
			method:          http.MethodGet,
			path:            "/abc",
			wantStatus:      http.StatusBadRequest,
			wantBody:        `"error"`,
			wantContentType: "application/json",
		},
		{
			name:       "num overflow",
			method:     http.MethodGet,
			path:       "/999999999999999999999",
			wantStatus: http.StatusBadRequest,
			wantBody:   `"error"`,
		},
		{
			// /{num} ловит любой односегментный путь, поэтому 404 — только для вложенного пути.
			name:       "unknown route",
			method:     http.MethodGet,
			path:       "/nope/nope",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "method not allowed",
			method:     http.MethodPost,
			path:       "/",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.wantStatus)
			}
			if tt.wantContentType != "" {
				if got := res.Header.Get("Content-Type"); got != tt.wantContentType {
					t.Errorf("Content-Type = %q, want %q", got, tt.wantContentType)
				}
			}
			if tt.wantBody != "" {
				body, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				if !strings.Contains(string(body), tt.wantBody) {
					t.Errorf("body = %q, want to contain %q", body, tt.wantBody)
				}
			}
		})
	}
}

func TestRouterRequestID(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	NewRouter(false).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))

	if got := rec.Header().Get("X-Request-Id"); got == "" {
		t.Error("X-Request-Id header is empty, middleware.RequestID is not wired")
	}
}

// TestServeGracefulShutdown проверяет реальный lifecycle:
// listener -> Serve -> отмена ctx -> Shutdown -> порт больше не принимает соединения.
func TestServeGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // :0 — свободный порт от ОС
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, newHTTPServer(NewRouter(false)), ln)
	}()

	// Без keep-alive, чтобы Shutdown не ждал простаивающие соединения клиента.
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   2 * time.Second,
	}

	baseURL := "http://" + ln.Addr().String()
	waitForServer(t, client, baseURL+"/ping")

	resp, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not return after context cancellation")
	}

	if conn, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond); err == nil {
		conn.Close()
		t.Error("listener still accepts connections after shutdown")
	}
}

func TestRunBadAddr(t *testing.T) {
	t.Parallel()

	if err := run(context.Background(), config{httpAddr: "127.0.0.1:not-a-port"}); err == nil {
		t.Fatal("run() = nil, want listen error")
	}
}

// TestRunTLSBadCert проверяет, что при нечитаемом сертификате run()
// не виснет на втором listener'е, а возвращает ошибку.
func TestRunTLSBadCert(t *testing.T) {
	t.Parallel()

	cfg := config{
		httpAddr:  freeAddr(t),
		httpsAddr: freeAddr(t),
		certFile:  filepath.Join(t.TempDir(), "missing-cert.pem"),
		keyFile:   filepath.Join(t.TempDir(), "missing-key.pem"),
	}

	done := make(chan error, 1)
	go func() { done <- run(context.Background(), cfg) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run() = nil, want TLS error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() не вернулся: второй listener не был остановлен")
	}
}

// TestRunServesHTTPAndHTTPS — интеграционный тест: оба listener'а реально
// принимают запросы, HTTPS отдаёт тот же роутер, что и HTTP.
func TestRunServesHTTPAndHTTPS(t *testing.T) {
	t.Parallel()

	certFile, keyFile := writeSelfSignedCert(t, t.TempDir())

	cfg := config{
		httpAddr:  freeAddr(t),
		httpsAddr: freeAddr(t),
		certFile:  certFile,
		keyFile:   keyFile,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()

	httpClient := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   2 * time.Second,
	}
	httpsClient := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			// Сертификат самоподписанный, проверять его в тесте нечем.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
		Timeout: 2 * time.Second,
	}

	waitForServer(t, httpClient, "http://"+cfg.httpAddr+"/ping")
	waitForServer(t, httpsClient, "https://"+cfg.httpsAddr+"/ping")

	resp, err := httpsClient.Get("https://" + cfg.httpsAddr + "/ping")
	if err != nil {
		t.Fatalf("GET https /ping: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if resp.TLS == nil {
		t.Error("соединение не по TLS: resp.TLS == nil")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not return after context cancellation")
	}
}

func TestLoadConfig(t *testing.T) {
	// t.Setenv несовместим с t.Parallel
	t.Run("defaults", func(t *testing.T) {
		for _, key := range []string{"HTTP_ADDR", "HTTPS_ADDR", "TLS_CERT_FILE", "TLS_KEY_FILE"} {
			t.Setenv(key, "")
		}

		cfg := loadConfig()
		if cfg.httpAddr != defaultAddr {
			t.Errorf("httpAddr = %q, want %q", cfg.httpAddr, defaultAddr)
		}
		if cfg.httpsAddr != defaultTLSAddr {
			t.Errorf("httpsAddr = %q, want %q", cfg.httpsAddr, defaultTLSAddr)
		}
		if cfg.tlsEnabled() {
			t.Error("tlsEnabled() = true без сертификата")
		}
	})

	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("HTTP_ADDR", ":9088")
		t.Setenv("HTTPS_ADDR", ":9443")
		t.Setenv("TLS_CERT_FILE", "/certs/cert.pem")
		t.Setenv("TLS_KEY_FILE", "/certs/key.pem")

		cfg := loadConfig()
		if cfg.httpAddr != ":9088" || cfg.httpsAddr != ":9443" {
			t.Errorf("адреса = %q, %q", cfg.httpAddr, cfg.httpsAddr)
		}
		if !cfg.tlsEnabled() {
			t.Error("tlsEnabled() = false, хотя сертификат и ключ заданы")
		}
	})
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     config
		wantErr bool
	}{
		{name: "оба пустые", cfg: config{}},
		{name: "оба заданы", cfg: config{certFile: "c.pem", keyFile: "k.pem"}},
		{name: "только сертификат", cfg: config{certFile: "c.pem"}, wantErr: true},
		{name: "только ключ", cfg: config{keyFile: "k.pem"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("PLANT_TEST_ADDR", ":9090") // t.Setenv нельзя сочетать с t.Parallel

	if got := envOr("PLANT_TEST_ADDR", ":8080"); got != ":9090" {
		t.Errorf("envOr(set) = %q, want %q", got, ":9090")
	}
	if got := envOr("PLANT_TEST_UNSET", ":8080"); got != ":8080" {
		t.Errorf("envOr(unset) = %q, want %q", got, ":8080")
	}
}

func waitForServer(t *testing.T, client *http.Client, url string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s is not ready", url)
}

// freeAddr занимает свободный порт у ОС и сразу освобождает его.
// Небольшая гонка с другими процессами здесь приемлема: тест локальный.
func freeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	return ln.Addr().String()
}

// writeSelfSignedCert создаёт пару cert/key для тестового HTTPS-listener'а.
func writeSelfSignedCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	return certFile, keyFile
}
