package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"hookrelay/internal/domain"
)

// config хранит адрес сервера, URL базы данных и настройки режимов.
type config struct {
	ServerPort  string
	DatabaseURL string
	AppMode     string

	WorkerBatchSize     int
	WorkerPollInterval  time.Duration
	WorkerLease         time.Duration
	DeliveryHTTPTimeout time.Duration
	WorkerRetryDelay    time.Duration
	WorkerMaxRetryDelay time.Duration
	WorkerMaxRetries    int
}

// leaseSafetyMargin — запас lease над HTTP-таймаутом: пока Worker
// отправляет запрос, его lease не должен истечь.
const leaseSafetyMargin = 5 * time.Second

// loadConfig читает конфигурацию из env-переменных.
// Обязательна: DATABASE_URL. SERVER_PORT по умолчанию "8080",
// APP_MODE по умолчанию "api".
func loadConfig() (config, error) {
	cfg := config{
		ServerPort:  getEnv("SERVER_PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		AppMode:     getEnv("APP_MODE", "api"),
	}
	if cfg.DatabaseURL == "" {
		return config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if _, err := strconv.Atoi(cfg.ServerPort); err != nil {
		return config{}, fmt.Errorf("SERVER_PORT must be a number, got %q", cfg.ServerPort)
	}
	if cfg.AppMode != "api" && cfg.AppMode != "worker" {
		return config{}, fmt.Errorf("APP_MODE must be api or worker, got %q", cfg.AppMode)
	}

	var err error
	if cfg.WorkerBatchSize, err = envInt("WORKER_BATCH_SIZE", 10); err != nil {
		return config{}, err
	}
	if cfg.WorkerPollInterval, err = envDuration("WORKER_POLL_INTERVAL", time.Second); err != nil {
		return config{}, err
	}
	if cfg.WorkerLease, err = envDuration("WORKER_LEASE_DURATION", 30*time.Second); err != nil {
		return config{}, err
	}
	if cfg.DeliveryHTTPTimeout, err = envDuration("DELIVERY_HTTP_TIMEOUT", 10*time.Second); err != nil {
		return config{}, err
	}
	if cfg.WorkerRetryDelay, err = envDuration("WORKER_RETRY_DELAY", 5*time.Second); err != nil {
		return config{}, err
	}
	if cfg.WorkerMaxRetryDelay, err = envDuration("WORKER_MAX_RETRY_DELAY", 5*time.Minute); err != nil {
		return config{}, err
	}
	if cfg.WorkerMaxRetries, err = envInt("WORKER_MAX_RETRIES", 5); err != nil {
		return config{}, err
	}

	// Политика повторов валидна в любом режиме — дешёвая проверка.
	if err := (domain.RetryPolicy{
		BaseDelay:  cfg.WorkerRetryDelay,
		MaxDelay:   cfg.WorkerMaxRetryDelay,
		MaxRetries: cfg.WorkerMaxRetries,
	}).Validate(); err != nil {
		return config{}, err
	}

	if cfg.AppMode == "worker" && cfg.WorkerLease <= cfg.DeliveryHTTPTimeout+leaseSafetyMargin {
		return config{}, fmt.Errorf(
			"WORKER_LEASE_DURATION (%s) must exceed DELIVERY_HTTP_TIMEOUT (%s) + %s margin",
			cfg.WorkerLease, cfg.DeliveryHTTPTimeout, leaseSafetyMargin)
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration (e.g. 30s), got %q", key, v)
	}
	return d, nil
}
