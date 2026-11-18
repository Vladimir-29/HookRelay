package delivery

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestIsForbiddenIP(t *testing.T) {
	tests := []struct {
		ip      string
		wantBad bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // link-local + metadata
		{"169.254.1.1", true},     // link-local
		{"0.0.0.0", true},
		{"::", true},
		{"224.0.0.1", true},        // multicast
		{"ff02::1", true},          // multicast v6
		{"::ffff:127.0.0.1", true}, // v4-mapped loopback
		{"::ffff:10.0.0.1", true},  // v4-mapped private
		{"fd00::1", true},          // ULA
		{"fe80::1", true},          // link-local v6
		{"100.100.100.200", true},  // Alibaba metadata
		{"8.8.8.8", false},
		{"93.184.216.34", false}, // example.com
		{"2606:4700:4700::1111", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			got := isForbiddenIP(net.ParseIP(tt.ip))
			if got != tt.wantBad {
				t.Fatalf("isForbiddenIP(%s) = %v, want %v", tt.ip, got, tt.wantBad)
			}
		})
	}
}

// stubResolver подменяет DNS-резолвер на время теста.
func stubResolver(t *testing.T, fn func(ctx context.Context, host string) ([]net.IPAddr, error)) {
	t.Helper()
	old := lookupIPAddr
	lookupIPAddr = fn
	t.Cleanup(func() { lookupIPAddr = old })
}

func TestSafeDialContextRejectsPrivateLiteral(t *testing.T) {
	dialer := &net.Dialer{Timeout: time.Second}
	dial := safeDialContext(dialer)

	for _, addr := range []string{"127.0.0.1:8080", "10.1.2.3:443", "[::1]:80", "169.254.169.254:80"} {
		if _, err := dial(context.Background(), "tcp", addr); err == nil ||
			!strings.Contains(err.Error(), "ssrf") {
			t.Fatalf("dial %s: err = %v, want ssrf error", addr, err)
		}
	}
}

func TestSafeDialContextRejectsForbiddenDNSAnswer(t *testing.T) {
	stubResolver(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
	})
	dial := safeDialContext(&net.Dialer{Timeout: time.Second})

	_, err := dial(context.Background(), "tcp", "internal.example:443")
	if err == nil || !strings.Contains(err.Error(), "ssrf") {
		t.Fatalf("err = %v, want ssrf error", err)
	}
}

func TestSafeDialContextMixedDNSAnswersRejected(t *testing.T) {
	// Один публичный + один приватный адрес — всё равно отказ.
	stubResolver(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("93.184.216.34")},
			{IP: net.ParseIP("192.168.0.1")},
		}, nil
	})
	dial := safeDialContext(&net.Dialer{Timeout: time.Second})

	_, err := dial(context.Background(), "tcp", "mixed.example:443")
	if err == nil || !strings.Contains(err.Error(), "ssrf") {
		t.Fatalf("err = %v, want ssrf error", err)
	}
}

func TestSafeDialContextResolvesOnce(t *testing.T) {
	calls := 0
	stubResolver(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}}, nil // TEST-NET-1, недоступен
	})
	dial := safeDialContext(&net.Dialer{Timeout: 300 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// Адрес валиден, dial к 192.0.2.1 завершится ошибкой/таймаутом —
	// важно, что DNS вызван ровно один раз и соединение шло на IP.
	_, _ = dial(ctx, "tcp", "public.example:80")

	if calls != 1 {
		t.Fatalf("DNS lookups = %d, want exactly 1 (no re-resolution)", calls)
	}
}

func TestSendBlockedBySSRF(t *testing.T) {
	// Боевой клиент New() + подменённый DNS: имя резолвится в loopback.
	stubResolver(t, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	})
	s := New(2*time.Second, testBox)

	d := testDelivery()
	d.TargetURL = "http://internal.example/hook"
	res := s.Send(context.Background(), d)

	if res.Success || res.StatusCode != 0 || res.Err == nil {
		t.Fatalf("expected blocked request, got %+v", res)
	}
	if !res.Retryable {
		t.Fatal("network-level failure must be retryable")
	}
}
