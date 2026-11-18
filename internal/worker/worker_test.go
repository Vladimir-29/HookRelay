package worker

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"hookrelay/internal/domain"
	"hookrelay/internal/postgres"
)

// testEnv поднимает Store и отдельный pool для фикстур/проверок.
// Без TEST_DATABASE_URL тесты пропускаются.
func testEnv(t *testing.T) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := postgres.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect store: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(func() { store.Close(); pool.Close() })
	return store, pool
}

// pendingDelivery создаёт endpoint+event+delivery(pending) и чистит за собой.
func pendingDelivery(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("workertest-%s-%d", t.Name(), time.Now().UnixNano())

	var endpointID, eventID, deliveryID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO endpoints (name, url) VALUES ($1, $2) RETURNING id`,
		name, "https://example.com/hook").Scan(&endpointID); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (event_type, payload) VALUES ('worker.test', '{}'::jsonb) RETURNING id`,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO deliveries (event_id, endpoint_id, status) VALUES ($1, $2, 'pending') RETURNING id`,
		eventID, endpointID).Scan(&deliveryID); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM delivery_attempts WHERE delivery_id = $1`, deliveryID)
		_, _ = pool.Exec(ctx, `DELETE FROM deliveries WHERE id = $1`, deliveryID)
		_, _ = pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, eventID)
		_, _ = pool.Exec(ctx, `DELETE FROM endpoints WHERE id = $1`, endpointID)
	})
	return deliveryID
}

// waitAttempts ждёт, пока Worker зафиксирует хотя бы n попыток.
// Для retryable-сценария статус возвращается в pending — по статусу
// переход не отличить от начального состояния.
func waitAttempts(t *testing.T, pool *pgxpool.Pool, id int64, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var attempts int
		if err := pool.QueryRow(context.Background(),
			`SELECT attempts FROM deliveries WHERE id = $1`, id).Scan(&attempts); err != nil {
			t.Fatalf("select attempts: %v", err)
		}
		if attempts >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("delivery %d did not reach %d attempts", id, want)
}

// waitStatus ждёт перехода доставки в целевой статус.
func waitStatus(t *testing.T, pool *pgxpool.Pool, id int64, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := pool.QueryRow(context.Background(),
			`SELECT status FROM deliveries WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("select status: %v", err)
		}
		if status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("delivery %d did not reach status %q", id, want)
}

func deliveryRow(t *testing.T, pool *pgxpool.Pool, id int64) (status string, attempts int, lastErr *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT status, attempts, last_error FROM deliveries WHERE id = $1`, id,
	).Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatalf("select delivery: %v", err)
	}
	return
}

func runWorker(t *testing.T, store *postgres.Store, s Sender, cfg Config) (cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan struct{})
	go func() {
		New(store, s, cfg).Run(ctx)
		close(ch)
	}()
	return cancel, ch
}

type fakeSender struct {
	result      domain.DeliveryResult
	delay       time.Duration
	calls       atomic.Int32
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (f *fakeSender) Send(ctx context.Context, d domain.ClaimedDelivery) domain.DeliveryResult {
	cur := f.inFlight.Add(1)
	for {
		max := f.maxInFlight.Load()
		if cur <= max || f.maxInFlight.CompareAndSwap(max, cur) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(f.delay):
		}
	}
	return f.result
}

func (f *fakeSender) CloseIdleConnections() {}

func testConfig() Config {
	return Config{
		BatchSize:    10,
		PollInterval: 50 * time.Millisecond,
		Lease:        30 * time.Second,
		Retry: domain.RetryPolicy{
			BaseDelay:  time.Minute,
			MaxDelay:   10 * time.Minute,
			MaxRetries: 5,
		},
	}
}

func TestWorkerSuccess(t *testing.T) {
	store, pool := testEnv(t)
	id := pendingDelivery(t, pool)

	s := &fakeSender{result: domain.DeliveryResult{StatusCode: 200, Success: true}}
	cancel, done := runWorker(t, store, s, testConfig())
	defer cancel()

	waitStatus(t, pool, id, "succeeded")
	cancel()
	<-done

	status, attempts, _ := deliveryRow(t, pool, id)
	if status != "succeeded" || attempts != 1 {
		t.Fatalf("status=%q attempts=%d", status, attempts)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if n != 1 {
		t.Fatalf("delivery_attempts rows = %d, want 1", n)
	}
}

func TestWorkerRetryableReturnsToPending(t *testing.T) {
	store, pool := testEnv(t)
	id := pendingDelivery(t, pool)

	s := &fakeSender{result: domain.DeliveryResult{Retryable: true, Err: fmt.Errorf("timeout")}}
	cancel, done := runWorker(t, store, s, testConfig())
	defer cancel()

	waitAttempts(t, pool, id, 1)
	cancel()
	<-done

	status, attempts, lastErr := deliveryRow(t, pool, id)
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if lastErr == nil || *lastErr != "timeout" {
		t.Fatalf("last_error = %v", lastErr)
	}
	// available_at отодинут на backoff-задержку — delivery не должна
	// вернуться в работу раньше времени.
	var availableAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT available_at FROM deliveries WHERE id = $1`, id).Scan(&availableAt); err != nil {
		t.Fatalf("select available_at: %v", err)
	}
	if !availableAt.After(time.Now().Add(30 * time.Second)) {
		t.Fatalf("available_at = %v, want ~now+1m", availableAt)
	}
}

func TestWorkerDeadOnPermanentError(t *testing.T) {
	store, pool := testEnv(t)
	id := pendingDelivery(t, pool)

	s := &fakeSender{result: domain.DeliveryResult{StatusCode: 400, Body: "bad"}}
	cancel, done := runWorker(t, store, s, testConfig())
	defer cancel()

	waitStatus(t, pool, id, "dead")
	cancel()
	<-done

	_, attempts, lastErr := deliveryRow(t, pool, id)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if lastErr == nil || *lastErr != "HTTP 400" {
		t.Fatalf("last_error = %v", lastErr)
	}
}

func TestWorkerParallelBatch(t *testing.T) {
	store, pool := testEnv(t)
	const n = 3
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = pendingDelivery(t, pool)
	}

	s := &fakeSender{
		result: domain.DeliveryResult{StatusCode: 200, Success: true},
		delay:  80 * time.Millisecond,
	}
	cancel, done := runWorker(t, store, s, testConfig())
	defer cancel()

	for _, id := range ids {
		waitStatus(t, pool, id, "succeeded")
	}
	cancel()
	<-done

	if got := s.maxInFlight.Load(); got < 2 {
		t.Fatalf("max in-flight = %d, want parallel processing (>1)", got)
	}
	if s.calls.Load() != n {
		t.Fatalf("sender calls = %d, want %d", s.calls.Load(), n)
	}
}

// TestWorkerRetryExhaustion: вечные временные ошибки — после
// MaxRetries+1 попыток доставка переходит в dead.
func TestWorkerRetryExhaustion(t *testing.T) {
	store, pool := testEnv(t)
	id := pendingDelivery(t, pool)

	s := &fakeSender{result: domain.DeliveryResult{Retryable: true, Err: fmt.Errorf("boom")}}
	cfg := testConfig()
	cfg.Retry = domain.RetryPolicy{
		BaseDelay:  50 * time.Millisecond,
		MaxDelay:   500 * time.Millisecond,
		MaxRetries: 2,
	}
	cfg.PollInterval = 20 * time.Millisecond

	cancel, done := runWorker(t, store, s, cfg)
	defer cancel()

	waitStatus(t, pool, id, "dead")
	cancel()
	<-done

	_, attempts, _ := deliveryRow(t, pool, id)
	if attempts != 3 { // 1 исходная + 2 повтора
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if n != 3 {
		t.Fatalf("delivery_attempts rows = %d, want 3", n)
	}
}

func TestWorkerStopsOnCancel(t *testing.T) {
	store, pool := testEnv(t)
	_ = pool

	s := &fakeSender{result: domain.DeliveryResult{StatusCode: 200, Success: true}}
	cfg := testConfig()
	cfg.PollInterval = time.Hour // если не отменится — зависнет

	cancel, done := runWorker(t, store, s, cfg)
	time.Sleep(150 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancel")
	}
	if time.Since(start) > time.Second {
		t.Fatal("worker ignored cancellation during poll wait")
	}
}

// blockingSender ждёт отмены ctx внутри Send, затем возвращает результат —
// имитация HTTP-запроса, завершившегося одновременно с shutdown.
type blockingSender struct {
	fakeSender
}

func (b *blockingSender) Send(ctx context.Context, d domain.ClaimedDelivery) domain.DeliveryResult {
	<-ctx.Done()
	return domain.DeliveryResult{StatusCode: 200, Success: true, Duration: time.Millisecond}
}

// TestWorkerSavesResultOnShutdown: Send завершился после отмены ctx —
// результат всё равно сохраняется (WithoutCancel + timeout), доставка
// доходит до succeeded, а не висит до lease.
func TestWorkerSavesResultOnShutdown(t *testing.T) {
	store, pool := testEnv(t)
	id := pendingDelivery(t, pool)

	s := &blockingSender{}
	cfg := testConfig()
	cfg.PollInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New(store, s, cfg).Run(ctx)
		close(done)
	}()

	// Ждём, пока worker захватит и начнёт обрабатывать доставку.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		_ = pool.QueryRow(context.Background(),
			`SELECT status FROM deliveries WHERE id = $1`, id).Scan(&status)
		if status == "processing" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivery was not claimed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel() // Send вернётся по ctx.Done, save должен пройти
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}

	waitStatus(t, pool, id, "succeeded")
	_, attempts, _ := deliveryRow(t, pool, id)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestWorkerClaimErrorNoBusyLoop(t *testing.T) {
	store, pool := testEnv(t)
	_ = pool
	store.Close() // пул закрыт — ClaimDeliveries будет ошибаться

	s := &fakeSender{}
	cfg := testConfig()
	cfg.PollInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New(store, s, cfg).Run(ctx)
		close(done)
	}()

	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
	// При busy loop без sleep worker бы сделал тысячи итераций;
	// здесь важно, что он честно ждал poll interval и остановился.
	if time.Since(start) > time.Second {
		t.Fatal("shutdown took too long")
	}
}
