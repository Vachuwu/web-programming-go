// Package middleware provides HTTP concerns shared by all API handlers.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

type requestIDKey struct{}

// RequestID creates an opaque identifier for every request and exposes it to
// clients and structured logs. It deliberately contains no client data.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID)))
	})
}

// ID returns a request identifier added by RequestID.
func ID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

// Logging records only request metadata, never request bodies or headers.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			response := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(response, r)
			logger.Info("http request",
				slog.String("method", r.Method),
				slog.String("route", r.URL.Path),
				slog.Int("status", response.status),
				slog.Duration("duration", time.Since(started)),
				slog.String("request_id", ID(r.Context())),
			)
		})
	}
}

// Timeout limits regular API work while leaving SSE streams untouched.
func Timeout(limit time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/playlist/events" {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), limit)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	return w.ResponseWriter.Write(data)
}

// Flush preserves streaming support when the logging middleware wraps an SSE
// handler.
func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func newRequestID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err == nil {
		return hex.EncodeToString(bytes)
	}
	return "unavailable"
}
