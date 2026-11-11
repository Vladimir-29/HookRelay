package httpapi

import "testing"

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid http", "http://example.com/hook", false},
		{"valid https", "https://example.com/hook", false},
		{"path and query", "https://example.com/webhooks?id=1&sig=abc", false},
		{"empty", "", true},
		{"relative", "/webhooks/endpoint", true},
		{"scheme only", "https://", true},
		{"unsupported scheme", "ftp://example.com/file", true},
		{"no hostname", "http:///path", true},
		{"with credentials", "https://user:pass@example.com/hook", true},
		{"surrounding spaces", "  https://example.com/hook  ", false},
		{"uppercase scheme", "HTTPS://example.com/hook", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateURL(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateURL(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got == "" {
				t.Fatalf("validateURL(%q) returned empty normalized URL", tt.input)
			}
		})
	}
}

func TestValidateURLNormalizesScheme(t *testing.T) {
	got, err := validateURL("HTTPS://Example.COM/hook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://Example.COM/hook" {
		t.Fatalf("normalized URL = %q", got)
	}
}
