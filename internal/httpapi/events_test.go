package httpapi

import (
	"encoding/json"
	"testing"
)

func TestValidateEventRequest(t *testing.T) {
	tests := []struct {
		name    string
		input   createEventRequest
		wantErr string
	}{
		{"valid", createEventRequest{EventType: "order.created", Payload: json.RawMessage(`{"order_id":123}`)}, ""},
		{"valid empty object", createEventRequest{EventType: "a.b", Payload: json.RawMessage(`{}`)}, ""},
		{"type trimmed", createEventRequest{EventType: "  order.created  ", Payload: json.RawMessage(`{"x":1}`)}, ""},
		{"missing type", createEventRequest{Payload: json.RawMessage(`{"x":1}`)}, "event type must not be empty"},
		{"invalid type", createEventRequest{EventType: "bad type!", Payload: json.RawMessage(`{"x":1}`)}, "invalid event type"},
		{"missing payload", createEventRequest{EventType: "a.b"}, "payload is required"},
		{"null payload", createEventRequest{EventType: "a.b", Payload: json.RawMessage(`null`)}, "payload must be a JSON object"},
		{"array payload", createEventRequest{EventType: "a.b", Payload: json.RawMessage(`[1,2]`)}, "payload must be a JSON object"},
		{"string payload", createEventRequest{EventType: "a.b", Payload: json.RawMessage(`"x"`)}, "payload must be a JSON object"},
		{"number payload", createEventRequest{EventType: "a.b", Payload: json.RawMessage(`42`)}, "payload must be a JSON object"},
		{"broken payload", createEventRequest{EventType: "a.b", Payload: json.RawMessage(`{bad`)}, "payload must be a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			et, payload, err := validateEventRequest(tt.input)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if et == "" || len(payload) == 0 {
					t.Fatal("expected normalized type and payload")
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
