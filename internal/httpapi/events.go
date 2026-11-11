package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"hookrelay/internal/postgres"
)

// createEventRequest — DTO тела POST /api/v1/events.
type createEventRequest struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

// createEventResponse — ответ 202 POST /api/v1/events.
type createEventResponse struct {
	ID            int64     `json:"id"`
	EventType     string    `json:"event_type"`
	DeliveryCount int       `json:"delivery_count"`
	CreatedAt     time.Time `json:"created_at"`
}

// POST /api/v1/events — принимает событие и ставит доставки подписчикам.
// Ответ 202: событие принято, обработки (Worker) ещё не было.
func createEventHandler(store *postgres.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createEventRequest
		if !decodeJSONBody(w, r, &req, maxJSONBody) {
			return
		}

		eventType, payload, err := validateEventRequest(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		ev, deliveryCount, err := store.CreateEvent(ctx, eventType, payload)
		if err != nil {
			log.Printf("create event: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusAccepted, createEventResponse{
			ID:            ev.ID,
			EventType:     ev.EventType,
			DeliveryCount: deliveryCount,
			CreatedAt:     ev.CreatedAt,
		})
	}
}

// validateEventRequest проверяет тип события и payload.
// Возвращает нормализованный тип и сырые байты payload для jsonb.
func validateEventRequest(req createEventRequest) (string, []byte, error) {
	eventType, err := validateEventType(req.EventType)
	if err != nil {
		return "", nil, err
	}
	payload, err := validatePayload(req.Payload)
	if err != nil {
		return "", nil, err
	}
	return eventType, payload, nil
}

// validatePayload требует JSON-объект и возвращает его канонические байты.
func validatePayload(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("payload is required")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errors.New("payload must be a JSON object")
	}
	return raw, nil
}
