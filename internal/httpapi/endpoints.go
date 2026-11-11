package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hookrelay/internal/postgres"
)

// createEndpointRequest — DTO тела POST /api/v1/endpoints.
// Не смешивается с доменной моделью domain.Endpoint.
type createEndpointRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Тело ограничено 1 MiB, лишние поля запрещены. На успех — 201 с созданной записью.
func createEndpointHandler(store *postgres.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createEndpointRequest
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			}
			return
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}

		name := strings.TrimSpace(req.Name)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		normalizedURL, err := validateURL(req.URL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		ep, err := store.CreateEndpoint(ctx, name, normalizedURL)
		if err != nil {
			log.Printf("insert endpoint: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusCreated, ep)
	}
}

// validateURL требует абсолютный URL со схемой http/https, потому что
// доставлять хуки будем через http.Client. Это НЕ защита от SSRF:
// блокировать private/loopback/metadata-IP нужно на момент доставки.
// Возвращает нормализованный URL (схема в нижнем регистре) для сохранения.
func validateURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("url is invalid")
	}
	if !u.IsAbs() || u.Hostname() == "" {
		return "", errors.New("url must be absolute")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("url scheme must be http or https")
	}
	if u.User != nil {
		return "", errors.New("url must not contain username or password")
	}
	return u.String(), nil
}
