package migrate

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"hookrelay/migrations"
)

// testSchemaPool создаёт отдельную schema и пул с search_path на неё —
// миграции применяются к ней, не трогая таблицы public.
func testSchemaPool(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	// Bootstrap-пул без search_path — создать schema.
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bootstrap pool: %v", err)
	}
	if _, err := boot.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema)); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := boot.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = boot.Exec(context.Background(),
			fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
		boot.Close()
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	// Файлы миграций содержат несколько операторов.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func schemaName(t *testing.T) string {
	return fmt.Sprintf("mig_test_%d", time.Now().UnixNano())
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestApplyRealMigrations: все 6 миграций применяются с нуля,
// повторный запуск ничего не делает.
func TestApplyRealMigrations(t *testing.T) {
	pool := testSchemaPool(t, schemaName(t))
	ctx := context.Background()

	applied, err := Apply(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 6 {
		t.Fatalf("applied %d migrations, want 6: %v", len(applied), applied)
	}
	// Порядок по имени файла.
	want := []string{
		"001_create_endpoints.sql",
		"002_create_endpoint_subscriptions.sql",
		"003_create_events.sql",
		"004_deliveries_processing_lease_index.sql",
		"005_create_delivery_attempts.sql",
		"006_endpoint_secret.sql",
	}
	for i, name := range want {
		if applied[i] != name {
			t.Fatalf("applied[%d] = %s, want %s", i, applied[i], name)
		}
	}

	// Таблица учёта заполнена.
	if n := countRows(t, pool,
		`SELECT count(*) FROM schema_migrations`); n != 6 {
		t.Fatalf("schema_migrations rows = %d, want 6", n)
	}
	// Миграция 006 реально применилась: колонка secret_encrypted есть.
	if n := countRows(t, pool, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'endpoints' AND column_name = 'secret_encrypted'`); n != 1 {
		t.Fatal("secret_encrypted column missing — migration 006 not applied")
	}

	// Повторный запуск — no-op.
	again, err := Apply(ctx, pool, migrations.FS)
	if err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("re-Apply applied %v, want none", again)
	}
}

// TestApplyError: ошибка в середине — применённые ранее остаются,
// упавшая и последующие не отмечаются.
func TestApplyError(t *testing.T) {
	pool := testSchemaPool(t, schemaName(t))
	fsys := fstest.MapFS{
		"001_ok.sql":    {Data: []byte(`CREATE TABLE t_ok (id int);`)},
		"002_bad.sql":   {Data: []byte(`THIS IS NOT VALID SQL;`)},
		"003_never.sql": {Data: []byte(`CREATE TABLE t_never (id int);`)},
	}

	_, err := Apply(context.Background(), pool, fsys)
	if err == nil {
		t.Fatal("expected error")
	}

	// 001 применилась и записана, 002/003 — нет.
	if n := countRows(t, pool, `SELECT count(*) FROM schema_migrations`); n != 1 {
		t.Fatalf("schema_migrations rows = %d, want 1", n)
	}
	var hasBad bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename='002_bad.sql')`).Scan(&hasBad); err != nil {
		t.Fatal(err)
	}
	if hasBad {
		t.Fatal("failed migration recorded as applied")
	}
	if n := countRows(t, pool, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = 't_never'`); n != 0 {
		t.Fatal("migration after the failed one must not run")
	}
}

// TestApplyOrder: миграции выполняются строго по имени файла —
// 002 требует результата 001, 003 — результат 002.
func TestApplyOrder(t *testing.T) {
	pool := testSchemaPool(t, schemaName(t))
	fsys := fstest.MapFS{
		"001_a.sql": {Data: []byte(`CREATE TABLE ord (n text);
			INSERT INTO ord VALUES ('a');`)},
		"002_b.sql": {Data: []byte(`INSERT INTO ord SELECT 'b'
			WHERE EXISTS (SELECT 1 FROM ord WHERE n = 'a');`)},
		"003_c.sql": {Data: []byte(`INSERT INTO ord SELECT 'c'
			WHERE (SELECT count(*) FROM ord) = 2;`)},
	}

	if _, err := Apply(context.Background(), pool, fsys); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := countRows(t, pool,
		`SELECT count(*) FROM ord WHERE n IN ('a','b','c')`); n != 3 {
		t.Fatalf("ord rows = %d, want 3 — order not respected", n)
	}
}

// TestApplyConcurrent: два параллельных Apply не применяют
// миграции дважды — advisory lock сериализует их.
func TestApplyConcurrent(t *testing.T) {
	pool := testSchemaPool(t, schemaName(t))
	fsys := fstest.MapFS{
		"001_a.sql": {Data: []byte(`CREATE TABLE c1 (id int);`)},
		"002_b.sql": {Data: []byte(`CREATE TABLE c2 (id int);`)},
	}

	errs := make([]error, 2)
	done := make([][]string, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			done[i], errs[i] = Apply(context.Background(), pool, fsys)
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("Apply %d: %v", i, errs[i])
		}
	}
	// Суммарно применено ровно 2 миграции — без дублей.
	total := len(done[0]) + len(done[1])
	if total != 2 {
		t.Fatalf("total applied = %d, want 2 (got %v + %v)", total, done[0], done[1])
	}
	if n := countRows(t, pool, `SELECT count(*) FROM schema_migrations`); n != 2 {
		t.Fatalf("schema_migrations rows = %d, want 2", n)
	}
}
