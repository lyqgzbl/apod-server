package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"apod-server/internal/httputil"
	applog "apod-server/internal/log"
	"apod-server/internal/server/cron"
)

const demoAPIKey = "DEMO_KEY"

// --- Trace ID ---

func traceIDMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Trace-ID")
		if id == "" {
			id = uuid.NewString()
		}
		requestLog := logger.With(zap.String("trace_id", id))
		c.Set("request_logger", requestLog)
		c.Writer.Header().Set("X-Trace-ID", id)
		c.Request = c.Request.WithContext(applog.WithLogger(c.Request.Context(), requestLog))
		c.Next()
	}
}

// RequestLogger extracts the per-request logger from the gin context.
func RequestLogger(c *gin.Context) *zap.Logger {
	if v, ok := c.Get("request_logger"); ok {
		if l, ok := v.(*zap.Logger); ok && l != nil {
			return l
		}
	}
	return applog.LoggerFromCtx(c.Request.Context())
}

// --- Access Log ---

func latencyFieldForAccessLog(d time.Duration, encoding string) zap.Field {
	if encoding == "console" {
		ms := float64(d) / float64(time.Millisecond)
		return zap.String("latency", fmt.Sprintf("%8.3fms", ms))
	}
	return zap.Duration("latency", d)
}

func accessLogMiddleware(logEncoding string) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		method := c.Request.Method
		path := c.Request.URL.Path
		c.Next()
		status := c.Writer.Status()
		latency := time.Since(started)
		ip := httputil.RealIP(c.Request)
		fields := []zap.Field{
			zap.String("method", method),
			zap.String("path", path),
			zap.Int("status", status),
			latencyFieldForAccessLog(latency, logEncoding),
			zap.String("ip", ip),
		}
		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("errors", c.Errors.String()))
		}
		l := RequestLogger(c)
		switch {
		case status >= http.StatusInternalServerError:
			l.Error("http_request", fields...)
		case status >= http.StatusBadRequest:
			l.Warn("http_request", fields...)
		default:
			l.Info("http_request", fields...)
		}
	}
}

// --- Rate Limit ---

func rateLimitMiddleware(limiter *rate.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limiter != nil && !limiter.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{"code": 429, "msg": "too many requests"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// --- API Key Auth ---

func apiKeyAuthMiddleware(requiredKey string, demoLimiter *cron.DemoIPLimiter) gin.HandlerFunc {
	requiredBytes := []byte(requiredKey)
	return func(c *gin.Context) {
		l := RequestLogger(c)
		ip := httputil.RealIP(c.Request)
		provided, mode, msg := readAPIKeyFromHeader(c)
		if msg != "" {
			l.Warn("auth failed", zap.String("method", c.Request.Method), zap.String("ip", ip), zap.String("path", c.Request.URL.Path), zap.Int("status", http.StatusUnauthorized), zap.String("reason", msg))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": msg})
			c.Abort()
			return
		}

		if mode == "demo" {
			if demoLimiter != nil && !demoLimiter.Allow(ip) {
				l.Warn("demo key quota exceeded", zap.String("method", c.Request.Method), zap.String("ip", ip), zap.String("path", c.Request.URL.Path), zap.Int("status", http.StatusTooManyRequests))
				c.JSON(http.StatusTooManyRequests, gin.H{"code": 429, "msg": fmt.Sprintf("DEMO_KEY limit exceeded: %d requests per 24 hours for this IP", demoLimiter.Limit())})
				c.Abort()
				return
			}
			c.Next()
			if demoLimiter != nil && c.Writer.Status() != http.StatusOK {
				demoLimiter.Rollback(ip)
			}
			return
		}

		if subtle.ConstantTimeEq(int32(len(provided)), int32(len(requiredBytes))) != 1 || subtle.ConstantTimeCompare([]byte(provided), requiredBytes) != 1 {
			l.Warn("auth failed", zap.String("method", c.Request.Method), zap.String("ip", ip), zap.String("path", c.Request.URL.Path), zap.Int("status", http.StatusUnauthorized), zap.String("reason", "invalid API key"))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": "invalid API key"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func readAPIKeyFromHeader(c *gin.Context) (string, string, string) {
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return "", "", "Authorization must use Bearer token"
		}
		token := strings.TrimSpace(parts[1])
		if token == "" {
			return "", "", "Bearer token is required"
		}
		return token, "header", ""
	}

	apiKey := strings.TrimSpace(c.GetHeader("X-API-Key"))
	if apiKey != "" {
		return apiKey, "header", ""
	}

	return demoAPIKey, "demo", ""
}

// --- Recovery ---

// strictAuthMiddleware requires a valid Bearer token; DEMO_KEY fallback is not allowed.
// Used for sensitive endpoints like /metrics.
func strictAuthMiddleware(requiredKey string) gin.HandlerFunc {
	requiredBytes := []byte(requiredKey)
	return func(c *gin.Context) {
		l := RequestLogger(c)
		ip := httputil.RealIP(c.Request)
		provided, mode, msg := readAPIKeyFromHeader(c)
		if msg != "" {
			l.Warn("strict auth failed", zap.String("ip", ip), zap.String("path", c.Request.URL.Path), zap.String("reason", msg))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": msg})
			c.Abort()
			return
		}
		if mode == "demo" {
			l.Warn("strict auth rejected demo key", zap.String("ip", ip), zap.String("path", c.Request.URL.Path))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": "API key required"})
			c.Abort()
			return
		}
		if subtle.ConstantTimeEq(int32(len(provided)), int32(len(requiredBytes))) != 1 || subtle.ConstantTimeCompare([]byte(provided), requiredBytes) != 1 {
			l.Warn("strict auth failed", zap.String("ip", ip), zap.String("path", c.Request.URL.Path), zap.String("reason", "invalid API key"))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": "invalid API key"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func recoveryMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return ginzap.RecoveryWithZap(logger, true)
}
