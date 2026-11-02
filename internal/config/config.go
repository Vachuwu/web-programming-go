// Package config reads the application settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	HTTPAddress        string
	DatabaseURL        string
	LogLevel           string
	DBMaxConnections   int32
	HTTPTimeout        time.Duration
	DBOperationTimeout time.Duration
	DBConnectTimeout   time.Duration
	ShutdownTimeout    time.Duration
}

// Load reads .env when it exists. A real system environment variable always
// takes precedence over a value from that file.
func Load() (Config, error) {
	fileValues, err := godotenv.Read(".env")
	if errors.Is(err, os.ErrNotExist) {
		fileValues = map[string]string{}
		err = nil
	}
	if err != nil {
		return Config{}, err
	}

	port := value("SERVER_PORT", fileValues)
	if port == "" {
		// HTTP_ADDR is retained solely for compatibility with earlier stages.
		port = strings.TrimPrefix(value("HTTP_ADDR", fileValues), ":")
	}
	if port == "" {
		port = "8080"
	}
	maxConnections, err := positiveInt32("DB_MAX_CONNECTIONS", value("DB_MAX_CONNECTIONS", fileValues), 10)
	if err != nil {
		return Config{}, err
	}
	httpTimeout, err := duration("HTTP_TIMEOUT", value("HTTP_TIMEOUT", fileValues), 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	dbOperationTimeout, err := duration("DB_OPERATION_TIMEOUT", value("DB_OPERATION_TIMEOUT", fileValues), 3*time.Second)
	if err != nil {
		return Config{}, err
	}
	dbConnectTimeout, err := duration("DB_CONNECT_TIMEOUT", value("DB_CONNECT_TIMEOUT", fileValues), 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := duration("SHUTDOWN_TIMEOUT", value("SHUTDOWN_TIMEOUT", fileValues), 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		HTTPAddress:        ":" + port,
		DatabaseURL:        value("DATABASE_URL", fileValues),
		LogLevel:           value("LOG_LEVEL", fileValues),
		DBMaxConnections:   maxConnections,
		HTTPTimeout:        httpTimeout,
		DBOperationTimeout: dbOperationTimeout,
		DBConnectTimeout:   dbConnectTimeout,
		ShutdownTimeout:    shutdownTimeout,
	}
	if config.LogLevel == "" {
		config.LogLevel = "info"
	}
	if config.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	return config, nil
}

func value(key string, fileValues map[string]string) string {
	if systemValue, exists := os.LookupEnv(key); exists {
		return systemValue
	}
	return fileValues[key]
}

func positiveInt32(name, raw string, fallback int32) (int32, error) {
	if raw == "" {
		return fallback, nil
	}
	var value int32
	if _, err := fmt.Sscan(raw, &value); err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func duration(name, raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}
