package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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
//   - Retryable, attempts+1 <= policy.MaxRetries → status='pending',
//     available_at = now()+policy.Delay(completed)
//   - Retryable, повторов нет → status='dead'
//   - иначе (4xx и пр.)     → status='dead'
//
// Лимит повторов проверяется в SQL по фактическому значению attempts
// (атомарно с инкрементом): newAttempts <= MaxRetries означает, что
// после этой попытки ещё разрешён автоматический повтор. При
// MaxRetries=5 временная ошибка на 6-й попытке → dead.
//
// attempts увеличивается ровно на 1, запись добавляется в
// delivery_attempts, lease-поля очищаются.
//
// Обновление выполняется только если доставка всё ещё в processing,
// locked_by совпадает с workerID и attempts равен значению из claim —
// иначе рассчитанная задержка может не соответствовать счётчику,
// и UPDATE не меняет запись (pgx.ErrNoRows).
func (s *Store) SaveDeliveryResult(ctx context.Context, workerID string, d domain.ClaimedDelivery,
	res domain.DeliveryResult, policy domain.RetryPolicy) error {

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

	// Задержка перед следующим повтором: после d.Attempts+1 завершённых
	// попыток. Значение согласовано с attempts+1 в SQL — под lease
	// счётчик не может измениться между claim и save.
	var retryDelaySecs float64
	if res.Retryable && !res.Success {
		retryDelaySecs = policy.Delay(d.Attempts + 1).Seconds()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var attempts int
	err = tx.QueryRow(ctx, `
		UPDATE deliveries SET
			status = CASE
				WHEN $3 THEN 'succeeded'
				WHEN NOT $4 THEN 'dead'
				WHEN attempts + 1 <= $5 THEN 'pending'
				ELSE 'dead' END,
			attempts     = attempts + 1,
			available_at = CASE
				WHEN NOT $3 AND $4 AND attempts + 1 <= $5
				THEN now() + ($6 * interval '1 second')
				ELSE available_at END,
			locked_by    = NULL,
			locked_until = NULL,
			last_error   = $7,
			updated_at   = now()
		WHERE id = $1 AND status = 'processing' AND locked_by = $2
		  AND attempts = $8
		RETURNING attempts`,
		d.ID, workerID, res.Success, res.Retryable, policy.MaxRetries,
		retryDelaySecs, lastErr, d.Attempts,
	).Scan(&attempts)
	if err != nil {
		return err // pgx.ErrNoRows — задача перехвачена другим worker'ом
	}

	// Номер попытки в истории — сквозной по delivery_attempts:
	// deliveries.attempts сбрасывается при ручном retry, а история
	// должна оставаться однозначной → MAX(attempt)+1.
	if _, err := tx.Exec(ctx, `
		INSERT INTO delivery_attempts
			(delivery_id, attempt, status_code, response_body, duration_ms, error)
		SELECT $1, COALESCE(MAX(attempt), 0) + 1, $2, $3, $4, $5
		FROM delivery_attempts WHERE delivery_id = $1`,
		d.ID, statusCode, body,
		int(res.Duration.Milliseconds()), lastErr,
	); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// scanDelivery читает строку deliveries в domain.Delivery.
func scanDelivery(row interface{ Scan(...any) error }) (domain.Delivery, error) {
	var d domain.Delivery
	var lastErr pgtype.Text
	err := row.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.Status, &d.Attempts,
		&d.AvailableAt, &lastErr, &d.CreatedAt, &d.UpdatedAt)
	if lastErr.Valid {
		s := lastErr.String
		d.LastError = &s
	}
	return d, err
}

const deliveryColumns = `id, event_id, endpoint_id, status, attempts,
	available_at, last_error, created_at, updated_at`

// GetDelivery возвращает доставку по id или pgx.ErrNoRows.
func (s *Store) GetDelivery(ctx context.Context, id int64) (domain.Delivery, error) {
	return scanDelivery(s.pool.QueryRow(ctx,
		`SELECT `+deliveryColumns+` FROM deliveries WHERE id = $1`, id))
}

// RetryDelivery атомарно переводит dead-доставку обратно в pending:
// available_at=now() (Worker заберёт сразу), attempts=0 — новый цикл
// автоматических повторов, lease-поля и last_error очищаются.
// История delivery_attempts не трогается.
// Если доставка не dead (или не существует) — pgx.ErrNoRows.
func (s *Store) RetryDelivery(ctx context.Context, id int64) (domain.Delivery, error) {
	return scanDelivery(s.pool.QueryRow(ctx, `
		UPDATE deliveries SET
			status       = 'pending',
			attempts     = 0,
			available_at = now(),
			locked_by    = NULL,
			locked_until = NULL,
			last_error   = NULL,
			updated_at   = now()
		WHERE id = $1 AND status = 'dead'
		RETURNING `+deliveryColumns, id))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
