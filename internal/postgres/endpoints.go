package postgres

import (
	"context"

	"hookrelay/internal/domain"
)

// CreateEndpoint вставляет endpoint и возвращает созданную запись
// через RETURNING (id и created_at генерирует PostgreSQL).
func (s *Store) CreateEndpoint(ctx context.Context, name, rawURL string) (domain.Endpoint, error) {
	var ep domain.Endpoint
	err := s.pool.QueryRow(ctx,
		`INSERT INTO endpoints (name, url) VALUES ($1, $2)
		 RETURNING id, name, url, created_at`,
		name, rawURL,
	).Scan(&ep.ID, &ep.Name, &ep.URL, &ep.CreatedAt)
	return ep, err
}
