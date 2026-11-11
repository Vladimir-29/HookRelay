package httpapi

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateEventTypes(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []string
		wantErr bool
	}{
		{"valid dotted", []string{"order.created", "order.updated"}, []string{"order.created", "order.updated"}, false},
		{"empty list allowed", []string{}, []string{}, false},
		{"nil rejected by handler, but func returns empty", nil, []string{}, false},
		{"single", []string{"payment_failed"}, []string{"payment_failed"}, false},
		{"trims spaces", []string{"  order.created  "}, []string{"order.created"}, false},
		{"empty element", []string{"order.created", ""}, nil, true},
		{"blank element", []string{"   "}, nil, true},
		{"duplicate", []string{"a.b", "a.b"}, nil, true},
		{"duplicate after trim", []string{"a.b", " a.b "}, nil, true},
		{"starts with dot", []string{".order"}, nil, true},
		{"ends with dot", []string{"order."}, nil, true},
		{"invalid char space inside", []string{"order created"}, nil, true},
		{"invalid char slash", []string{"order/created"}, nil, true},
		{"unicode rejected", []string{"заказ.created"}, nil, true},
		{"too long", []string{strings.Repeat("a", maxEventTypeLen+1)}, nil, true},
		{"max length ok", []string{strings.Repeat("a", maxEventTypeLen)}, []string{strings.Repeat("a", maxEventTypeLen)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateEventTypes(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateEventTypes(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("validateEventTypes(%v) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("validateEventTypes(%v) = %v, want %v", tt.input, got, tt.want)
				}
			}
		})
	}
}

func TestValidateEventTypesTooMany(t *testing.T) {
	input := make([]string, maxEventTypes+1)
	for i := range input {
		input[i] = fmt.Sprintf("evt.%d", i)
	}
	if _, err := validateEventTypes(input); err == nil {
		t.Fatal("expected error for too many event types")
	}
}
