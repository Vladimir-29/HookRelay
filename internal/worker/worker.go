// Package worker реализует цикл обработки доставок: захват задач
// через postgres.Store.ClaimDeliveries, отправка через Sender
// и сохранение результата через Store.SaveDeliveryResult.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"hookrelay/internal/domain"
	"hookrelay/internal/postgres"
)

// Sender — то, что нужно Worker'у от HTTP-отправителя.
// *delivery.Sender удовлетворяет интерфейсу; в тестах подставляется fake.
type Sender interface {
	Send(ctx context.Context, d domain.ClaimedDelivery) domain.DeliveryResult
	CloseIdleConnections()
}

// Config — настройки Worker'а.
type Config struct {
	WorkerID     string        // пусто → генерируется уникальный
	BatchSize    int           // размер пакета ClaimDeliveries
	PollInterval time.Duration // пауза между опросами, когда задач нет
	Lease        time.Duration // длительность lease при захвате
	RetryDelay   time.Duration // задержка перед повторной попыткой
}

// Worker периодически забирает и обрабатывает доставки.
type Worker struct {
	store  *postgres.Store
	sender Sender
	cfg    Config
}

// New создаёт Worker. При пустом WorkerID генерируется уникальный
// идентификатор вида "host-pid-nanotime".
func New(store *postgres.Store, sender Sender, cfg Config) *Worker {
	if cfg.WorkerID == "" {
		cfg.WorkerID = newWorkerID()
	}
	return &Worker{store: store, sender: sender, cfg: cfg}
}

func newWorkerID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().UnixNano())
}

// Run крутит цикл до отмены ctx: захват пакета → параллельная обработка
// (не более BatchSize горутин) → повтор. При остановке новые задачи
// не захватываются, текущие HTTP-запросы отменяются по ctx;
// незавершённые доставки восстановит lease-механизм.
func (w *Worker) Run(ctx context.Context) {
	defer w.sender.CloseIdleConnections()
	log.Printf("worker %s started (batch=%d poll=%s lease=%s)",
		w.cfg.WorkerID, w.cfg.BatchSize, w.cfg.PollInterval, w.cfg.Lease)

	for {
		if ctx.Err() != nil {
			return
		}
		claimed, err := w.store.ClaimDeliveries(ctx, w.cfg.WorkerID,
			w.cfg.BatchSize, w.cfg.Lease)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Ошибка PostgreSQL — не busy loop: ждём интервал опроса.
			log.Printf("worker %s: claim deliveries: %v", w.cfg.WorkerID, err)
			if !wait(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		if len(claimed) == 0 {
			if !wait(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		w.processBatch(ctx, claimed)
	}
}

// processBatch обрабатывает пакет параллельно: по одной горутине
// на доставку, их число ограничено размером пакета.
func (w *Worker) processBatch(ctx context.Context, deliveries []domain.ClaimedDelivery) {
	var wg sync.WaitGroup
	for _, d := range deliveries {
		wg.Add(1)
		go func(d domain.ClaimedDelivery) {
			defer wg.Done()
			res := w.sender.Send(ctx, d)
			err := w.store.SaveDeliveryResult(ctx, w.cfg.WorkerID, d.ID, res, w.cfg.RetryDelay)
			switch {
			case err == nil:
			case errors.Is(err, pgx.ErrNoRows):
				// Lease истёк и задачу перехватил другой Worker —
				// его результат не перезаписываем.
				log.Printf("worker %s: delivery %d claim is stale, result skipped",
					w.cfg.WorkerID, d.ID)
			default:
				log.Printf("worker %s: save result for delivery %d: %v",
					w.cfg.WorkerID, d.ID, err)
			}
		}(d)
	}
	wg.Wait()
}

// wait спит d или возвращает false при отмене ctx.
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
