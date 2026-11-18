package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"hookrelay/internal/postgres"
	"hookrelay/internal/secrets"
)

func testSecretsBox(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

func createEndpointViaAPI(t *testing.T, handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints",
		strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCreateEndpointReturnsSecretOnce(t *testing.T) {
	store := testSecretsStore(t)
	box := testSecretsBox(t)
	handler := createEndpointHandler(store, box)

	rec := createEndpointViaAPI(t, handler,
		`{"name":"wh","url":"https://example.com/hook"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	secret, ok := resp["secret"].(string)
	if !ok || secret == "" {
		t.Fatalf("response has no secret: %s", rec.Body.String())
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != 32 {
		t.Fatalf("secret is not base64url 32-byte value: %q err=%v", secret, err)
	}

	// GET list не должен содержать секрет.
	listRec := httptest.NewRecorder()
	listEndpointsHandler(store).ServeHTTP(listRec,
		httptest.NewRequest(http.MethodGet, "/api/v1/endpoints", nil))
	if bytes.Contains(listRec.Body.Bytes(), []byte("secret")) {
		t.Fatalf("list response leaks secret: %s", listRec.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, ep := range list {
		if _, has := ep["secret"]; has {
			t.Fatalf("endpoint object contains secret key: %v", ep)
		}
	}
}

func TestRotateSecret(t *testing.T) {
	store := testSecretsStore(t)
	box := testSecretsBox(t)

	// Endpoint без секрета — как созданный до миграции 006.
	var endpointID int64
	if err := store.Pool().QueryRow(context.Background(),
		`INSERT INTO endpoints (name, url) VALUES ($1, $2) RETURNING id`,
		"legacy-"+t.Name(), "https://example.com/legacy").Scan(&endpointID); err != nil {
		t.Fatalf("insert legacy endpoint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(),
			`DELETE FROM endpoints WHERE id = $1`, endpointID)
	})

	handler := rotateSecretHandler(store, box)
	doRotate := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/endpoints/"+strconv.FormatInt(endpointID, 10)+"/rotate-secret", nil)
		req.SetPathValue("id", strconv.FormatInt(endpointID, 10))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// Первый rotate для legacy endpoint (secret_encrypted IS NULL).
	rec1 := doRotate()
	if rec1.Code != http.StatusOK {
		t.Fatalf("first rotate: status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	var r1 rotateSecretResponse
	if err := json.Unmarshal(rec1.Body.Bytes(), &r1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r1.Secret == "" {
		t.Fatal("rotate response has no secret")
	}

	// Повторный rotate → другой секрет.
	rec2 := doRotate()
	if rec2.Code != http.StatusOK {
		t.Fatalf("second rotate: status=%d", rec2.Code)
	}
	var r2 rotateSecretResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &r2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r2.Secret == r1.Secret {
		t.Fatal("rotate must generate a new secret")
	}

	// Несуществующий endpoint → 404.
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/endpoints/999999/rotate-secret", nil)
	req.SetPathValue("id", "999999")
	rec404 := httptest.NewRecorder()
	handler.ServeHTTP(rec404, req)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("missing endpoint: status=%d, want 404", rec404.Code)
	}
}

// testSecretsStore — тот же helper, что в deliveries_test.go.
func testSecretsStore(t *testing.T) *postgres.Store {
	t.Helper()
	return testDeliveriesStore(t)
}
