package main

import "go.uber.org/zap"

func main() {
	// Configure Gin mode/output at process startup to avoid noisy default startup logs.
	configureGinMode()

	var err error
	logger, err = newAppLogger()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()
	logger.Info("logger initialized", zap.String("app_env", appEnv()), zap.String("log_encoding", logEncoding()))

	if envLoadErr != nil {
		logger.Warn("load .env failed", zap.Error(envLoadErr))
	}

	if err := runServer(); err != nil {
		logger.Fatal("server exited", zap.Error(err))
	}
}
