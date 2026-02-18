package observability

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"langopen.dev/pkg/contracts"
)

type contextKey string

const requestIDKey contextKey = "request_id"

var (
	metricsOnce sync.Once
	reqTotal    *prometheus.CounterVec
	reqDuration *prometheus.HistogramVec
)

func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func CorrelationMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	initMetrics()
	serviceName := os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "langopen"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get(contracts.HeaderRequestID)
			if requestID == "" {
				requestID = uuid.NewString()
			}
			ctx := context.WithValue(r.Context(), requestIDKey, requestID)
			w.Header().Set(contracts.HeaderRequestID, requestID)
			started := time.Now()
			rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))
			latency := time.Since(started).Seconds()
			statusCode := strconv.Itoa(rec.statusCode)
			reqTotal.WithLabelValues(serviceName, r.Method, r.URL.Path, statusCode).Inc()
			reqDuration.WithLabelValues(serviceName, r.Method, r.URL.Path, statusCode).Observe(latency)
			logger.Info("http_request",
				"request_id", requestID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.statusCode,
				"latency_ms", time.Since(started).Milliseconds(),
			)
		})
	}
}

func MetricsHandler() http.Handler {
	initMetrics()
	return promhttp.Handler()
}

func initMetrics() {
	metricsOnce.Do(func() {
		reqTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "langopen_http_requests_total",
			Help: "Total HTTP requests handled by service and route.",
		}, []string{"service", "method", "path", "status"})
		reqDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "langopen_http_request_duration_seconds",
			Help:    "HTTP request duration by service and route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "method", "path", "status"})
		prometheus.MustRegister(reqTotal, reqDuration)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
