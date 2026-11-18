package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"hookrelay/internal/domain"
)

var testPolicy = domain.RetryPolicy{
	BaseDelay:  time.Minute,
	MaxDelay:   10 * time.Minute,
	MaxRetries: 5,
}

// claimedFixture создаёт delivery в состоянии processing с locked_by=workerID.
// attempts=2 (как в deliveryFixture).
func claimedFixture(t *testing.T, store *Store, workerID string) int64 {
	t.Helper()
	future := time.Now().Add(time.Hour)
	id := deliveryFixture(t, store, "processing", nil, &future)
	if _, err := store.pool.Exec(context.Background(),
		`UPDATE deliveries SET locked_by = $2 WHERE id = $1`, id, workerID); err != nil {
		t.Fatalf("set locked_by: %v", err)
	}
	return id
}

// claimedDelivery собирает ClaimedDelivery для SaveDeliveryResult.
func claimedDelivery(id int64, attempts int) domain.ClaimedDelivery {
	return domain.ClaimedDelivery{ID: id, Attempts: attempts}
}

func setAttempts(t *testing.T, store *Store, id int64, attempts int) {
	t.Helper()
	if _, err := store.pool.Exec(context.Background(),
		`UPDATE deliveries SET attempts = $2 WHERE id = $1`, id, attempts); err != nil {
		t.Fatalf("set attempts: %v", err)
	}
}

func attemptCount(t *testing.T, store *Store, deliveryID int64) int {
	t.Helper()
	var n int
	if err := store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1`,
		deliveryID).Scan(&n); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	return n
}

func TestSaveDeliveryResultSuccess(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")

	res := domain.DeliveryResult{StatusCode: 200, Body: "ok", Success: true, Duration: 120 * time.Millisecond}
	if err := store.SaveDeliveryResult(context.Background(), "w1", claimedDelivery(id, 2), res, testPolicy); err != nil {
		t.Fatalf("save: %v", err)
	}

	status, lockedBy, _, attempts := deliveryState(t, store, id)
	if status != "succeeded" {
		t.Fatalf("status = %q, want succeeded", status)
	}
	if lockedBy != "" {
		t.Fatalf("locked_by = %q, want cleared", lockedBy)
	}
	if attempts != 3 { // фикстура создаёт attempts=2, +1 за попытку
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if n := attemptCount(t, store, id); n != 1 {
		t.Fatalf("delivery_attempts rows = %d, want 1", n)
	}

	var statusCode int
	var att int
	if err := store.pool.QueryRow(context.Background(),
		`SELECT attempt, status_code FROM delivery_attempts WHERE delivery_id = $1`,
		id).Scan(&att, &statusCode); err != nil {
		t.Fatalf("select attempt: %v", err)
	}
	// attempt в истории — сквозной номер (MAX+1), у фикстуры строк
	// истории нет → 1, хотя deliveries.attempts уже 3.
	if att != 1 || statusCode != 200 {
		t.Fatalf("attempt=%d status_code=%d, want 1/200", att, statusCode)
	}
}

func TestSaveDeliveryResultRetryable(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")

	res := domain.DeliveryResult{Retryable: true, Err: errors.New("connection refused"), Duration: time.Second}
	if err := store.SaveDeliveryResult(context.Background(), "w1", claimedDelivery(id, 2), res, testPolicy); err != nil {
		t.Fatalf("save: %v", err)
	}

	status, lockedBy, lockedUntil, attempts := deliveryState(t, store, id)
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
	if lockedBy != "" || lockedUntil != nil {
		t.Fatal("lease fields not cleared")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}

	var availableAt time.Time
	var lastErr pgtype.Text
	if err := store.pool.QueryRow(context.Background(),
		`SELECT available_at, last_error FROM deliveries WHERE id = $1`, id,
	).Scan(&availableAt, &lastErr); err != nil {
		t.Fatalf("select delivery: %v", err)
	}
	// completed=3 → delay = base*2^2 = 4min, jitter [3.2min, 4.8min].
	if !availableAt.After(time.Now().Add(3*time.Minute)) ||
		availableAt.After(time.Now().Add(5*time.Minute)) {
		t.Fatalf("available_at = %v, want ~now+[3m,5m]", availableAt)
	}
	if lastErr.String != "connection refused" {
		t.Fatalf("last_error = %v", lastErr.String)
	}
}

func TestSaveDeliveryResultDeadOnPermanentError(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")

	res := domain.DeliveryResult{StatusCode: 400, Body: "bad request", Duration: 50 * time.Millisecond}
	if err := store.SaveDeliveryResult(context.Background(), "w1", claimedDelivery(id, 2), res, testPolicy); err != nil {
		t.Fatalf("save: %v", err)
	}

	status, _, _, attempts := deliveryState(t, store, id)
	if status != "dead" {
		t.Fatalf("status = %q, want dead", status)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}

	var lastErr pgtype.Text
	if err := store.pool.QueryRow(context.Background(),
		`SELECT last_error FROM deliveries WHERE id = $1`, id,
	).Scan(&lastErr); err != nil {
		t.Fatalf("select last_error: %v", err)
	}
	if lastErr.String != "HTTP 400" {
		t.Fatalf("last_error = %v, want HTTP 400", lastErr.String)
	}
}

// TestSaveDeliveryResultExhaustsRetries: attempts=5 → следующая
// временная ошибка (6-я попытка) исчерпывает MaxRetries=5 → dead.
func TestSaveDeliveryResultExhaustsRetries(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")
	setAttempts(t, store, id, 5)

	res := domain.DeliveryResult{Retryable: true, Err: errors.New("timeout"), Duration: time.Second}
	if err := store.SaveDeliveryResult(context.Background(), "w1",
		claimedDelivery(id, 5), res, testPolicy); err != nil {
		t.Fatalf("save: %v", err)
	}

	status, _, _, attempts := deliveryState(t, store, id)
	if status != "dead" {
		t.Fatalf("status = %q, want dead (retries exhausted)", status)
	}
	if attempts != 6 {
		t.Fatalf("attempts = %d, want 6", attempts)
	}
	if n := attemptCount(t, store, id); n != 1 {
		t.Fatalf("delivery_attempts rows = %d, want 1", n)
	}
}

// TestSaveDeliveryResultLastRetryAllowed: attempts=4 → 5-я попытка
// временной ошибки ещё разрешает повтор (5 <= MaxRetries) → pending.
func TestSaveDeliveryResultLastRetryAllowed(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")
	setAttempts(t, store, id, 4)

	res := domain.DeliveryResult{Retryable: true, Err: errors.New("timeout")}
	if err := store.SaveDeliveryResult(context.Background(), "w1",
		claimedDelivery(id, 4), res, testPolicy); err != nil {
		t.Fatalf("save: %v", err)
	}

	status, _, _, attempts := deliveryState(t, store, id)
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
	if attempts != 5 {
		t.Fatalf("attempts = %d, want 5", attempts)
	}
}

// TestSaveDeliveryResultAttemptsMismatch: d.Attempts не совпадает
// с attempts в БД → UPDATE не должен изменить запись.
func TestSaveDeliveryResultAttemptsMismatch(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1") // attempts=2 в БД

	res := domain.DeliveryResult{StatusCode: 200, Success: true}
	err := store.SaveDeliveryResult(context.Background(), "w1",
		claimedDelivery(id, 5), res, testPolicy) // врёт про attempts
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}

	status, lockedBy, _, attempts := deliveryState(t, store, id)
	if status != "processing" || lockedBy != "w1" || attempts != 2 {
		t.Fatalf("delivery modified: status=%q locked_by=%q attempts=%d",
			status, lockedBy, attempts)
	}
	if n := attemptCount(t, store, id); n != 0 {
		t.Fatalf("delivery_attempts rows = %d, want 0", n)
	}
}

func TestSaveDeliveryResultStaleWorker(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")

	// Чужой workerID не должен перезаписать результат.
	res := domain.DeliveryResult{StatusCode: 200, Success: true}
	err := store.SaveDeliveryResult(context.Background(), "other-worker", claimedDelivery(id, 2), res, testPolicy)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}

	status, lockedBy, _, attempts := deliveryState(t, store, id)
	if status != "processing" || lockedBy != "w1" || attempts != 2 {
		t.Fatalf("delivery modified by stale worker: status=%q locked_by=%q attempts=%d",
			status, lockedBy, attempts)
	}
	if n := attemptCount(t, store, id); n != 0 {
		t.Fatalf("delivery_attempts rows = %d, want 0", n)
	}
}
