package main

import (
	"fmt"

	"go.uber.org/zap"

	"apod-server/internal/app"
	"apod-server/internal/config"
	applog "apod-server/internal/log"
)

func main() {
	// Load .env FIRST so that APP_ENV, GIN_MODE, LOG_LEVEL etc. are available
	// for ConfigureGinMode and NewAppLogger.
	envLoadErr := config.LoadDotEnv()

	applog.ConfigureGinMode()

	logger, err := applog.NewAppLogger()
	if err != nil {
		panic(fmt.Sprintf("init logger: %v", err))
	}
	defer logger.Sync()
	logger.Info("logger initialized", zap.String("app_env", config.AppEnv()), zap.String("log_encoding", applog.LogEncoding()))

	if envLoadErr != nil {
		logger.Warn("load .env failed", zap.Error(envLoadErr))
	}

	srv, cronStop, err := app.NewApp(logger)
	if err != nil {
		logger.Fatal("init failed", zap.Error(err))
	}
	defer cronStop()

	if err := srv.Run(); err != nil {
		logger.Fatal("server exited", zap.Error(err))
	}
}
