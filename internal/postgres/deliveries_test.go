package postgres

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"hookrelay/internal/domain"
)

// testStore подключается к тестовой БД из TEST_DATABASE_URL.
// Без переменной тесты пропускаются — обычный `go test ./...` не трогает БД.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	store, err := Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// deliveryFixture создаёт endpoint + event + delivery и удаляет их
// после теста. Возвращает id доставки.
func deliveryFixture(t *testing.T, store *Store, status string, availableAt, lockedUntil *time.Time) int64 {
	t.Helper()
	ctx := context.Background()

	var endpointID, eventID, deliveryID int64
	err := store.pool.QueryRow(ctx,
		`INSERT INTO endpoints (name, url) VALUES ($1, $2) RETURNING id`,
		"claimtest-"+t.Name(), "https://example.com/hook",
	).Scan(&endpointID)
	if err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	if err := store.pool.QueryRow(ctx,
		`INSERT INTO events (event_type, payload) VALUES ('claim.test', '{}'::jsonb) RETURNING id`,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	err = store.pool.QueryRow(ctx,
		`INSERT INTO deliveries (event_id, endpoint_id, status, available_at, locked_until, attempts)
		 VALUES ($1, $2, $3, COALESCE($4, now()), $5, 2) RETURNING id`,
		eventID, endpointID, status, availableAt, lockedUntil,
	).Scan(&deliveryID)
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}

	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM delivery_attempts WHERE delivery_id = $1`, deliveryID)
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM deliveries WHERE id = $1`, deliveryID)
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, eventID)
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM endpoints WHERE id = $1`, endpointID)
	})
	return deliveryID
}

// findClaimed ищет доставку id в результате ClaimDeliveries.
func findClaimed(ds []domain.ClaimedDelivery, id int64) (domain.ClaimedDelivery, bool) {
	for _, d := range ds {
		if d.ID == id {
			return d, true
		}
	}
	return domain.ClaimedDelivery{}, false
}

// deliveryState читает status/locked_by/locked_until/attempts доставки.
func deliveryState(t *testing.T, store *Store, id int64) (string, string, *time.Time, int) {
	t.Helper()
	var status, lockedBy pgtype.Text
	var lockedUntil pgtype.Timestamptz
	var attempts int
	err := store.pool.QueryRow(context.Background(),
		`SELECT status, locked_by, locked_until, attempts FROM deliveries WHERE id = $1`, id,
	).Scan(&status, &lockedBy, &lockedUntil, &attempts)
	if err != nil {
		t.Fatalf("select delivery %d: %v", id, err)
	}
	var until *time.Time
	if lockedUntil.Valid {
		u := lockedUntil.Time
		until = &u
	}
	return status.String, lockedBy.String, until, attempts
}

func TestClaimDeliveriesValidation(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	lease := 30 * time.Second

	for name, args := range map[string][3]any{
		"empty worker":   {"", 1, lease},
		"blank worker":   {"   ", 1, lease},
		"zero batch":     {"w1", 0, lease},
		"negative batch": {"w1", -1, lease},
		"zero lease":     {"w1", 1, time.Duration(0)},
		"negative lease": {"w1", 1, -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.ClaimDeliveries(ctx, args[0].(string), args[1].(int), args[2].(time.Duration)); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestClaimDeliveriesPending(t *testing.T) {
	store := testStore(t)
	past := time.Now().Add(-time.Minute)
	id := deliveryFixture(t, store, "pending", &past, nil)

	got, err := store.ClaimDeliveries(context.Background(), "worker-1", 100, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	d, ok := findClaimed(got, id)
	if !ok {
		t.Fatalf("delivery %d not claimed (got %d deliveries)", id, len(got))
	}
	if d.EventType != "claim.test" {
		t.Fatalf("event type = %q", d.EventType)
	}
	if d.TargetURL != "https://example.com/hook" {
		t.Fatalf("target url = %q", d.TargetURL)
	}
	if string(d.Payload) != "{}" {
		t.Fatalf("payload = %s", d.Payload)
	}
	if d.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (unchanged)", d.Attempts)
	}
	if d.LeaseUntil.IsZero() {
		t.Fatal("lease until is zero")
	}

	status, lockedBy, lockedUntil, attempts := deliveryState(t, store, id)
	if status != "processing" {
		t.Fatalf("status = %q, want processing", status)
	}
	if lockedBy != "worker-1" {
		t.Fatalf("locked_by = %q", lockedBy)
	}
	if lockedUntil == nil || !lockedUntil.After(time.Now()) {
		t.Fatalf("locked_until = %v, want future", lockedUntil)
	}
	if attempts != 2 {
		t.Fatalf("attempts in db = %d, want 2", attempts)
	}
}

func TestClaimDeliveriesSkipsFutureAvailableAt(t *testing.T) {
	store := testStore(t)
	future := time.Now().Add(time.Hour)
	id := deliveryFixture(t, store, "pending", &future, nil)

	if _, err := store.ClaimDeliveries(context.Background(), "worker-1", 100, 30*time.Second); err != nil {
		t.Fatalf("claim: %v", err)
	}
	status, _, _, _ := deliveryState(t, store, id)
	if status != "pending" {
		t.Fatalf("fixture status = %q, want pending", status)
	}
}

func TestClaimDeliveriesReclaimsExpiredLease(t *testing.T) {
	store := testStore(t)
	past := time.Now().Add(-time.Minute)
	id := deliveryFixture(t, store, "processing", nil, &past)

	got, err := store.ClaimDeliveries(context.Background(), "worker-2", 100, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, ok := findClaimed(got, id); !ok {
		t.Fatal("expired-lease delivery was not claimed")
	}
	status, lockedBy, _, attempts := deliveryState(t, store, id)
	if status != "processing" || lockedBy != "worker-2" {
		t.Fatalf("reclaimed delivery: status=%q locked_by=%q", status, lockedBy)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestClaimDeliveriesSkipsActiveLease(t *testing.T) {
	store := testStore(t)
	future := time.Now().Add(time.Hour)
	id := deliveryFixture(t, store, "processing", nil, &future)
	if _, err := store.pool.Exec(context.Background(),
		`UPDATE deliveries SET locked_by = 'old-worker' WHERE id = $1`, id); err != nil {
		t.Fatalf("set locked_by: %v", err)
	}

	got, err := store.ClaimDeliveries(context.Background(), "worker-2", 100, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, ok := findClaimed(got, id); ok {
		t.Fatal("delivery with active lease was claimed")
	}
	status, lockedBy, _, _ := deliveryState(t, store, id)
	if status != "processing" {
		t.Fatalf("status = %q, want processing", status)
	}
	if lockedBy != "old-worker" {
		t.Fatalf("locked_by = %q, want old-worker (untouched)", lockedBy)
	}
}

func TestClaimDeliveriesConcurrentWorkers(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	var endpointID int64
	if err := store.pool.QueryRow(ctx,
		`INSERT INTO endpoints (name, url) VALUES ($1, $2) RETURNING id`,
		"claimtest-conc-"+t.Name(), "https://example.com/hook",
	).Scan(&endpointID); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	// UNIQUE(event_id, endpoint_id) не даёт повесить несколько доставок
	// на одну пару — создаём отдельное событие на каждую доставку.
	var deliveryIDs, eventIDs []int64
	for i := 0; i < 6; i++ {
		var eventID, id int64
		if err := store.pool.QueryRow(ctx,
			`INSERT INTO events (event_type, payload) VALUES ('claim.test', '{}'::jsonb) RETURNING id`,
		).Scan(&eventID); err != nil {
			t.Fatalf("insert event: %v", err)
		}
		if err := store.pool.QueryRow(ctx,
			`INSERT INTO deliveries (event_id, endpoint_id, status) VALUES ($1, $2, 'pending') RETURNING id`,
			eventID, endpointID).Scan(&id); err != nil {
			t.Fatalf("insert delivery: %v", err)
		}
		deliveryIDs = append(deliveryIDs, id)
		eventIDs = append(eventIDs, eventID)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(ctx, `DELETE FROM deliveries WHERE endpoint_id = $1`, endpointID)
		_, _ = store.pool.Exec(ctx, `DELETE FROM events WHERE id = ANY($1)`, eventIDs)
		_, _ = store.pool.Exec(ctx, `DELETE FROM endpoints WHERE id = $1`, endpointID)
	})

	// Два worker'а стартуют одновременно и запрашивают batch=50 —
	// больше числа фикстур, чтобы гарантировать пересечение кандидатов.
	const workers = 2
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([][]int64, workers)
	errs := make([]error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			ds, err := store.ClaimDeliveries(ctx, "conc-worker", 50, time.Minute)
			if err != nil {
				errs[w] = err
				return
			}
			for _, d := range ds {
				results[w] = append(results[w], d.ID)
			}
		}(w)
	}
	close(start)
	wg.Wait()
	for w := 0; w < workers; w++ {
		if errs[w] != nil {
			t.Fatalf("worker %d: %v", w, errs[w])
		}
	}

	// Ни одна фикстурная доставка не должна достаться обоим worker'ам,
	// и все шесть должны быть захвачены суммарно.
	seen := map[int64]int{}
	for _, ids := range results {
		for _, id := range ids {
			seen[id]++
		}
	}
	claimed := 0
	for _, id := range deliveryIDs {
		if seen[id] > 1 {
			t.Fatalf("delivery %d claimed by %d workers", id, seen[id])
		}
		if seen[id] == 1 {
			claimed++
		}
	}
	if claimed != len(deliveryIDs) {
		t.Fatalf("claimed %d of %d fixture deliveries", claimed, len(deliveryIDs))
	}
}
