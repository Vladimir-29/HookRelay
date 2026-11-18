package main

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

// validEncryptionKey — base64 от 32-байтного ключа для тестов.
func validEncryptionKey() string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("HOOKRELAY_ENCRYPTION_KEY", validEncryptionKey())
}

func TestLoadConfigDefaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AppMode != "api" {
		t.Fatalf("AppMode = %q, want api", cfg.AppMode)
	}
	if cfg.Secrets == nil {
		t.Fatal("Secrets box must be initialized")
	}
	if cfg.WorkerMaxRetries != 5 || cfg.WorkerMaxRetryDelay != 5*time.Minute ||
		cfg.WorkerRetryDelay != 5*time.Second {
		t.Fatalf("unexpected retry defaults: %+v", cfg)
	}
}

func TestLoadConfigEncryptionKey(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
		t.Setenv("HOOKRELAY_ENCRYPTION_KEY", "")
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected error for missing HOOKRELAY_ENCRYPTION_KEY")
		}
	})

	t.Run("not base64", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("HOOKRELAY_ENCRYPTION_KEY", "!!!not-base64!!!")
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected error for invalid base64")
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("HOOKRELAY_ENCRYPTION_KEY",
			base64.StdEncoding.EncodeToString([]byte("short")))
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected error for non-32-byte key")
		}
	})
}

func TestLoadConfigWorkerRetryValidation(t *testing.T) {
	base := func() {
		setRequiredEnv(t)
		t.Setenv("APP_MODE", "worker")
	}

	t.Run("negative retries", func(t *testing.T) {
		base()
		t.Setenv("WORKER_MAX_RETRIES", "-1")
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected error for negative WORKER_MAX_RETRIES")
		}
	})

	t.Run("max delay below base", func(t *testing.T) {
		base()
		t.Setenv("WORKER_RETRY_DELAY", "1m")
		t.Setenv("WORKER_MAX_RETRY_DELAY", "30s")
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected error for MAX_RETRY_DELAY < RETRY_DELAY")
		}
	})

	t.Run("invalid duration", func(t *testing.T) {
		base()
		t.Setenv("WORKER_MAX_RETRY_DELAY", "not-a-duration")
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected error for invalid duration")
		}
	})

	t.Run("valid custom policy", func(t *testing.T) {
		base()
		t.Setenv("WORKER_RETRY_DELAY", "2s")
		t.Setenv("WORKER_MAX_RETRY_DELAY", "1m")
		t.Setenv("WORKER_MAX_RETRIES", "3")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.WorkerMaxRetries != 3 || cfg.WorkerMaxRetryDelay != time.Minute {
			t.Fatalf("unexpected config: %+v", cfg)
		}
	})
}
