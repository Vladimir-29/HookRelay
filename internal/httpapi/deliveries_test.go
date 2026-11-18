package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"hookrelay/internal/postgres"
)

func testDeliveriesStore(t *testing.T) *postgres.Store {
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

// deadDeliveryFixture — endpoint + event + dead-доставка с attempts=6.
func deadDeliveryFixture(t *testing.T, store *postgres.Store) int64 {
	t.Helper()
	ctx := t.Context()

	var endpointID, eventID, deliveryID int64
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO endpoints (name, url) VALUES ($1, $2) RETURNING id`,
		"retrytest-"+t.Name(), "https://example.com/hook",
	).Scan(&endpointID); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO events (event_type, payload) VALUES ('retry.test', '{}'::jsonb) RETURNING id`,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO deliveries (event_id, endpoint_id, status, attempts, available_at,
			last_error, locked_by, locked_until)
		 VALUES ($1, $2, 'dead', 6, now() - interval '1 hour', 'HTTP 500',
			'old-worker', now() + interval '1 hour') RETURNING id`,
		eventID, endpointID,
	).Scan(&deliveryID); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = store.Pool().Exec(c, `DELETE FROM deliveries WHERE id = $1`, deliveryID)
		_, _ = store.Pool().Exec(c, `DELETE FROM events WHERE id = $1`, eventID)
		_, _ = store.Pool().Exec(c, `DELETE FROM endpoints WHERE id = $1`, endpointID)
	})
	return deliveryID
}

func doRetry(t *testing.T, store *postgres.Store, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/deliveries/"+id+"/retry", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	retryDeliveryHandler(store).ServeHTTP(rec, req)
	return rec
}

func TestRetryDeliveryHandlerSuccess(t *testing.T) {
	store := testDeliveriesStore(t)
	id := deadDeliveryFixture(t, store)

	rec := doRetry(t, store, strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var d struct {
		ID       int64  `json:"id"`
		Status   string `json:"status"`
		Attempts int    `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.ID != id || d.Status != "pending" || d.Attempts != 0 {
		t.Fatalf("unexpected response: %+v", d)
	}

	got, err := store.GetDelivery(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "pending" || got.Attempts != 0 || got.LastError != nil {
		t.Fatalf("db state: %+v", got)
	}
	if time.Since(got.AvailableAt) > 5*time.Second {
		t.Fatalf("available_at = %v, want ~now (worker picks up immediately)", got.AvailableAt)
	}
}

func TestRetryDeliveryHandlerNotDead(t *testing.T) {
	store := testDeliveriesStore(t)
	ctx := t.Context()

	var endpointID, eventID, deliveryID int64
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO endpoints (name, url) VALUES ($1, $2) RETURNING id`,
		"retry409-"+t.Name(), "https://example.com/hook",
	).Scan(&endpointID); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO events (event_type, payload) VALUES ('retry.test', '{}'::jsonb) RETURNING id`,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO deliveries (event_id, endpoint_id, status) VALUES ($1, $2, 'pending') RETURNING id`,
		eventID, endpointID,
	).Scan(&deliveryID); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = store.Pool().Exec(c, `DELETE FROM deliveries WHERE id = $1`, deliveryID)
		_, _ = store.Pool().Exec(c, `DELETE FROM events WHERE id = $1`, eventID)
		_, _ = store.Pool().Exec(c, `DELETE FROM endpoints WHERE id = $1`, endpointID)
	})

	rec := doRetry(t, store, strconv.FormatInt(deliveryID, 10))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRetryDeliveryHandlerNotFound(t *testing.T) {
	store := testDeliveriesStore(t)
	rec := doRetry(t, store, "999999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRetryDeliveryHandlerBadID(t *testing.T) {
	store := testDeliveriesStore(t)
	rec := doRetry(t, store, "abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
