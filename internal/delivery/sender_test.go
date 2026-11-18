package delivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hookrelay/internal/domain"
	"hookrelay/internal/secrets"
)

// Общие тестовые секреты: box с нулевым ключом и подпись endpoint.
var (
	testBox = func() *secrets.Box {
		b, err := secrets.NewBox(make([]byte, 32))
		if err != nil {
			panic(err)
		}
		return b
	}()
	testRawSecret = []byte("0123456789abcdef0123456789abcdef")
	testEncrypted = func() []byte {
		ct, err := testBox.Seal(testRawSecret)
		if err != nil {
			panic(err)
		}
		return ct
	}()
)

// testSender — Sender с обычным транспортом для httptest-серверов
// (127.0.0.1 намеренно запрещён SSRF-фильтром боевого dialer'а).
func testSender(timeout time.Duration) *Sender {
	return &Sender{client: &http.Client{
		Timeout:   timeout,
		Transport: http.DefaultTransport,
	}, secrets: testBox}
}

func testDelivery() domain.ClaimedDelivery {
	return domain.ClaimedDelivery{
		ID:         42,
		EventID:    7,
		EndpointID: 3,
		EventType:  "order.created",
		Payload:    []byte(`{"order_id":123,"amount":100}`),
		TargetURL:  "", // заполняется в тесте
		// Зашифрованный тестовый секрет endpoint — Send подписывает запрос.
		SecretEncrypted: testEncrypted,
	}
}

func TestSendSuccessAndRequestFormat(t *testing.T) {
	var gotMethod, gotBody, gotContentType, gotDeliveryID, gotEventType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotContentType = r.Header.Get("Content-Type")
		gotDeliveryID = r.Header.Get("X-HookRelay-Delivery-ID")
		gotEventType = r.Header.Get("X-HookRelay-Event-Type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	d := testDelivery()
	d.TargetURL = srv.URL + "/hook"
	res := testSender(5*time.Second).Send(context.Background(), d)

	if !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	if res.StatusCode != 200 || res.Body != "ok" {
		t.Fatalf("status=%d body=%q", res.StatusCode, res.Body)
	}
	if res.Err != nil || res.Retryable {
		t.Fatalf("unexpected err/retryable: %+v", res)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q", gotMethod)
	}
	if gotBody != string(d.Payload) {
		t.Fatalf("body = %q, want raw payload %q", gotBody, d.Payload)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotDeliveryID != "42" || gotEventType != "order.created" {
		t.Fatalf("headers: delivery=%q event=%q", gotDeliveryID, gotEventType)
	}
}

func TestSendStatusClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantSucc  bool
		wantRetry bool
	}{
		{"200", 200, true, false},
		{"201", 201, true, false},
		{"204", 204, true, false},
		{"400", 400, false, false},
		{"403", 403, false, false},
		{"404", 404, false, false},
		{"408", 408, false, true},
		{"425", 425, false, true},
		{"429", 429, false, true},
		{"500", 500, false, true},
		{"503", 503, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			d := testDelivery()
			d.TargetURL = srv.URL
			res := testSender(5*time.Second).Send(context.Background(), d)
			if res.Success != tt.wantSucc || res.Retryable != tt.wantRetry {
				t.Fatalf("status %d: success=%v retryable=%v, want %v/%v",
					tt.status, res.Success, res.Retryable, tt.wantSucc, tt.wantRetry)
			}
			if res.Err != nil {
				t.Fatalf("unexpected err: %v", res.Err)
			}
		})
	}
}

func TestSendRedirectNotFollowed(t *testing.T) {
	redirected := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			redirected = true
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	defer srv.Close()

	// Клиент с той же политикой, что и боевой: не следовать редиректам.
	s := &Sender{client: &http.Client{
		Timeout:   5 * time.Second,
		Transport: http.DefaultTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, secrets: testBox}
	d := testDelivery()
	d.TargetURL = srv.URL
	res := s.Send(context.Background(), d)

	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 (initial response)", res.StatusCode)
	}
	if res.Success {
		t.Fatal("3xx must not count as success")
	}
	if redirected {
		t.Fatal("redirect was followed")
	}
}

func TestNewClientDoesNotFollowRedirects(t *testing.T) {
	s := New(5*time.Second, testBox)
	err := s.client.CheckRedirect(nil, nil)
	if err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect = %v, want ErrUseLastResponse", err)
	}
	tr, ok := s.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	if tr.Proxy != nil {
		t.Fatal("Proxy must be nil — env proxies bypass SSRF checks")
	}
}

func TestSendNetworkError(t *testing.T) {
	d := testDelivery()
	d.TargetURL = "http://127.0.0.1:1/unreachable"
	res := testSender(2*time.Second).Send(context.Background(), d)

	if res.Err == nil {
		t.Fatal("expected network error")
	}
	if !res.Retryable || res.Success || res.StatusCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSendTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := testDelivery()
	d.TargetURL = srv.URL
	res := testSender(50*time.Millisecond).Send(context.Background(), d)

	if res.Err == nil {
		t.Fatal("expected timeout error")
	}
	if !res.Retryable {
		t.Fatal("timeout must be retryable")
	}
}

func TestSendResponseBodyLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 10<<10))) // 10 KiB
	}))
	defer srv.Close()

	d := testDelivery()
	d.TargetURL = srv.URL
	res := testSender(5*time.Second).Send(context.Background(), d)

	if len(res.Body) > maxResponseBody {
		t.Fatalf("body len = %d, limit %d", len(res.Body), maxResponseBody)
	}
}

func TestSendSignatureHeader(t *testing.T) {
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotSig = r.Header.Get("X-HookRelay-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := testDelivery()
	d.TargetURL = srv.URL
	res := testSender(5*time.Second).Send(context.Background(), d)
	if !res.Success {
		t.Fatalf("send failed: %+v", res)
	}

	// Проверка на стороне получателя: HMAC-SHA256(secret, body).
	mac := hmac.New(sha256.New, testRawSecret)
	mac.Write([]byte(gotBody))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(gotSig), []byte(want)) {
		t.Fatalf("signature = %q, want %q", gotSig, want)
	}
}

func TestSendUsesEndpointSecret(t *testing.T) {
	// Другой секрет endpoint → другая подпись; подпись первого секрета
	// не должна подходить.
	otherSecret := []byte("fedcba9876543210fedcba9876543210")
	otherEnc, err := testBox.Seal(otherSecret)
	if err != nil {
		t.Fatal(err)
	}
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-HookRelay-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := testDelivery()
	d.TargetURL = srv.URL
	d.SecretEncrypted = otherEnc
	if res := testSender(5*time.Second).Send(context.Background(), d); !res.Success {
		t.Fatalf("send failed: %+v", res)
	}

	mac := hmac.New(sha256.New, otherSecret)
	mac.Write(d.Payload)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(gotSig), []byte(want)) {
		t.Fatalf("signature = %q, want %q (other endpoint secret)", gotSig, want)
	}
}

func TestSendNoSecretIsPermanent(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	for name, enc := range map[string][]byte{
		"missing (NULL)":   nil,
		"undecryptable":    []byte("garbage-not-a-box"),
		"wrong-key-format": {},
	} {
		t.Run(name, func(t *testing.T) {
			called = false
			d := testDelivery()
			d.TargetURL = srv.URL
			d.SecretEncrypted = enc
			res := testSender(5*time.Second).Send(context.Background(), d)
			if res.Success || res.Retryable {
				t.Fatalf("result = %+v, want permanent failure", res)
			}
			if res.Err == nil {
				t.Fatal("expected error")
			}
			if called {
				t.Fatal("request must not be sent without a valid secret")
			}
		})
	}
}
