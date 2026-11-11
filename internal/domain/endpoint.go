// Package domain содержит сущности и бизнес-правила сервиса,
// не зависящие от транспорта (HTTP) и хранилища (PostgreSQL).
package domain

import "time"

// Endpoint — webhook endpoint, куда доставляются события.
type Endpoint struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}
