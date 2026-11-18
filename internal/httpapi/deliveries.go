package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"hookrelay/internal/postgres"
)

// POST /api/v1/deliveries/{id}/retry — ручной повтор dead-доставки:
// dead → pending, Worker заберёт её сразу. 404 — нет такой доставки,
// 409 — статус не dead.
func retryDeliveryHandler(store *postgres.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		d, err := store.GetDelivery(ctx, id)
		if err != nil {
			writeDeliveryError(w, err, "get delivery")
			return
		}
		if d.Status != "dead" {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "delivery is not dead",
			})
			return
		}

		d, err = store.RetryDelivery(ctx, id)
		if err != nil {
			// Гонка: статус успел измениться между проверкой и UPDATE.
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "delivery is not dead",
				})
				return
			}
			writeDeliveryError(w, err, "retry delivery")
			return
		}
		writeJSON(w, http.StatusOK, d)
	}
}

// writeDeliveryError отображает pgx.ErrNoRows как 404, прочие ошибки — как 500.
func writeDeliveryError(w http.ResponseWriter, err error, op string) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "delivery not found"})
		return
	}
	log.Printf("%s: %v", op, err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
