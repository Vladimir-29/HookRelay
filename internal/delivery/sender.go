// Package delivery выполняет одну HTTP-попытку доставки вебхука:
// POST JSON-payload с HMAC-SHA256 подписью секрета endpoint.
// Не содержит retry и очередей — это следующий этап Worker'а.
package delivery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"hookrelay/internal/domain"
	"hookrelay/internal/secrets"
)

// maxResponseBody — сколько тела ответа читаем. Получатель не должен
// заставить нас читать неограниченный объём.
const maxResponseBody = 4 << 10 // 4 KiB

// Sender выполняет HTTP POST с JSON-payload события и HMAC-SHA256
// подписью X-HookRelay-Signature. Содержит переиспользуемый http.Client
// — соединения Keep-Alive.
type Sender struct {
	client  *http.Client
	secrets *secrets.Box
}

// CloseIdleConnections закрывает неактивные keep-alive соединения.
// Вызывается при остановке Worker.
func (s *Sender) CloseIdleConnections() {
	s.client.CloseIdleConnections()
}

// New создаёт Sender с SSRF-защитой: соединение устанавливается
// только к IP-адресам, прошедшим проверку (без proxy из env).
// secrets — Box для расшифровки секрета endpoint; без него подпись
// невозможна, поэтому nil не допускается.
func New(timeout time.Duration, box *secrets.Box) *Sender {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &Sender{
		secrets: box,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:                 nil, // не использовать proxy из окружения
				DialContext:           safeDialContext(dialer),
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
			// Не следовать редиректам: возвращаем исходный 3xx ответ.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// NewWithClient создаёт Sender с уже готовым http.Client.
// Предназначен для тестов и нестандартных транспортов (например,
// httptest-серверы на loopback, которые блокирует SSRF-dialer).
// Production-код должен использовать New — там SSRF-защита.
func NewWithClient(client *http.Client, box *secrets.Box) *Sender {
	return &Sender{client: client, secrets: box}
}

// Send выполняет одну попытку доставки. Тело запроса — исходный
// JSON payload без повторной сериализации; подпись HMAC-SHA256
// вычисляется по тем же байтам, что уходят в HTTP body.
//
// Без валидного секрета endpoint запрос не отправляется: результат
// постоянный (Retryable=false) — повтор не поможет, нужен rotate-secret.
func (s *Sender) Send(ctx context.Context, d domain.ClaimedDelivery) domain.DeliveryResult {
	start := time.Now()

	secret, err := s.endpointSecret(d.SecretEncrypted)
	if err != nil {
		return domain.DeliveryResult{
			Duration: time.Since(start),
			Err:      fmt.Errorf("endpoint secret: %w", err),
		}
	}
	defer clear(secret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TargetURL,
		bytes.NewReader(d.Payload))
	if err != nil {
		return domain.DeliveryResult{Duration: time.Since(start), Retryable: true, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-HookRelay-Delivery-ID", strconv.FormatInt(d.ID, 10))
	req.Header.Set("X-HookRelay-Event-Type", d.EventType)
	req.Header.Set("X-HookRelay-Signature", "sha256="+signPayload(secret, d.Payload))

	resp, err := s.client.Do(req)
	if err != nil {
		return domain.DeliveryResult{Duration: time.Since(start), Retryable: true, Err: err}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if readErr != nil {
		return domain.DeliveryResult{Duration: time.Since(start), Retryable: true, Err: readErr}
	}

	res := domain.DeliveryResult{
		StatusCode: resp.StatusCode,
		Body:       string(body),
		Duration:   time.Since(start),
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Success = true
	case resp.StatusCode == http.StatusRequestTimeout ||
		resp.StatusCode == http.StatusTooEarly ||
		resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode >= 500:
		res.Retryable = true
	}
	return res
}

// endpointSecret расшифровывает секрет endpoint из claim-данных.
// Ошибка — секрета нет (NULL в БД) или он не расшифровывается
// (неверный мастер-ключ, повреждённые данные).
func (s *Sender) endpointSecret(encrypted []byte) ([]byte, error) {
	if s.secrets == nil || len(encrypted) == 0 {
		return nil, fmt.Errorf("endpoint has no signing secret")
	}
	return s.secrets.Open(encrypted)
}

// signPayload вычисляет HMAC-SHA256 подпись точных байтов тела
// запроса и возвращает её hex-представление.
func signPayload(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
