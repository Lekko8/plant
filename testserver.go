package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/render"
)

const (
	// Адреса задаются переменными окружения HTTP_ADDR и HTTPS_ADDR.
	// Наружу смотрим через Cloudflare: 80 -> :8088, 443 -> :8443 (TLS).
	defaultAddr    = ":8088"
	defaultTLSAddr = ":8443"

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
	middlewareTimeout = 10 * time.Second
)

// NewRouter собирает роутер и middleware.
// accessLog выключаем в тестах, чтобы не засорять вывод go test.
func NewRouter(accessLog bool) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestIDHeader) // удобно искать запрос по X-Request-Id в логах и на клиенте
	if accessLog {
		r.Use(middleware.Logger)
	}
	r.Use(middleware.Recoverer)
	r.Use(middleware.URLFormat)
	r.Use(middleware.Timeout(middlewareTimeout))

	r.Route("/", func(r chi.Router) {
		getHandler(r, "/")
		getPingHandler(r, "/ping")
		getNumHandler(r, "/{num}")
	})
	return r
}

// requestIDHeader прокидывает id запроса из context в ответ.
func requestIDHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", middleware.GetReqID(r.Context()))
		next.ServeHTTP(w, r)
	})
}

// config — настройки сервера, собранные из окружения.
// Вынесены в структуру, чтобы жизненный цикл можно было тестировать без env.
type config struct {
	httpAddr  string
	httpsAddr string
	certFile  string
	keyFile   string
}

func loadConfig() config {
	return config{
		httpAddr:  envOr("HTTP_ADDR", defaultAddr),
		httpsAddr: envOr("HTTPS_ADDR", defaultTLSAddr),
		certFile:  os.Getenv("TLS_CERT_FILE"),
		keyFile:   os.Getenv("TLS_KEY_FILE"),
	}
}

// validate не даёт стартовать с половинчатой TLS-конфигурацией:
// сертификат без ключа бесполезен, и наоборот.
func (c config) validate() error {
	if (c.certFile == "") != (c.keyFile == "") {
		return errors.New("TLS_CERT_FILE и TLS_KEY_FILE должны быть заданы вместе")
	}
	return nil
}

// tlsEnabled сообщает, нужно ли поднимать дополнительный HTTPS-listener.
func (c config) tlsEnabled() bool {
	return c.certFile != "" && c.keyFile != ""
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, loadConfig()); err != nil {
		slog.Error("❌ сервер завершился с ошибкой", "error", err)
		os.Exit(1)
	}
	slog.Info("✅ сервер остановлен")
}

// run занимает порты и обслуживает их до отмены ctx.
// Вынесен из main, чтобы жизненный цикл сервера можно было тестировать.
func run(ctx context.Context, cfg config) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	httpLn, err := net.Listen("tcp", cfg.httpAddr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", cfg.httpAddr, err)
	}
	slog.Info("🚀 HTTP-сервер запущен", "addr", httpLn.Addr().String())

	if !cfg.tlsEnabled() {
		return serve(ctx, newHTTPServer(NewRouter(true)), httpLn)
	}

	tlsLn, err := net.Listen("tcp", cfg.httpsAddr)
	if err != nil {
		_ = httpLn.Close()
		return fmt.Errorf("listen %q: %w", cfg.httpsAddr, err)
	}
	slog.Info("🔒 HTTPS-сервер запущен", "addr", tlsLn.Addr().String())

	errCh := make(chan error, 2)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() { errCh <- serve(runCtx, newHTTPServer(NewRouter(true)), httpLn) }()
	go func() { errCh <- serveTLS(runCtx, newHTTPServer(NewRouter(true)), tlsLn, cfg.certFile, cfg.keyFile) }()

	// Если один listener упал (например, не читается сертификат),
	// гасим второй, иначе run() будет ждать его вечно.
	firstErr := <-errCh
	cancel()
	secondErr := <-errCh

	return errors.Join(firstErr, secondErr)
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout, // Защита от Slowloris атаки
		ReadTimeout:       readTimeout,       // Лимит на чтение всего запроса
		WriteTimeout:      writeTimeout,      // Лимит на запись ответа
		IdleTimeout:       idleTimeout,       // Таймаут keep-alive соединений
	}
}

// serve запускает сервер и корректно останавливает его при отмене ctx.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	return serveWith(ctx, srv, ln, func() error { return srv.Serve(ln) })
}

// serveTLS — то же самое, но listener обслуживается по TLS.
// Сертификат читается из файлов при старте, поэтому отсутствие файла
// проявится сразу, а не на первом запросе.
func serveTLS(ctx context.Context, srv *http.Server, ln net.Listener, certFile, keyFile string) error {
	return serveWith(ctx, srv, ln, func() error { return srv.ServeTLS(ln, certFile, keyFile) })
}

func serveWith(ctx context.Context, srv *http.Server, ln net.Listener, start func() error) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- start()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	slog.Info("🛑 завершение работы сервера...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getHandler(r chi.Router, pattern string) {
	r.Get(pattern, func(w http.ResponseWriter, r *http.Request) {
		render.JSON(w, r, "200 OK bro")
	})
}

func getPingHandler(r chi.Router, pattern string) {
	r.Get(pattern, func(w http.ResponseWriter, r *http.Request) {
		render.JSON(w, r, map[string]string{"status": "ok"})
	})
}

func getNumHandler(r chi.Router, pattern string) {
	r.Get(pattern, func(w http.ResponseWriter, r *http.Request) {
		num, err := strconv.Atoi(chi.URLParam(r, "num"))
		if err != nil {
			// render.Status только запоминает код, заголовок пишет следующий render.*
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, map[string]string{"error": "num должен быть целым числом"})
			return
		}
		render.JSON(w, r, fmt.Sprintf("Получен %d", num))
	})
}
