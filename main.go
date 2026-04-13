package main

import "go.uber.org/zap"

func main() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()

	if envLoadErr != nil {
		logger.Warn("load .env failed", zap.Error(envLoadErr))
	}

	if err := runServer(); err != nil {
		logger.Fatal("server exited", zap.Error(err))
	}
}
