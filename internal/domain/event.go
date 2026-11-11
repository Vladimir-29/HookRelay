package domain

import (
	"encoding/json"
	"time"
)

// Event — принятое событие, которое нужно доставить подписанным endpoints.
type Event struct {
	ID        int64           `json:"id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}
