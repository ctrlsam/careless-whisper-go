package flood

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
)

type fakeMessenger struct {
	sent         int64
	failUntil    int64
	sessionLost  bool
	registered   bool
	known        bool
	lastReaction atomic.Value
	lastNoAck    atomic.Bool
}

func (f *fakeMessenger) Start(context.Context) error          { return nil }
func (f *fakeMessenger) WaitUntilReady(context.Context) error { return nil }

func (f *fakeMessenger) SendSilentMessage(_ context.Context, _, reaction string, opts messenger.SendOptions) (string, error) {
	if f.sessionLost {
		return "", messenger.ErrSessionLost
	}
	n := atomic.AddInt64(&f.sent, 1)
	if n <= f.failUntil {
		return "", errors.New("transient failure")
	}
	f.lastReaction.Store(reaction)
	f.lastNoAck.Store(opts.NoAck)
	return "msg", nil
}

func (f *fakeMessenger) IsOnPlatform(context.Context, string) (bool, bool, error) {
	return f.registered, f.known, nil
}

func (f *fakeMessenger) Stop(context.Context) error                       { return nil }
func (f *fakeMessenger) SetReceiptTarget(string)                          {}
func (f *fakeMessenger) SetDeliveryCallback(messenger.DeliveryFunc)       {}
func (f *fakeMessenger) SetReceiptCallback(messenger.ReceiptFunc)         {}
func (f *fakeMessenger) SetSentMessageCallback(messenger.SentMessageFunc) {}

var _ messenger.Messenger = (*fakeMessenger)(nil)

func TestRunStopsAtMaxMessages(t *testing.T) {
	m := &fakeMessenger{registered: true, known: true}
	stats, err := Run(context.Background(), m, Settings{
		PhoneNumber:       "123",
		ConcurrentWorkers: 4,
		MaxMessages:       25,
		PayloadSize:       64,
	}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if stats.Sent < 25 {
		t.Fatalf("expected at least 25 sent, got %d", stats.Sent)
	}
	// With four workers the overshoot past the limit is bounded.
	if stats.Sent > 25+4 {
		t.Fatalf("expected the worker pool to stop near the limit, got %d", stats.Sent)
	}
	if got := m.lastReaction.Load(); got != nil && len(got.(string)) != 64 {
		t.Fatalf("expected 64-byte payload, got %d bytes", len(got.(string)))
	}
}

func TestRunPropagatesNoAck(t *testing.T) {
	m := &fakeMessenger{registered: true, known: true}
	if _, err := Run(context.Background(), m, Settings{
		PhoneNumber:       "123",
		ConcurrentWorkers: 1,
		MaxMessages:       1,
		PayloadSize:       8,
		NoAck:             true,
	}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !m.lastNoAck.Load() {
		t.Fatalf("expected NoAck to reach the messenger")
	}
}

func TestRunCountsFailures(t *testing.T) {
	m := &fakeMessenger{registered: true, known: true, failUntil: 5}
	stats, err := Run(context.Background(), m, Settings{
		PhoneNumber:       "123",
		ConcurrentWorkers: 1,
		MaxMessages:       10,
		PayloadSize:       8,
	}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if stats.Failed != 5 {
		t.Fatalf("expected 5 failures, got %d", stats.Failed)
	}
	if stats.Sent != 10 {
		t.Fatalf("expected 10 successful sends, got %d", stats.Sent)
	}
}

func TestRunRefusesUnregisteredTarget(t *testing.T) {
	m := &fakeMessenger{registered: false, known: true}
	_, err := Run(context.Background(), m, Settings{
		PhoneNumber: "123",
		MaxMessages: 1,
		PayloadSize: 8,
	}, nil)
	if !errors.Is(err, messenger.ErrUnregistered) {
		t.Fatalf("expected ErrUnregistered, got %v", err)
	}
	if atomic.LoadInt64(&m.sent) != 0 {
		t.Fatalf("expected no messages sent to an unregistered target")
	}
}

func TestRunStopsOnDuration(t *testing.T) {
	m := &fakeMessenger{registered: true, known: true}
	start := time.Now()
	stats, err := Run(context.Background(), m, Settings{
		PhoneNumber:       "123",
		ConcurrentWorkers: 2,
		Duration:          50 * time.Millisecond,
		DelayBetween:      10 * time.Millisecond,
		PayloadSize:       8,
	}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("duration stop took too long")
	}
	if stats.Sent == 0 {
		t.Fatalf("expected some messages before the duration elapsed")
	}
}

func TestBuildPayloadRejectsNonPositive(t *testing.T) {
	if _, err := BuildPayload(0); err == nil {
		t.Fatal("expected error for zero payload size")
	}
}

func TestEstimatedGiB(t *testing.T) {
	stats := Stats{Sent: 1024}
	if got := stats.EstimatedGiB(1 << 20); got != 1 {
		t.Fatalf("expected 1 GiB estimate, got %v", got)
	}
}

func TestBuildPayloadIsNotConstant(t *testing.T) {
	got, err := BuildPayload(4096)
	if err != nil {
		t.Fatal(err)
	}
	distinct := map[byte]struct{}{}
	for i := 0; i < len(got); i++ {
		distinct[got[i]] = struct{}{}
	}
	if len(distinct) < 2 {
		t.Fatalf("payload is not random: only %d distinct bytes", len(distinct))
	}
}

func TestStatsAckedReflectsNoAck(t *testing.T) {
	for _, noAck := range []bool{false, true} {
		m := &fakeMessenger{registered: true, known: true}
		stats, err := Run(context.Background(), m, Settings{
			PhoneNumber: "123", ConcurrentWorkers: 1, MaxMessages: 1, PayloadSize: 8, NoAck: noAck,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Acked == noAck {
			t.Fatalf("NoAck=%v: expected Acked=%v, got %v", noAck, !noAck, stats.Acked)
		}
	}
}

func TestRunStopsOnSessionLost(t *testing.T) {
	m := &fakeMessenger{registered: true, known: true, sessionLost: true}
	done := make(chan struct{})
	var stats Stats
	var err error
	go func() {
		// No MaxMessages or Duration: only a fatal stop can end this run.
		stats, err = Run(context.Background(), m, Settings{
			PhoneNumber: "123", ConcurrentWorkers: 4, PayloadSize: 8,
		}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the session was lost (hot-spin?)")
	}
	if !errors.Is(err, messenger.ErrSessionLost) {
		t.Fatalf("expected ErrSessionLost, got %v", err)
	}
	// Each worker hits the fatal error at most once before stopping, so the
	// failure count stays bounded rather than spinning into the millions.
	if stats.Failed > 4 {
		t.Fatalf("expected a bounded failure count, got %d", stats.Failed)
	}
}

type captureReporter struct{ warnings []string }

func (c *captureReporter) Progress(Stats, int) {}
func (c *captureReporter) Warn(m string)       { c.warnings = append(c.warnings, m) }
func (c *captureReporter) Info(string)         {}

func TestRunWarnsOnOversizedPayload(t *testing.T) {
	m := &fakeMessenger{registered: true, known: true}
	r := &captureReporter{}
	if _, err := Run(context.Background(), m, Settings{
		PhoneNumber: "123", ConcurrentWorkers: 1, MaxMessages: 1,
		PayloadSize: likelyRejectedPayloadBytes + 1,
	}, r); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range r.warnings {
		if strings.Contains(w, "oversized reactions") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an oversized-payload warning, got %v", r.warnings)
	}
}
