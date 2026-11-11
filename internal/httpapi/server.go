//Package httpapi реализует HTTP-слой сервиса: маршруты, handlers,
// валидацию входа и DTO. Бизнес-логика и работа с БД — в других пакетах.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"hookrelay/internal/postgres"
)

// NewServer создаёт HTTP-сервер с маршрутами API. Порт передаётся
// без ведущего двоеточия.
func NewServer(port string, store *postgres.Store) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(store))
	mux.HandleFunc("GET /api/v1/endpoints", listEndpointsHandler(store))
	mux.HandleFunc("POST /api/v1/endpoints", createEndpointHandler(store))
	mux.HandleFunc("GET /api/v1/endpoints/{id}/subscriptions", getSubscriptionsHandler(store))
	mux.HandleFunc("PUT /api/v1/endpoints/{id}/subscriptions", replaceSubscriptionsHandler(store))
	mux.HandleFunc("POST /api/v1/events", createEventHandler(store))

	return &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// healthHandler — readiness-проба: пингует БД. Таймаут 2с — бюджет пробы.
// Возвращает 200/503 и JSON {status:"ok"|"unavailable"}.
func healthHandler(store *postgres.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := store.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// writeJSON сериализует v в JSON и пишет в w с указанным статусом.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
