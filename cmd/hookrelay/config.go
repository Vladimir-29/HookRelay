package main

import (
	"fmt"
	"os"
	"strconv"
)

// config хранит адрес сервера и URL базы данных
type config struct {
	ServerPort  string
	DatabaseURL string
}

// loadConfig читает конфигурацию из env-переменных.
// Обязательна: DATABASE_URL. SERVER_PORT по умолчанию "8080"
func loadConfig() (config, error) {
	cfg := config{
		ServerPort:  getEnv("SERVER_PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
	}
	if cfg.DatabaseURL == "" {
		return config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if _, err := strconv.Atoi(cfg.ServerPort); err != nil {
		return config{}, fmt.Errorf("SERVER_PORT must be a number, got %q", cfg.ServerPort)
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
