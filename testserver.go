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
	// Адрес задаётся переменной окружения HTTP_ADDR.
	// Сейчас публикуемся напрямую через белый IPv4: 0.0.0.0:8088.
	defaultAddr = ":8088"

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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, envOr("HTTP_ADDR", defaultAddr)); err != nil {
		slog.Error("❌ сервер завершился с ошибкой", "error", err)
		os.Exit(1)
	}
	slog.Info("✅ сервер остановлен")
}

// run занимает порт и обслуживает его до отмены ctx.
// Вынесен из main, чтобы жизненный цикл сервера можно было тестировать.
func run(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", addr, err)
	}

	slog.Info("🚀 HTTP-сервер запущен", "addr", ln.Addr().String())
	return serve(ctx, newHTTPServer(NewRouter(true)), ln)
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
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
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
