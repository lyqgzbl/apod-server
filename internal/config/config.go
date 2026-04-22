package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// LoadDotEnv loads .env file if present. Returns nil if file does not exist.
func LoadDotEnv() error {
	err := godotenv.Load()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Getenv returns the environment variable value or fallback.
func Getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// GetenvInt returns the environment variable value as int or fallback.
func GetenvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// GetenvFloat64 returns the environment variable value as float64 or fallback.
func GetenvFloat64(key string, fallback float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return n
}

// AppEnv returns the current application environment name.
func AppEnv() string {
	env := strings.ToLower(strings.TrimSpace(Getenv("APP_ENV", "development")))
	if env == "" {
		return "development"
	}
	return env
}

// IsProdEnv reports whether the current environment is production.
func IsProdEnv() bool {
	switch AppEnv() {
	case "prod", "production":
		return true
	default:
		return false
	}
}
