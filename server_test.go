package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

	if err := run(context.Background(), "127.0.0.1:not-a-port"); err == nil {
		t.Fatal("run() = nil, want listen error")
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
