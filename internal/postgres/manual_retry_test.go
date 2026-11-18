package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"hookrelay/internal/domain"
)

// deadFixture создаёт delivery в статусе dead с attempts=6,
// last_error и записью в delivery_attempts.
func deadFixture(t *testing.T, store *Store) int64 {
	t.Helper()
	ctx := context.Background()

	future := time.Now().Add(time.Hour)
	id := deliveryFixture(t, store, "dead", nil, &future)
	if _, err := store.pool.Exec(ctx, `
		UPDATE deliveries SET
			attempts = 6, last_error = 'HTTP 500',
			locked_by = NULL, locked_until = NULL
		WHERE id = $1`, id); err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO delivery_attempts (delivery_id, attempt, status_code, duration_ms)
		VALUES ($1, 1, 500, 100), ($1, 2, 500, 100)`, id); err != nil {
		t.Fatalf("insert attempts history: %v", err)
	}
	return id
}

func TestRetryDeliverySuccess(t *testing.T) {
	store := testStore(t)
	id := deadFixture(t, store)

	d, err := store.RetryDelivery(context.Background(), id)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if d.Status != "pending" {
		t.Fatalf("status = %q, want pending", d.Status)
	}
	if d.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 (new retry cycle)", d.Attempts)
	}
	if d.LastError != nil {
		t.Fatalf("last_error = %v, want NULL", *d.LastError)
	}
	// available_at = now() — Worker должен забрать задачу сразу.
	if time.Since(d.AvailableAt) > 5*time.Second {
		t.Fatalf("available_at = %v, want ~now", d.AvailableAt)
	}
	// История попыток сохранена.
	if n := attemptCount(t, store, id); n != 2 {
		t.Fatalf("delivery_attempts rows = %d, want 2", n)
	}
}

func TestRetryDeliveryNotDead(t *testing.T) {
	store := testStore(t)
	for _, status := range []string{"pending", "processing", "succeeded"} {
		future := time.Now().Add(time.Hour)
		id := deliveryFixture(t, store, status, nil, &future)

		if _, err := store.RetryDelivery(context.Background(), id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("status %s: err = %v, want pgx.ErrNoRows", status, err)
		}
		got, err := store.GetDelivery(context.Background(), id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != status {
			t.Fatalf("status changed: %q → %q", status, got.Status)
		}
	}
}

func TestRetryDeliveryNotFound(t *testing.T) {
	store := testStore(t)
	if _, err := store.GetDelivery(context.Background(), 999999); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("get missing: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := store.RetryDelivery(context.Background(), 999999); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("retry missing: err = %v, want pgx.ErrNoRows", err)
	}
}

// TestRetryDeliveryConcurrent: два параллельных retry одной доставки —
// ровно один успешен, второй получает ErrNoRows.
func TestRetryDeliveryConcurrent(t *testing.T) {
	store := testStore(t)
	id := deadFixture(t, store)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.RetryDelivery(context.Background(), id)
		}(i)
	}
	wg.Wait()

	var ok, conflict int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, pgx.ErrNoRows):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("ok=%d conflict=%d, want 1/1", ok, conflict)
	}

	d, err := store.GetDelivery(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if d.Status != "pending" {
		t.Fatalf("status = %q, want pending", d.Status)
	}
}

// TestRetryDeliveryNewCycle: после ручного retry автоматический лимит
// работает заново — временная ошибка на «первой» попытке нового цикла
// снова ставит pending (а не dead), а номер в истории продолжается.
func TestRetryDeliveryNewCycle(t *testing.T) {
	store := testStore(t)
	id := deadFixture(t, store) // attempts=6, 2 записи в истории

	if _, err := store.RetryDelivery(context.Background(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}

	// Эмулируем Worker: claim → временная ошибка → save.
	claimed, err := store.ClaimDeliveries(context.Background(), "w1", 100, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	d, ok := findClaimed(claimed, id)
	if !ok {
		t.Fatal("retried delivery was not claimed — available_at not immediate")
	}
	if d.Attempts != 0 {
		t.Fatalf("claimed attempts = %d, want 0 (new retry cycle)", d.Attempts)
	}

	res := domain.DeliveryResult{Retryable: true, Err: errors.New("timeout")}
	if err := store.SaveDeliveryResult(context.Background(), "w1", d, res, testPolicy); err != nil {
		t.Fatalf("save after retry: %v", err)
	}

	// Новая попытка не исчерпала лимит → pending, attempts=1.
	status, _, _, attempts := deliveryState(t, store, id)
	if status != "pending" || attempts != 1 {
		t.Fatalf("status=%q attempts=%d, want pending/1", status, attempts)
	}

	// Номер попытки в истории — 3 (две старые + одна новая), не 1.
	var maxAttempt int
	if err := store.pool.QueryRow(context.Background(),
		`SELECT COALESCE(MAX(attempt),0) FROM delivery_attempts WHERE delivery_id = $1`,
		id).Scan(&maxAttempt); err != nil {
		t.Fatalf("max attempt: %v", err)
	}
	if maxAttempt != 3 {
		t.Fatalf("max attempt = %d, want 3 (history numbering continues)", maxAttempt)
	}
}
