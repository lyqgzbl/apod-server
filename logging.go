package main

import (
	"io"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func appEnv() string {
	env := strings.ToLower(strings.TrimSpace(getenv("APP_ENV", "development")))
	if env == "" {
		return "development"
	}
	return env
}

func isProdEnv() bool {
	switch appEnv() {
	case "prod", "production":
		return true
	default:
		return false
	}
}

func logEncoding() string {
	if isProdEnv() {
		return "json"
	}
	return "console"
}

func parseLogLevel(defaultLevel zapcore.Level) zap.AtomicLevel {
	raw := strings.TrimSpace(os.Getenv("LOG_LEVEL"))
	if raw == "" {
		return zap.NewAtomicLevelAt(defaultLevel)
	}
	var level zapcore.Level
	if err := level.Set(strings.ToLower(raw)); err != nil {
		return zap.NewAtomicLevelAt(defaultLevel)
	}
	return zap.NewAtomicLevelAt(level)
}

func newAppLogger() (*zap.Logger, error) {
	var cfg zap.Config
	if isProdEnv() {
		cfg = zap.NewProductionConfig()
		cfg.Encoding = "json"
		cfg.Level = parseLogLevel(zapcore.InfoLevel)
	} else {
		cfg = zap.NewDevelopmentConfig()
		cfg.Encoding = "console"
		cfg.Level = parseLogLevel(zapcore.DebugLevel)
		// Keep development logs readable: only panic/fatal should include long stacks.
		cfg.DisableStacktrace = true
	}

	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.LevelKey = "level"
	cfg.EncoderConfig.CallerKey = "caller"
	cfg.EncoderConfig.MessageKey = "msg"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.EncoderConfig.EncodeDuration = zapcore.StringDurationEncoder

	return cfg.Build()
}

func configureGinMode() string {
	// Always silence Gin's own startup/debug writers; access/recovery logs go through Zap.
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard

	mode := strings.TrimSpace(os.Getenv("GIN_MODE"))
	mode = strings.ToLower(mode)

	switch mode {
	case gin.DebugMode, gin.ReleaseMode, gin.TestMode:
		gin.SetMode(mode)
		return gin.Mode()
	case "":
		if isProdEnv() {
			gin.SetMode(gin.ReleaseMode)
		} else {
			gin.SetMode(gin.DebugMode)
		}
		return gin.Mode()
	default:
		if isProdEnv() {
			gin.SetMode(gin.ReleaseMode)
		} else {
			gin.SetMode(gin.DebugMode)
		}
		return gin.Mode()
	}
}
