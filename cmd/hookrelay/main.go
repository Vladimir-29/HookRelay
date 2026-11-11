package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"hookrelay/internal/httpapi"
	"hookrelay/internal/postgres"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer store.Close()

	server := httpapi.NewServer(cfg.ServerPort, store)

	// Буфер 1: main может уже уйти в shutdown по сигналу, и без буфера
	// горутина сервера навсегда зависнет на отправке в errCh.
	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on :%s", cfg.ServerPort)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Println("shutting down")
	case err := <-errCh:
		// ErrServerClosed — штатный результат Shutdown, а не авария.
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: %w", err)
		}
	}

	// Должно быть меньше terminationGracePeriodSeconds пода (по умолчанию 30с),
	// иначе pod убьют по SIGKILL до окончания дренажа.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
