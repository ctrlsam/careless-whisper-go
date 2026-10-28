package runner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

// deliveringMessenger acknowledges every silent message with a delivery
// receipt, so the runner produces RTT reports and analysis snapshots.
type deliveringMessenger struct {
	mu         sync.Mutex
	counter    int64
	onDelivery messenger.DeliveryFunc
	registered bool
}

func (d *deliveringMessenger) Start(context.Context) error          { return nil }
func (d *deliveringMessenger) WaitUntilReady(context.Context) error { return nil }

func (d *deliveringMessenger) SendSilentMessage(_ context.Context, _, _ string, _ messenger.SendOptions) (string, error) {
	id := "msg-" + itoa(atomic.AddInt64(&d.counter, 1))
	d.mu.Lock()
	deliver := d.onDelivery
	d.mu.Unlock()
	if deliver != nil {
		go deliver(id)
	}
	return id, nil
}

func (d *deliveringMessenger) IsOnPlatform(context.Context, string) (bool, bool, error) {
	return d.registered, true, nil
}

func (d *deliveringMessenger) Stop(context.Context) error { return nil }
func (d *deliveringMessenger) SetReceiptTarget(string)    {}

func (d *deliveringMessenger) SetDeliveryCallback(fn messenger.DeliveryFunc) {
	d.mu.Lock()
	d.onDelivery = fn
	d.mu.Unlock()
}

func (d *deliveringMessenger) SetReceiptCallback(messenger.ReceiptFunc)         {}
func (d *deliveringMessenger) SetSentMessageCallback(messenger.SentMessageFunc) {}

var _ messenger.Messenger = (*deliveringMessenger)(nil)

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

type recordingObserver struct {
	mu        sync.Mutex
	reports   int
	analyses  int
	lastTotal int
}

func (r *recordingObserver) OnReport(model.ReceiptReport) {
	r.mu.Lock()
	r.reports++
	r.mu.Unlock()
}

func (r *recordingObserver) OnAnalysis(analysis model.Analysis) {
	r.mu.Lock()
	r.analyses++
	r.lastTotal = analysis.TotalRequests
	r.mu.Unlock()
}

func (r *recordingObserver) Info(string) {}
func (r *recordingObserver) Warn(string) {}

func TestRunProducesReportsAndAnalysis(t *testing.T) {
	m := &deliveringMessenger{registered: true}
	observer := &recordingObserver{}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := Run(ctx, m, Settings{
		PhoneNumber:       "123456789",
		ConcurrentWorkers: 3,
		DelayBetween:      5 * time.Millisecond,
	}, nil, observer)
	if err != nil && err != context.DeadlineExceeded {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.reports == 0 {
		t.Fatal("expected at least one RTT report")
	}
	if observer.analyses == 0 {
		t.Fatal("expected at least one analysis snapshot")
	}
	if observer.lastTotal == 0 {
		t.Fatal("expected analysis to count tracked requests")
	}
}

func TestRunRefusesUnregisteredTarget(t *testing.T) {
	m := &deliveringMessenger{registered: false}
	err := Run(context.Background(), m, Settings{PhoneNumber: "123"}, nil, nil)
	if err == nil {
		t.Fatal("expected refusal for an unregistered target")
	}
}

// sessionLostMessenger is registered but every probe reports a lost session.
type sessionLostMessenger struct{ deliveringMessenger }

func (s *sessionLostMessenger) SendSilentMessage(context.Context, string, string, messenger.SendOptions) (string, error) {
	return "", messenger.ErrSessionLost
}

func TestRunStopsOnSessionLost(t *testing.T) {
	m := &sessionLostMessenger{deliveringMessenger{registered: true}}
	done := make(chan error, 1)
	go func() {
		// No timeout: only a fatal stop can end this run.
		done <- Run(context.Background(), m, Settings{
			PhoneNumber: "123", ConcurrentWorkers: 4, DelayBetween: time.Millisecond,
		}, nil, nil)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, messenger.ErrSessionLost) {
			t.Fatalf("expected ErrSessionLost, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the session was lost (hot-spin?)")
	}
}

func TestEffectivePingInterval(t *testing.T) {
	settings := Settings{DelayBetween: time.Second, ConcurrentWorkers: 5}
	if got := settings.effectivePingInterval(); got != 200*time.Millisecond {
		t.Fatalf("expected 200ms effective interval, got %v", got)
	}
}
