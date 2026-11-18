// Package e2e — сквозные интеграционные тесты HookRelay:
// HTTP API → PostgreSQL → Worker → HTTP-доставка с HMAC-подписью.
//
// Запуск: TEST_DATABASE_URL=postgres://... go test ./internal/e2e/
//
// SSRF-защита не ослабляется: Sender собирается через
// delivery.NewWithClient с обычным http.Client для httptest-серверов
// на loopback. Сама SSRF-проверка покрыта тестами internal/delivery.
package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"hookrelay/internal/delivery"
	"hookrelay/internal/domain"
	"hookrelay/internal/httpapi"
	"hookrelay/internal/postgres"
	"hookrelay/internal/secrets"
	"hookrelay/internal/worker"
)

// testBox — Box с фиксированным тестовым ключом.
var testBox = func() *secrets.Box {
	b, err := secrets.NewBox(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		panic(err)
	}
	return b
}()

func testStore(t *testing.T) *postgres.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	store, err := postgres.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// apiServer поднимает реальный HTTP API поверх httptest.
func apiServer(t *testing.T, store *postgres.Store) *httptest.Server {
	t.Helper()
	srv := httpapi.NewServer("0", store, testBox)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func apiCall(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// fixture хранит id созданных через API сущностей для cleanup.
type fixture struct {
	endpointID int64
	eventIDs   []int64
}

func (f *fixture) cleanup(t *testing.T, store *postgres.Store) {
	t.Helper()
	ctx := context.Background()
	pool := store.Pool()
	if f.endpointID == 0 {
		return
	}
	// Порядок важен: attempts → deliveries → events → endpoints
	// (subscriptions удаляются каскадом вместе с endpoint).
	_, _ = pool.Exec(ctx, `
		DELETE FROM delivery_attempts WHERE delivery_id IN
			(SELECT id FROM deliveries WHERE endpoint_id = $1)`, f.endpointID)
	_, _ = pool.Exec(ctx, `DELETE FROM deliveries WHERE endpoint_id = $1`, f.endpointID)
	for _, id := range f.eventIDs {
		_, _ = pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, id)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM endpoints WHERE id = $1`, f.endpointID)
}

// createEndpoint создаёт endpoint через API и возвращает id и
// raw-секрет (декодированный из base64url ответа).
func createEndpoint(t *testing.T, api *httptest.Server, f *fixture, name, targetURL string) (int64, []byte) {
	t.Helper()
	code, resp := apiCall(t, http.MethodPost, api.URL+"/api/v1/endpoints",
		fmt.Sprintf(`{"name":%q,"url":%q}`, name, targetURL))
	if code != http.StatusCreated {
		t.Fatalf("create endpoint: %d %v", code, resp)
	}
	f.endpointID = int64(resp["id"].(float64))
	secretB64 := resp["secret"].(string)
	raw, err := base64.RawURLEncoding.DecodeString(secretB64)
	if err != nil || len(raw) != 32 {
		t.Fatalf("bad secret in response: %q", secretB64)
	}
	return f.endpointID, raw
}

func subscribe(t *testing.T, api *httptest.Server, endpointID int64, eventType string) {
	t.Helper()
	code, resp := apiCall(t, http.MethodPut,
		fmt.Sprintf("%s/api/v1/endpoints/%d/subscriptions", api.URL, endpointID),
		fmt.Sprintf(`{"event_types":[%q]}`, eventType))
	if code != http.StatusOK {
		t.Fatalf("subscribe: %d %v", code, resp)
	}
}

// sendEvent отправляет событие через API, возвращает event id.
func sendEvent(t *testing.T, api *httptest.Server, f *fixture, eventType string, payload string) int64 {
	t.Helper()
	code, resp := apiCall(t, http.MethodPost, api.URL+"/api/v1/events",
		fmt.Sprintf(`{"event_type":%q,"payload":%s}`, eventType, payload))
	if code != http.StatusAccepted {
		t.Fatalf("send event: %d %v", code, resp)
	}
	id := int64(resp["id"].(float64))
	f.eventIDs = append(f.eventIDs, id)
	return id
}

// startWorker запускает Worker с реальным Sender (кастомный транспорт
// для loopback httptest) и останавливает его в cleanup.
func startWorker(t *testing.T, store *postgres.Store, policy domain.RetryPolicy) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	sender := delivery.NewWithClient(client, testBox)
	w := worker.New(store, sender, worker.Config{
		BatchSize:    10,
		PollInterval: 20 * time.Millisecond,
		Lease:        30 * time.Second,
		Retry:        policy,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	})
}

// waitFor ждёт условие до timeout, опрашивая каждые 20ms.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func deliveryID(t *testing.T, store *postgres.Store, eventID int64) int64 {
	t.Helper()
	var id int64
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT id FROM deliveries WHERE event_id = $1`, eventID).Scan(&id); err != nil {
		t.Fatalf("delivery for event %d: %v", eventID, err)
	}
	return id
}

func deliveryState(t *testing.T, store *postgres.Store, id int64) (status string, attempts int) {
	t.Helper()
	err := store.Pool().QueryRow(context.Background(),
		`SELECT status, attempts FROM deliveries WHERE id = $1`, id).Scan(&status, &attempts)
	if err != nil {
		t.Fatalf("delivery state: %v", err)
	}
	return status, attempts
}

func attemptRows(t *testing.T, store *postgres.Store, id int64) (count, maxAttempt int) {
	t.Helper()
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*), COALESCE(MAX(attempt),0) FROM delivery_attempts WHERE delivery_id = $1`,
		id).Scan(&count, &maxAttempt); err != nil {
		t.Fatalf("attempt rows: %v", err)
	}
	return count, maxAttempt
}

// receiver — тестовый HTTP-сервер, проверяющий HMAC-SHA256 подпись
// каждого запроса секретом endpoint и считающий обращения.
// Секрет задаётся после создания endpoint через setSecret —
// запросы приходят только после события, гонки нет.
type receiver struct {
	mu       sync.Mutex
	secret   []byte
	requests int
	sigOK    []bool
	// respond возвращает HTTP-статус для n-го запроса (0-based);
	// по умолчанию 200.
	respond func(n int) int
}

func newReceiver(t *testing.T, respond func(n int) int) (*receiver, *httptest.Server) {
	t.Helper()
	r := &receiver{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)

		r.mu.Lock()
		secret := r.secret
		n := r.requests
		r.requests++
		r.mu.Unlock()

		var ok bool
		if secret != nil {
			mac := hmac.New(sha256.New, secret)
			mac.Write(body)
			want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			ok = hmac.Equal([]byte(req.Header.Get("X-HookRelay-Signature")), []byte(want))
		}
		r.mu.Lock()
		r.sigOK = append(r.sigOK, ok)
		r.mu.Unlock()

		status := 200
		if r.respond != nil {
			status = r.respond(n)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return r, srv
}

func (r *receiver) setSecret(secret []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secret = secret
}

func (r *receiver) stats() (count int, allSigOK bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := len(r.sigOK) > 0
	for _, ok := range r.sigOK {
		all = all && ok
	}
	return r.requests, all
}

// e2eEnv — общий стенд: store, api, worker, fixture.
type e2eEnv struct {
	store *postgres.Store
	api   *httptest.Server
	f     fixture
}

func newE2EEnv(t *testing.T, policy domain.RetryPolicy) *e2eEnv {
	t.Helper()
	env := &e2eEnv{store: testStore(t)}
	env.api = apiServer(t, env.store)
	startWorker(t, env.store, policy)
	t.Cleanup(func() { env.f.cleanup(t, env.store) })
	return env
}

// setupEndpoint создаёт endpoint на URL receiver'а и подписывает
// его на eventType. Возвращает raw-секрет.
func (e *e2eEnv) setupEndpoint(t *testing.T, recv *receiver, recvURL, name, eventType string) []byte {
	t.Helper()
	_, secret := createEndpoint(t, e.api, &e.f, name, recvURL)
	recv.setSecret(secret)
	subscribe(t, e.api, e.f.endpointID, eventType)
	return secret
}

// TestE2EFullCycle: API → событие → Worker → подписанный webhook →
// succeeded + запись в delivery_attempts.
func TestE2EFullCycle(t *testing.T) {
	env := newE2EEnv(t, domain.RetryPolicy{
		BaseDelay: 50 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxRetries: 3,
	})
	recv, recvSrv := newReceiver(t, nil)
	env.setupEndpoint(t, recv, recvSrv.URL, "e2e-full", "e2e.ping")

	eventID := sendEvent(t, env.api, &env.f, "e2e.ping", `{"ping":1}`)
	dID := deliveryID(t, env.store, eventID)

	waitFor(t, "delivery succeeded", 10*time.Second, func() bool {
		status, _ := deliveryState(t, env.store, dID)
		return status == "succeeded"
	})

	count, _ := attemptRows(t, env.store, dID)
	if count != 1 {
		t.Fatalf("delivery_attempts rows = %d, want 1", count)
	}
	n, allOK := recv.stats()
	if n != 1 || !allOK {
		t.Fatalf("receiver got %d requests, allSigOK=%v", n, allOK)
	}
}

// TestE2ERetryThenSuccess: первая попытка → 500 (pending),
// вторая → 200 (succeeded). Подпись корректна в обоих запросах.
func TestE2ERetryThenSuccess(t *testing.T) {
	env := newE2EEnv(t, domain.RetryPolicy{
		BaseDelay: 50 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxRetries: 3,
	})
	recv, recvSrv := newReceiver(t, func(n int) int {
		if n == 0 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	env.setupEndpoint(t, recv, recvSrv.URL, "e2e-retry", "e2e.retry")

	eventID := sendEvent(t, env.api, &env.f, "e2e.retry", `{"try":2}`)
	dID := deliveryID(t, env.store, eventID)

	waitFor(t, "delivery succeeded after retry", 10*time.Second, func() bool {
		status, attempts := deliveryState(t, env.store, dID)
		return status == "succeeded" && attempts == 2
	})

	// Проходили через pending: в истории 2 попытки, вторая — 200.
	count, maxAtt := attemptRows(t, env.store, dID)
	if count != 2 || maxAtt != 2 {
		t.Fatalf("attempts rows=%d max=%d, want 2/2", count, maxAtt)
	}
	n, allOK := recv.stats()
	if n != 2 || !allOK {
		t.Fatalf("receiver got %d requests, allSigOK=%v", n, allOK)
	}
}

// TestE2ERetryExhaustedAndManualRetry: исчерпание retry-бюджета →
// dead; ручной retry через API → новый цикл → снова dead;
// история попыток сохраняется и нумерация продолжается.
func TestE2ERetryExhaustedAndManualRetry(t *testing.T) {
	env := newE2EEnv(t, domain.RetryPolicy{
		BaseDelay: 50 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxRetries: 1,
	})
	// MaxRetries=1 → максимум 2 HTTP-попытки за цикл.
	recv, recvSrv := newReceiver(t, func(n int) int {
		return http.StatusInternalServerError
	})
	env.setupEndpoint(t, recv, recvSrv.URL, "e2e-dead", "e2e.dead")

	eventID := sendEvent(t, env.api, &env.f, "e2e.dead", `{"fail":1}`)
	dID := deliveryID(t, env.store, eventID)

	// Первые 2 попытки → dead.
	waitFor(t, "delivery dead after exhausting retries", 10*time.Second, func() bool {
		status, attempts := deliveryState(t, env.store, dID)
		return status == "dead" && attempts == 2
	})

	// Ручной retry через API → pending, attempts сброшены.
	code, resp := apiCall(t, http.MethodPost,
		fmt.Sprintf("%s/api/v1/deliveries/%d/retry", env.api.URL, dID), "")
	if code != http.StatusOK {
		t.Fatalf("manual retry: %d %v", code, resp)
	}
	if st, att := deliveryState(t, env.store, dID); st != "pending" || att != 0 {
		t.Fatalf("after manual retry: status=%s attempts=%d, want pending/0", st, att)
	}

	// Новый цикл: ещё 2 попытки → dead; история — 4 строки,
	// нумерация продолжается (max attempt = 4).
	waitFor(t, "delivery dead again after new cycle", 10*time.Second, func() bool {
		status, attempts := deliveryState(t, env.store, dID)
		return status == "dead" && attempts == 2
	})
	count, maxAtt := attemptRows(t, env.store, dID)
	if count != 4 || maxAtt != 4 {
		t.Fatalf("attempt rows=%d max=%d, want 4/4 (history preserved)", count, maxAtt)
	}

	// Все 4 запроса имели корректную подпись — включая ретраи
	// после ручного retry (секрет тот же).
	n, allOK := recv.stats()
	if n != 4 || !allOK {
		t.Fatalf("receiver got %d requests, allSigOK=%v", n, allOK)
	}
}
