package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"hookrelay/internal/domain"
)

// ListEndpoints возвращает все endpoints, отсортированные по id.
// При пустой таблице возвращает пустой срез, а не nil.
func (s *Store) ListEndpoints(ctx context.Context) ([]domain.Endpoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, url, created_at FROM endpoints ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	eps := []domain.Endpoint{}
	for rows.Next() {
		var ep domain.Endpoint
		if err := rows.Scan(&ep.ID, &ep.Name, &ep.URL, &ep.CreatedAt); err != nil {
			return nil, err
		}
		eps = append(eps, ep)
	}
	return eps, rows.Err()
}

// GetEndpoint возвращает endpoint по id или pgx.ErrNoRows, если его нет.
func (s *Store) GetEndpoint(ctx context.Context, id int64) (domain.Endpoint, error) {
	var ep domain.Endpoint
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, url, created_at FROM endpoints WHERE id = $1`, id,
	).Scan(&ep.ID, &ep.Name, &ep.URL, &ep.CreatedAt)
	return ep, err
}

// ListSubscriptions возвращает типы событий endpoint, отсортированные по имени.
// При отсутствии подписок возвращает пустой срез, а не nil.
func (s *Store) ListSubscriptions(ctx context.Context, endpointID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT event_type FROM endpoint_subscriptions
		 WHERE endpoint_id = $1 ORDER BY event_type`, endpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	types := []string{}
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			return nil, err
		}
		types = append(types, et)
	}
	return types, rows.Err()
}

// ReplaceSubscriptions полностью заменяет подписки endpoint на eventTypes
// в одной транзакции: при любой ошибке прежний список сохраняется.
// Возвращает pgx.ErrNoRows, если endpoint не существует.
func (s *Store) ReplaceSubscriptions(ctx context.Context, endpointID int64, eventTypes []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM endpoints WHERE id = $1)`, endpointID,
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return pgx.ErrNoRows
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM endpoint_subscriptions WHERE endpoint_id = $1`, endpointID,
	); err != nil {
		return err
	}
	for _, et := range eventTypes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO endpoint_subscriptions (endpoint_id, event_type) VALUES ($1, $2)`,
			endpointID, et,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
