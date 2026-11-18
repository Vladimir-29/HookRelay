// hookrelay-migrate — одноразовый контейнер/команда применения
// SQL-миграций HookRelay к PostgreSQL.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"hookrelay/internal/migrate"
	"hookrelay/migrations"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	// Simple query protocol: файлы миграций содержат несколько
	// SQL-операторов в одном Exec.
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	poolCfg.MaxConns = 2

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	applied, err := migrate.Apply(ctx, pool, migrations.FS)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		log.Println("migrations: nothing to apply")
		return nil
	}
	for _, name := range applied {
		log.Printf("migration applied: %s", name)
	}
	return nil
}
