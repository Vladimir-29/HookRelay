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

// claimedFixture создаёт delivery в состоянии processing с locked_by=workerID.
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
	if err := store.SaveDeliveryResult(context.Background(), "w1", id, res, 5*time.Second); err != nil {
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
	if att != 3 || statusCode != 200 {
		t.Fatalf("attempt=%d status_code=%d, want 3/200", att, statusCode)
	}
}

func TestSaveDeliveryResultRetryable(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")

	res := domain.DeliveryResult{Retryable: true, Err: errors.New("connection refused"), Duration: time.Second}
	if err := store.SaveDeliveryResult(context.Background(), "w1", id, res, time.Minute); err != nil {
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
	if !availableAt.After(time.Now().Add(30 * time.Second)) {
		t.Fatalf("available_at = %v, want ~now+1m", availableAt)
	}
	if lastErr.String != "connection refused" {
		t.Fatalf("last_error = %v", lastErr.String)
	}
}

func TestSaveDeliveryResultDead(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")

	res := domain.DeliveryResult{StatusCode: 400, Body: "bad request", Duration: 50 * time.Millisecond}
	if err := store.SaveDeliveryResult(context.Background(), "w1", id, res, time.Minute); err != nil {
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

func TestSaveDeliveryResultStaleWorker(t *testing.T) {
	store := testStore(t)
	id := claimedFixture(t, store, "w1")

	// Чужой workerID не должен перезаписать результат.
	res := domain.DeliveryResult{StatusCode: 200, Success: true}
	err := store.SaveDeliveryResult(context.Background(), "other-worker", id, res, time.Minute)
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
