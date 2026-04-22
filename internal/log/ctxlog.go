package log

import (
	"context"

	"go.uber.org/zap"
)

type ctxLoggerKey struct{}

// WithLogger stores a logger in the context.
func WithLogger(ctx context.Context, l *zap.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxLoggerKey{}, l)
}

// LoggerFromCtx retrieves the logger from context. Returns zap.NewNop() if none found.
func LoggerFromCtx(ctx context.Context) *zap.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxLoggerKey{}).(*zap.Logger); ok && l != nil {
			return l
		}
	}
	return zap.NewNop()
}
