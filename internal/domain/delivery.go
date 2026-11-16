package domain

import (
	"encoding/json"
	"time"
)

// ClaimedDelivery — доставка, захваченная Worker'ом для отправки вебхука.
// LeaseUntil — момент, после которого задачу может забрать другой Worker,
// если текущий не завершил её обработку.
type ClaimedDelivery struct {
	ID         int64
	EventID    int64
	EndpointID int64
	EventType  string
	Payload    json.RawMessage
	TargetURL  string
	Attempts   int
	LeaseUntil time.Time
}

// DeliveryResult — результат одной HTTP-попытки доставки.
// Success — только HTTP 2xx; Retryable — сетевые ошибки, таймауты
// и статусы 408/425/429/5xx, доставку можно повторить позже.
type DeliveryResult struct {
	StatusCode int
	Body       string
	Duration   time.Duration
	Success    bool
	Retryable  bool
	Err        error
}
