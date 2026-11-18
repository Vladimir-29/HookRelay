// Package postgres предоставляет реализацию хранилища в PostgreSQL.
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"hookrelay/internal/domain"
)

// endpointColumns — поля endpoints без secret_encrypted: секрет не должен
// попадать в публичные структуры API и обычные SELECT-ответы.
const endpointColumns = `id, name, url, created_at`

func scanEndpoint(row interface{ Scan(...any) error }) (domain.Endpoint, error) {
	var ep domain.Endpoint
	err := row.Scan(&ep.ID, &ep.Name, &ep.URL, &ep.CreatedAt)
	return ep, err
}

// CreateEndpoint вставляет endpoint с зашифрованным секретом
// (secretEncrypted — nonce||ciphertext AES-256-GCM) и возвращает
// созданную запись через RETURNING (id и created_at генерирует PostgreSQL).
func (s *Store) CreateEndpoint(ctx context.Context, name, rawURL string, secretEncrypted []byte) (domain.Endpoint, error) {
	return scanEndpoint(s.pool.QueryRow(ctx,
		`INSERT INTO endpoints (name, url, secret_encrypted) VALUES ($1, $2, $3)
		 RETURNING `+endpointColumns,
		name, rawURL, secretEncrypted,
	))
}

// RotateEndpointSecret заменяет зашифрованный секрет endpoint.
// Работает и для endpoints без секрета (NULL → новое значение).
// pgx.ErrNoRows, если endpoint не существует.
func (s *Store) RotateEndpointSecret(ctx context.Context, id int64, secretEncrypted []byte) error {
	var existing int64
	err := s.pool.QueryRow(ctx,
		`UPDATE endpoints SET secret_encrypted = $2
		 WHERE id = $1 RETURNING id`,
		id, secretEncrypted,
	).Scan(&existing)
	if err != nil {
		if err == pgx.ErrNoRows {
			return pgx.ErrNoRows
		}
		return err
	}
	return nil
}
