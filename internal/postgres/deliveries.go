package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"hookrelay/internal/domain"
)

// ClaimDeliveries атомарно захватывает до batchSize доступных доставок
// для workerID и возвращает их с данными для отправки вебхука.
//
// Доступны: pending с available_at <= now() и processing с истёкшим
// lease (locked_until <= now() или NULL — например, после падения Worker).
// Захват: status=processing, locked_by=workerID, locked_until=now()+lease.
// attempts не увеличивается — это счётчик HTTP-попыток, а не захватов.
//
// FOR UPDATE SKIP LOCKED гарантирует, что параллельные Worker'ы
// получают непересекающиеся наборы задач.
func (s *Store) ClaimDeliveries(ctx context.Context, workerID string, batchSize int, lease time.Duration) ([]domain.ClaimedDelivery, error) {
	if strings.TrimSpace(workerID) == "" {
		return nil, errors.New("worker id must not be empty")
	}
	if batchSize <= 0 {
		return nil, fmt.Errorf("batch size must be positive, got %d", batchSize)
	}
	if lease <= 0 {
		return nil, fmt.Errorf("lease must be positive, got %s", lease)
	}

	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
			SELECT id
			FROM deliveries
			WHERE (status = 'pending' AND available_at <= now())
			   OR (status = 'processing'
			       AND (locked_until IS NULL OR locked_until <= now()))
			ORDER BY id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE deliveries d
		SET status       = 'processing',
		    locked_by    = $1,
		    locked_until = now() + ($2 * interval '1 second'),
		    updated_at   = now()
		FROM candidates c, events e, endpoints ep
		WHERE d.id = c.id
		  AND e.id = d.event_id
		  AND ep.id = d.endpoint_id
		RETURNING d.id, d.event_id, d.endpoint_id, d.attempts, d.locked_until,
		          e.event_type, e.payload, ep.url`,
		workerID, lease.Seconds(), batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	deliveries := []domain.ClaimedDelivery{}
	for rows.Next() {
		var d domain.ClaimedDelivery
		var payload []byte
		if err := rows.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.Attempts,
			&d.LeaseUntil, &d.EventType, &payload, &d.TargetURL); err != nil {
			return nil, err
		}
		d.Payload = payload
		deliveries = append(deliveries, d)
	}
	return deliveries, rows.Err()
}

// maxDiagnosticLen — предел для текстов диагностики (last_error, error
// в delivery_attempts), чтобы не раздувать БД.
const maxDiagnosticLen = 1024

// SaveDeliveryResult сохраняет результат одной HTTP-попытки и обновляет
// доставку в одной транзакции:
//   - Success (2xx)         → status='succeeded'
//   - Retryable             → status='pending', available_at = now()+retryDelay
//   - иначе                 → status='dead'
//
// attempts увеличивается ровно на 1, запись добавляется в
// delivery_attempts, lease-поля очищаются.
//
// Обновление выполняется только если доставка всё ещё в processing
// и locked_by совпадает с workerID — устаревший Worker (lease истёк,
// задачу перехватил другой) не перезапишет чужой результат.
// В этом случае возвращается pgx.ErrNoRows.
func (s *Store) SaveDeliveryResult(ctx context.Context, workerID string, deliveryID int64,
	res domain.DeliveryResult, retryDelay time.Duration) error {

	status := "dead"
	switch {
	case res.Success:
		status = "succeeded"
	case res.Retryable:
		status = "pending"
	}

	var lastErr any
	if res.Err != nil {
		lastErr = truncate(res.Err.Error(), maxDiagnosticLen)
	} else if !res.Success {
		lastErr = fmt.Sprintf("HTTP %d", res.StatusCode)
	}

	var statusCode any
	if res.StatusCode != 0 {
		statusCode = res.StatusCode
	}
	var body any
	if res.Body != "" {
		body = res.Body
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var attempts int
	err = tx.QueryRow(ctx, `
		UPDATE deliveries SET
			status       = $3,
			attempts     = attempts + 1,
			available_at = CASE WHEN $3 = 'pending'
			                THEN now() + ($4 * interval '1 second')
			                ELSE available_at END,
			locked_by    = NULL,
			locked_until = NULL,
			last_error   = $5,
			updated_at   = now()
		WHERE id = $1 AND status = 'processing' AND locked_by = $2
		RETURNING attempts`,
		deliveryID, workerID, status, retryDelay.Seconds(), lastErr,
	).Scan(&attempts)
	if err != nil {
		return err // pgx.ErrNoRows — задача перехвачена другим worker'ом
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO delivery_attempts
			(delivery_id, attempt, status_code, response_body, duration_ms, error)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		deliveryID, attempts, statusCode, body,
		int(res.Duration.Milliseconds()), lastErr,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
