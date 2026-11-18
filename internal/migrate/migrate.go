// Package migrate применяет SQL-миграции к PostgreSQL.
//
// Гарантии:
//   - порядок — по имени файла (001_..., 002_..., ...);
//   - каждая миграция и запись в schema_migrations — одна транзакция:
//     при ошибке миграция не считается применённой;
//   - уже применённые файлы пропускаются — повторный запуск безопасен;
//   - pg_advisory_lock не допускает конкурентного применения
//     несколькими экземплярами.
//
// Пул соединений должен работать в simple query protocol
// (pgx.QueryExecModeSimpleProtocol): файлы миграций содержат
// несколько SQL-операторов.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey — произвольный ключ session-level advisory lock
// для сериализации миграций между экземплярами приложения.
const advisoryLockKey int64 = 727272001

// Apply применяет все неприменённые *.sql файлы из files (fs.FS,
// корень — каталог с миграциями) к базе пула pool.
// Возвращает имена вновь применённых миграций в порядке применения.
func Apply(ctx context.Context, pool *pgxpool.Pool, files fs.FS) ([]string, error) {
	names, err := migrationFiles(files)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no migration files found")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	// Блокирующий advisory lock: второй экземпляр дождётся первого
	// и увидит уже применённые миграции в schema_migrations.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return nil, err
	}

	var done []string
	for _, name := range names {
		if applied[name] {
			continue
		}
		sql, err := fs.ReadFile(files, name)
		if err != nil {
			return done, fmt.Errorf("read %s: %w", name, err)
		}
		if err := applyOne(ctx, conn, name, string(sql)); err != nil {
			return done, fmt.Errorf("migration %s: %w", name, err)
		}
		done = append(done, name)
	}
	return done, nil
}

// migrationFiles возвращает отсортированные имена *.sql файлов.
func migrationFiles(files fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// appliedMigrations читает множество уже применённых файлов.
func appliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `SELECT filename FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

// applyOne выполняет один файл миграции и запись в schema_migrations
// в одной транзакции.
func applyOne(ctx context.Context, conn *pgxpool.Conn, name, sql string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
