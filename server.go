package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newServer создаёт HTTP-сервер с маршрутом /health. Порт передаётся
// без ведущего двоеточия.
func newServer(port string, pool *pgxpool.Pool) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(pool))

	return &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// healthHandler — readiness-проба: пингует БД. Таймаут 2с — бюджет пробы.
// Возвращает 200/503 и JSON {status:"ok"|"unavailable"}.
func healthHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
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
