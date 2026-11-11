package postgres

import (
	"context"

	"hookrelay/internal/domain"
)

// CreateEvent сохраняет событие и создаёт доставки всем endpoints,
// подписанным на его event_type — всё в одной транзакции.
// При ошибке на любом шаге событие не сохраняется (rollback).
// Возвращает созданное событие и количество созданных доставок
// (0, если подписчиков нет).
func (s *Store) CreateEvent(ctx context.Context, eventType string, payload []byte) (domain.Event, int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Event{}, 0, err
	}
	defer tx.Rollback(ctx)

	var ev domain.Event
	err = tx.QueryRow(ctx,
		`INSERT INTO events (event_type, payload) VALUES ($1, $2::jsonb)
		 RETURNING id, event_type, payload, created_at`,
		eventType, string(payload),
	).Scan(&ev.ID, &ev.EventType, &ev.Payload, &ev.CreatedAt)
	if err != nil {
		return domain.Event{}, 0, err
	}

	rows, err := tx.Query(ctx,
		`SELECT endpoint_id FROM endpoint_subscriptions WHERE event_type = $1`,
		eventType)
	if err != nil {
		return domain.Event{}, 0, err
	}
	var endpointIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return domain.Event{}, 0, err
		}
		endpointIDs = append(endpointIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return domain.Event{}, 0, err
	}

	if len(endpointIDs) > 0 {
		_, err = tx.Exec(ctx,
			`INSERT INTO deliveries (event_id, endpoint_id)
			 SELECT $1, unnest($2::bigint[])`,
			ev.ID, endpointIDs)
		if err != nil {
			return domain.Event{}, 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Event{}, 0, err
	}
	return ev, len(endpointIDs), nil
}
