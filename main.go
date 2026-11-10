package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := connectDB(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	server := newServer(cfg.ServerPort, pool)

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
			log.Fatalf("server: %v", err)
		}
	}

	// Должно быть меньше terminationGracePeriodSeconds пода (по умолчанию 30с),
	// иначе pod убьют по SIGKILL до окончания дренажа.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("graceful shutdown: %v", err)
	}
}
