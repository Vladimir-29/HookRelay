package domain

import (
	"encoding/json"
	"time"
)

// Delivery — доставка события endpoint'у в очереди.
type Delivery struct {
	ID          int64     `json:"id"`
	EventID     int64     `json:"event_id"`
	EndpointID  int64     `json:"endpoint_id"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	AvailableAt time.Time `json:"available_at"`
	LastError   *string   `json:"last_error"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

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
