package graphcache

import (
	"context"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// contextKey is an unexported type for context keys in this package.
type contextKey string

const (
	// ContextKeyRequestID is the context key for request/operation IDs.
	contextKeyRequestID contextKey = "graph-cache-request-id"
)

// requestIDCounter is an atomic counter for generating unique request IDs.
var requestIDCounter atomic.Uint64

// newRequestID generates a unique request ID for correlating log entries
// across a single operation.
func newRequestID() uint64 {
	return requestIDCounter.Add(1)
}

// ContextWithRequestID returns a new context with a request ID attached.
func ContextWithRequestID(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKeyRequestID, newRequestID())
}

// RequestIDFromContext extracts the request ID from context, or 0 if not set.
func RequestIDFromContext(ctx context.Context) uint64 {
	if id, ok := ctx.Value(contextKeyRequestID).(uint64); ok {
		return id
	}
	return 0
}

// OperationLogger provides structured logging for graph cache operations.
// It automatically includes component, operation name, and request ID fields.
type OperationLogger struct {
	entry     *log.Entry
	operation string
	startTime time.Time
}

// NewOperationLogger creates a logger for a specific operation.
// All log entries include the component, operation, and request_id fields.
func NewOperationLogger(operation string) *OperationLogger {
	return &OperationLogger{
		entry: log.WithFields(log.Fields{
			"component":  "graph-cache",
			"operation":  operation,
			"request_id": newRequestID(),
		}),
		operation: operation,
		startTime: time.Now(),
	}
}

// NewOperationLoggerFromContext creates a logger using the request ID from context.
func NewOperationLoggerFromContext(ctx context.Context, operation string) *OperationLogger {
	reqID := RequestIDFromContext(ctx)
	if reqID == 0 {
		reqID = newRequestID()
	}

	return &OperationLogger{
		entry: log.WithFields(log.Fields{
			"component":  "graph-cache",
			"operation":  operation,
			"request_id": reqID,
		}),
		operation: operation,
		startTime: time.Now(),
	}
}

// WithField adds a field to the logger.
func (ol *OperationLogger) WithField(key string, value interface{}) *OperationLogger {
	ol.entry = ol.entry.WithField(key, value)
	return ol
}

// WithFields adds multiple fields to the logger.
func (ol *OperationLogger) WithFields(fields log.Fields) *OperationLogger {
	ol.entry = ol.entry.WithFields(fields)
	return ol
}

// WithError adds an error field to the logger.
func (ol *OperationLogger) WithError(err error) *OperationLogger {
	ol.entry = ol.entry.WithError(err)
	return ol
}

// Info logs at info level.
func (ol *OperationLogger) Info(msg string) {
	ol.entry.Info(msg)
}

// Warn logs at warn level.
func (ol *OperationLogger) Warn(msg string) {
	ol.entry.Warn(msg)
}

// Error logs at error level.
func (ol *OperationLogger) Error(msg string) {
	ol.entry.Error(msg)
}

// Debug logs at debug level.
func (ol *OperationLogger) Debug(msg string) {
	ol.entry.Debug(msg)
}

// Complete logs the operation completion with duration.
func (ol *OperationLogger) Complete(msg string) {
	ol.entry.WithField("duration_ms", time.Since(ol.startTime).Milliseconds()).Info(msg)
}

// CompleteWithError logs the operation failure with duration and error.
func (ol *OperationLogger) CompleteWithError(err error, msg string) {
	ol.entry.WithFields(log.Fields{
		"duration_ms": time.Since(ol.startTime).Milliseconds(),
	}).WithError(err).Error(msg)
}

// Duration returns the time elapsed since the operation started.
func (ol *OperationLogger) Duration() time.Duration {
	return time.Since(ol.startTime)
}
