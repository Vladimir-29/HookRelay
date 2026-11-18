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
	"hookrelay/internal/secrets"
)

// maxJSONBody — лимит тела JSON-запросов.
const maxJSONBody = 1 << 20 // 1 MiB

// createEndpointRequest — DTO тела POST /api/v1/endpoints.
// Не смешивается с доменной моделью domain.Endpoint.
type createEndpointRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// createEndpointResponse — ответ POST /api/v1/endpoints.
// Секрет возвращается только здесь, один раз: GET/list его не содержат.
type createEndpointResponse struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret"`
	CreatedAt time.Time `json:"created_at"`
}

// Тело ограничено 1 MiB, лишние поля запрещены. На успех — 201
// с созданной записью и секретом подписи (base64url, 32 байта).
func createEndpointHandler(store *postgres.Store, box *secrets.Box) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createEndpointRequest
		if !decodeJSONBody(w, r, &req, maxJSONBody) {
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

		secret, err := newEndpointSecret(box)
		if err != nil {
			log.Printf("generate endpoint secret: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		ep, err := store.CreateEndpoint(ctx, name, normalizedURL, secret.ciphertext)
		if err != nil {
			log.Printf("insert endpoint: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusCreated, createEndpointResponse{
			ID:        ep.ID,
			Name:      ep.Name,
			URL:       ep.URL,
			Secret:    secret.encoded,
			CreatedAt: ep.CreatedAt,
		})
	}
}

// decodeJSONBody строго декодирует один JSON-объект в dst: тело ограничено
// maxBytes, неизвестные поля запрещены, второй объект или мусор — ошибка.
// При ошибке сам пишет ответ (413 или 400) и возвращает false.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		}
		return false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
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
