package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store — конкретное хранилище поверх пула соединений pgx.
type Store struct {
	pool *pgxpool.Pool
}

// Connect создаёт пул соединений к PostgreSQL и проверяет доступность БД
// через Ping. При ошибке пул закрывается. Вызывающий обязан
// закрыть успешно созданный Store через Close.
func Connect(ctx context.Context, url string) (*Store, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close освобождает все соединения пула.
func (s *Store) Close() {
	s.pool.Close()
}

// Ping проверяет доступность базы данных.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}
