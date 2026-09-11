package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const requestIDHeader = "X-Request-ID"

type requestIDKey struct{}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext возвращает id запроса для логов и трейсинга.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush сохраняет поддержку SSE через обертку.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap позволяет http.ResponseController достучаться до базового writer
// (например, чтобы снять deadline для SSE).
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func newRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(buf)
}

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.metrics.observe(r.Method, rec.status, time.Since(start).Seconds())
		slog.Info("http request",
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// httpMetrics - минимальный набор метрик без внешних зависимостей,
// экспортируется в формате Prometheus.
type httpMetrics struct {
	mu          sync.Mutex
	requests    map[string]int64 // ключ "METHOD|STATUS"
	durationSum float64
	durationCnt int64
}

func newHTTPMetrics() *httpMetrics {
	return &httpMetrics{requests: make(map[string]int64)}
}

func (m *httpMetrics) observe(method string, status int, seconds float64) {
	m.mu.Lock()
	m.requests[method+"|"+strconv.Itoa(status)]++
	m.durationSum += seconds
	m.durationCnt++
	m.mu.Unlock()
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.metrics.mu.Lock()
	lines := make([]string, 0, len(s.metrics.requests)+8)
	lines = append(lines,
		"# HELP metacrawler_http_requests_total Total HTTP requests.",
		"# TYPE metacrawler_http_requests_total counter",
	)
	for key, count := range s.metrics.requests {
		method, status, _ := splitMethodStatus(key)
		lines = append(lines, fmt.Sprintf("metacrawler_http_requests_total{method=%q,status=%q} %d", method, status, count))
	}
	durationSum := s.metrics.durationSum
	durationCnt := s.metrics.durationCnt
	s.metrics.mu.Unlock()

	lines = append(lines,
		"# HELP metacrawler_http_request_duration_seconds HTTP request latency.",
		"# TYPE metacrawler_http_request_duration_seconds summary",
		fmt.Sprintf("metacrawler_http_request_duration_seconds_sum %f", durationSum),
		fmt.Sprintf("metacrawler_http_request_duration_seconds_count %d", durationCnt),
	)

	status := s.workerMgr.GetStatus()
	running := 0
	if status.Status == "Running" {
		running = 1
	}
	lines = append(lines,
		"# HELP metacrawler_worker_running Whether the crawl cycle is running.",
		"# TYPE metacrawler_worker_running gauge",
		fmt.Sprintf("metacrawler_worker_running %d", running),
		"# HELP metacrawler_worker_last_processed Games processed in the last cycle.",
		"# TYPE metacrawler_worker_last_processed gauge",
		fmt.Sprintf("metacrawler_worker_last_processed %d", status.ProcessedCount),
	)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, line := range lines {
		_, _ = w.Write([]byte(line + "\n"))
	}
}

func splitMethodStatus(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:], true
		}
	}
	return key, "", false
}
