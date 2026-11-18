package domain

import (
	"testing"
	"time"
)

func TestRetryPolicyDelay(t *testing.T) {
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxRetries: 5}

	// Первая повторная задержка: base * 2^0 = 1s, jitter ±20% → [0.8s, 1.2s].
	for i := 0; i < 200; i++ {
		d := p.Delay(1)
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("Delay(1) = %v, want [0.8s, 1.2s]", d)
		}
	}
}

func TestRetryPolicyDelayExponential(t *testing.T) {
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Hour, MaxRetries: 10}

	// Средние значения по многим вызовам должны расти ×2:
	// Delay(2)≈2s, Delay(3)≈4s, Delay(4)≈8s (jitter в среднем нейтрален).
	avg := func(completed int) time.Duration {
		const n = 500
		var sum time.Duration
		for i := 0; i < n; i++ {
			sum += p.Delay(completed)
		}
		return sum / n
	}
	d2, d3, d4 := avg(2), avg(3), avg(4)
	if d2 < 1700*time.Millisecond || d2 > 2300*time.Millisecond {
		t.Fatalf("avg Delay(2) = %v, want ~2s", d2)
	}
	if d3 < 3400*time.Millisecond || d3 > 4600*time.Millisecond {
		t.Fatalf("avg Delay(3) = %v, want ~4s", d3)
	}
	if d4 < 6800*time.Millisecond || d4 > 9200*time.Millisecond {
		t.Fatalf("avg Delay(4) = %v, want ~8s", d4)
	}
}

func TestRetryPolicyDelayMaxCap(t *testing.T) {
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: 10 * time.Second, MaxRetries: 100}

	// Даже при огромной степени двойки задержка не превышает MaxDelay.
	for _, completed := range []int{10, 20, 50, 1000, 1_000_000} {
		if d := p.Delay(completed); d > p.MaxDelay {
			t.Fatalf("Delay(%d) = %v > MaxDelay %v", completed, d, p.MaxDelay)
		}
	}
}

func TestRetryPolicyDelayBoundsWithJitter(t *testing.T) {
	p := RetryPolicy{BaseDelay: 5 * time.Second, MaxDelay: 5 * time.Minute, MaxRetries: 5}

	// Все значения — в пределах [0.8*base_delay, MaxDelay].
	for i := 0; i < 1000; i++ {
		d := p.Delay(3) // ожидаемая задержка 5s*2^2=20s → jitter [16s,24s]
		if d < 16*time.Second || d > 24*time.Second {
			t.Fatalf("Delay(3) = %v outside jitter bounds", d)
		}
	}
}

func TestRetryPolicyDelayEdgeCases(t *testing.T) {
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxRetries: 5}

	if d := p.Delay(0); d <= 0 {
		t.Fatalf("Delay(0) = %v, want positive (treated as 1)", d)
	}
	if d := p.Delay(-5); d <= 0 {
		t.Fatalf("Delay(-5) = %v, want positive", d)
	}

	// Нулевая базовая задержка → 0 (отключённые повторы).
	zero := RetryPolicy{BaseDelay: 0, MaxDelay: time.Minute, MaxRetries: 5}
	if d := zero.Delay(3); d != 0 {
		t.Fatalf("Delay with zero base = %v, want 0", d)
	}
}

func TestRetryPolicyValidate(t *testing.T) {
	ok := RetryPolicy{BaseDelay: 5 * time.Second, MaxDelay: 5 * time.Minute, MaxRetries: 5}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}

	for name, p := range map[string]RetryPolicy{
		"negative retries":     {BaseDelay: time.Second, MaxDelay: time.Minute, MaxRetries: -1},
		"negative base":        {BaseDelay: -time.Second, MaxDelay: time.Minute, MaxRetries: 1},
		"negative max":         {BaseDelay: time.Second, MaxDelay: -time.Minute, MaxRetries: 1},
		"max < base":           {BaseDelay: time.Minute, MaxDelay: time.Second, MaxRetries: 1},
		"zero base w/ retries": {BaseDelay: 0, MaxDelay: time.Minute, MaxRetries: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := p.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	// MaxRetries=0 — повторы отключены, нулевая BaseDelay допустима.
	off := RetryPolicy{BaseDelay: 0, MaxDelay: 0, MaxRetries: 0}
	if err := off.Validate(); err != nil {
		t.Fatalf("retries disabled policy rejected: %v", err)
	}
}

func TestRetryPolicyDelayNoOverflow(t *testing.T) {
	// Экстремальные значения не должны переполнять time.Duration.
	p := RetryPolicy{BaseDelay: time.Hour, MaxDelay: 100 * 365 * 24 * time.Hour, MaxRetries: 100}
	for _, completed := range []int{1, 100, 1000000} {
		d := p.Delay(completed)
		if d < 0 || d > p.MaxDelay {
			t.Fatalf("Delay(%d) = %v out of range", completed, d)
		}
	}
}

func TestRetryExhaustionSemantics(t *testing.T) {
	// MaxRetries=5 → повтор разрешён после попыток 1..5,
	// после 6-й временной ошибки — dead. Проверяем сам счётчик:
	// newAttempts = oldAttempts + 1 <= MaxRetries.
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Minute, MaxRetries: 5}
	for attempt := 1; attempt <= 6; attempt++ {
		newAttempts := attempt // attempts после инкремента
		canRetry := newAttempts <= p.MaxRetries
		if attempt <= 5 && !canRetry {
			t.Fatalf("attempt %d: expected retry allowed", attempt)
		}
		if attempt == 6 && canRetry {
			t.Fatal("attempt 6: expected retries exhausted")
		}
	}
}
