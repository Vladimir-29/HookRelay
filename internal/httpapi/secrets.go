package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"hookrelay/internal/postgres"
	"hookrelay/internal/secrets"
)

// endpointSecretPair — свежий секрет endpoint: ciphertext для БД
// и base64url-представление для ответа клиенту (один раз).
type endpointSecretPair struct {
	ciphertext []byte
	encoded    string
}

// newEndpointSecret генерирует 32-байтный секрет и шифрует его
// для хранения. Открытый секрет не сохраняется — только ciphertext
// и его base64url-кодировка для ответа.
func newEndpointSecret(box *secrets.Box) (endpointSecretPair, error) {
	raw, err := secrets.Generate()
	if err != nil {
		return endpointSecretPair{}, err
	}
	defer clear(raw)
	ct, err := box.Seal(raw)
	if err != nil {
		return endpointSecretPair{}, err
	}
	return endpointSecretPair{
		ciphertext: ct,
		encoded:    base64.RawURLEncoding.EncodeToString(raw),
	}, nil
}

// rotateSecretResponse — ответ POST /api/v1/endpoints/{id}/rotate-secret.
type rotateSecretResponse struct {
	ID     int64  `json:"id"`
	Secret string `json:"secret"`
}

// POST /api/v1/endpoints/{id}/rotate-secret — перевыпуск секрета
// подписи. Новый секрет возвращается только в этом ответе.
// Работает и для endpoints, созданных до появления секретов (NULL).
// 404 — endpoint не найден.
func rotateSecretHandler(store *postgres.Store, box *secrets.Box) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		if _, err := store.GetEndpoint(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "endpoint not found"})
				return
			}
			log.Printf("get endpoint: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		secret, err := newEndpointSecret(box)
		if err != nil {
			log.Printf("generate endpoint secret: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if err := store.RotateEndpointSecret(ctx, id, secret.ciphertext); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "endpoint not found"})
				return
			}
			log.Printf("rotate endpoint secret: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, rotateSecretResponse{ID: id, Secret: secret.encoded})
	}
}
