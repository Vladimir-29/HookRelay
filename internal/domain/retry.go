package domain

import (
	"math"
	"math/rand/v2"
	"time"
)

// RetryPolicy — параметры exponential backoff для повторных доставок.
// MaxRetries — число автоматических повторов ПОСЛЕ первой попытки:
// при MaxRetries=5 всего выполняется до 6 HTTP-попыток.
type RetryPolicy struct {
	BaseDelay  time.Duration // задержка перед первым повтором (WORKER_RETRY_DELAY)
	MaxDelay   time.Duration // верхняя граница задержки (WORKER_MAX_RETRY_DELAY)
	MaxRetries int           // автоматические повторы (WORKER_MAX_RETRIES)
}

// maxBackoffExp — предел степени двойки в backoff: 2^62 уже далеко
// за любым разумным MaxDelay, дальше степень не имеет смысла.
const maxBackoffExp = 62

// Delay возвращает задержку перед следующей попыткой после
// completed завершённых HTTP-попыток: completed=1 → BaseDelay,
// далее удвоение на каждую попытку, jitter ±20%, потолок MaxDelay.
// При невалидной политике (BaseDelay <= 0) возвращает 0.
func (p RetryPolicy) Delay(completed int) time.Duration {
	if completed < 1 {
		completed = 1
	}
	if p.BaseDelay <= 0 {
		return 0
	}

	exp := completed - 1
	if exp > maxBackoffExp {
		exp = maxBackoffExp
	}
	d := float64(p.BaseDelay) * math.Pow(2, float64(exp))

	// Jitter ±20%, чтобы задачи после массового сбоя не повторялись
	// одновременно.
	d *= 0.8 + 0.4*rand.Float64()

	if p.MaxDelay > 0 && d > float64(p.MaxDelay) {
		d = float64(p.MaxDelay)
	}
	if d < 0 {
		return 0
	}
	return time.Duration(d)
}

// Validate проверяет осмысленность политики.
// При MaxRetries > 0 базовая задержка обязана быть положительной;
// при MaxRetries = 0 повторы отключены и нулевая BaseDelay допустима.
func (p RetryPolicy) Validate() error {
	if p.MaxRetries < 0 {
		return errNegative("WORKER_MAX_RETRIES")
	}
	if p.BaseDelay < 0 {
		return errNegative("WORKER_RETRY_DELAY")
	}
	if p.MaxDelay < 0 {
		return errNegative("WORKER_MAX_RETRY_DELAY")
	}
	if p.MaxRetries > 0 && p.BaseDelay <= 0 {
		return errInvalid("WORKER_RETRY_DELAY must be positive when WORKER_MAX_RETRIES > 0")
	}
	if p.BaseDelay > 0 && p.MaxDelay < p.BaseDelay {
		return errInvalid("WORKER_MAX_RETRY_DELAY must be >= WORKER_RETRY_DELAY")
	}
	return nil
}

type policyError string

func (e policyError) Error() string { return string(e) }

func errNegative(name string) error {
	return policyError(name + " must not be negative")
}

func errInvalid(msg string) error { return policyError(msg) }
