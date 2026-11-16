// Package delivery выполняет одну HTTP-попытку доставки вебхука.
// Не содержит retry, подписи и очередей — это следующий этап Worker'а.
package delivery

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"hookrelay/internal/domain"
)

// maxResponseBody — сколько тела ответа читаем. Получатель не должен
// заставить нас читать неограниченный объём.
const maxResponseBody = 4 << 10 // 4 KiB

// Sender выполняет HTTP POST с JSON-payload события.
// Содержит переиспользуемый http.Client — соединения Keep-Alive.
type Sender struct {
	client *http.Client
}

// CloseIdleConnections закрывает неактивные keep-alive соединения.
// Вызывается при остановке Worker.
func (s *Sender) CloseIdleConnections() {
	s.client.CloseIdleConnections()
}

// New создаёт Sender с SSRF-защитой: соединение устанавливается
// только к IP-адресам, прошедшим проверку (без proxy из env).
func New(timeout time.Duration) *Sender {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &Sender{
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

// Send выполняет одну попытку доставки. Тело запроса — исходный
// JSON payload без повторной сериализации.
func (s *Sender) Send(ctx context.Context, d domain.ClaimedDelivery) domain.DeliveryResult {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TargetURL,
		bytes.NewReader(d.Payload))
	if err != nil {
		return domain.DeliveryResult{Duration: time.Since(start), Retryable: true, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-HookRelay-Delivery-ID", strconv.FormatInt(d.ID, 10))
	req.Header.Set("X-HookRelay-Event-Type", d.EventType)

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
