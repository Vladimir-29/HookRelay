package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"hookrelay/internal/postgres"
)

const (
	// maxSubscriptionsBody — лимит тела PUT /subscriptions: список event_types
	// до 100 коротких строк не может превысить 64 KiB.
	maxSubscriptionsBody = 64 << 10
	maxEventTypes        = 100
	maxEventTypeLen      = 200
)

// eventTypePattern — допустимые символы типа события: буквы, цифры,
// точка, дефис, подчёркивание; не может начинаться/заканчиваться
// разделителем. Примеры: "order.created", "payment_failed".
var eventTypePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

type subscriptionsRequest struct {
	EventTypes []string `json:"event_types"`
}

type subscriptionsResponse struct {
	EventTypes []string `json:"event_types"`
}

// GET /api/v1/endpoints — список всех endpoints.
func listEndpointsHandler(store *postgres.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		eps, err := store.ListEndpoints(ctx)
		if err != nil {
			log.Printf("list endpoints: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, eps)
	}
}

// GET /api/v1/endpoints/{id}/subscriptions — список типов событий endpoint.
func getSubscriptionsHandler(store *postgres.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseEndpointID(w, r)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		if _, err := store.GetEndpoint(ctx, id); err != nil {
			writeEndpointLookupError(w, err, "get endpoint")
			return
		}
		types, err := store.ListSubscriptions(ctx, id)
		if err != nil {
			log.Printf("list subscriptions: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, subscriptionsResponse{EventTypes: types})
	}
}

// PUT /api/v1/endpoints/{id}/subscriptions — полная замена подписок endpoint.
// Пустой массив удаляет все подписки.
func replaceSubscriptionsHandler(store *postgres.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseEndpointID(w, r)
		if !ok {
			return
		}

		var req subscriptionsRequest
		if !decodeJSONBody(w, r, &req, maxSubscriptionsBody) {
			return
		}
		if req.EventTypes == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event_types is required"})
			return
		}
		types, err := validateEventTypes(req.EventTypes)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		if err := store.ReplaceSubscriptions(ctx, id, types); err != nil {
			writeEndpointLookupError(w, err, "replace subscriptions")
			return
		}
		writeJSON(w, http.StatusOK, subscriptionsResponse{EventTypes: types})
	}
}

// parseEndpointID извлекает и проверяет id из path. При ошибке пишет 400.
func parseEndpointID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid endpoint id"})
		return 0, false
	}
	return id, true
}

// writeEndpointLookupError отображает pgx.ErrNoRows как 404, прочие ошибки — как 500.
func writeEndpointLookupError(w http.ResponseWriter, err error, op string) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "endpoint not found"})
		return
	}
	log.Printf("%s: %v", op, err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

// validateEventTypes проверяет список типов событий и возвращает
// нормализованный (обрезанный от пробелов) список. Пустой список допустим.
func validateEventTypes(raw []string) ([]string, error) {
	if len(raw) > maxEventTypes {
		return nil, errors.New("too many event types")
	}
	types := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, s := range raw {
		et, err := validateEventType(s)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[et]; dup {
			return nil, errors.New("duplicate event type")
		}
		seen[et] = struct{}{}
		types = append(types, et)
	}
	return types, nil
}

// validateEventType проверяет один тип события и возвращает его
// нормализованным (обрезанным от пробелов).
func validateEventType(s string) (string, error) {
	et := strings.TrimSpace(s)
	if et == "" {
		return "", errors.New("event type must not be empty")
	}
	if len(et) > maxEventTypeLen || !eventTypePattern.MatchString(et) {
		return "", errors.New("invalid event type")
	}
	return et, nil
}
