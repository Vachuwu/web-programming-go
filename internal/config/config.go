// Package config reads the application settings from the environment.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	HTTPAddress string
	DatabaseURL string
}

// Load reads .env when it exists. A real system environment variable always
// takes precedence over a value from that file.
func Load() (Config, error) {
	fileValues, err := readDotEnv(".env")
	if err != nil {
		return Config{}, err
	}

	config := Config{
		HTTPAddress: value("HTTP_ADDR", fileValues),
		DatabaseURL: value("DATABASE_URL", fileValues),
	}
	if config.HTTPAddress == "" {
		config.HTTPAddress = ":8080"
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

func readDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, fmt.Errorf("invalid .env line %d", lineNumber)
		}
		values[key] = strings.Trim(strings.TrimSpace(value), "\"")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return values, nil
}
