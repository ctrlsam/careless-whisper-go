package whatsapp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
)

type poolFake struct {
	inFlight int32
	maxSeen  int32
	sent     int64
}

func (f *poolFake) Start(context.Context) error          { return nil }
func (f *poolFake) WaitUntilReady(context.Context) error { return nil }

func (f *poolFake) SendSilentMessage(_ context.Context, _, _ string, _ messenger.SendOptions) (string, error) {
	n := atomic.AddInt32(&f.inFlight, 1)
	for {
		seen := atomic.LoadInt32(&f.maxSeen)
		if n <= seen || atomic.CompareAndSwapInt32(&f.maxSeen, seen, n) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	atomic.AddInt32(&f.inFlight, -1)
	atomic.AddInt64(&f.sent, 1)
	return "msg", nil
}

func (f *poolFake) IsOnPlatform(context.Context, string) (bool, bool, error) { return true, true, nil }
func (f *poolFake) Stop(context.Context) error                               { return nil }
func (f *poolFake) SetReceiptTarget(string)                                  {}
func (f *poolFake) SetDeliveryCallback(messenger.DeliveryFunc)               {}
func (f *poolFake) SetReceiptCallback(messenger.ReceiptFunc)                 {}
func (f *poolFake) SetSentMessageCallback(messenger.SentMessageFunc)         {}

func TestDevicePath(t *testing.T) {
	cases := map[string]string{
		"careless-whisper.db": "careless-whisper-2.db",
		"store":               "store-2",
		"a/b/store.sqlite":    "a/b/store-2.sqlite",
	}
	for base, want := range cases {
		if got := devicePath(base, 2); got != want {
			t.Fatalf("devicePath(%q, 2) = %q, want %q", base, got, want)
		}
	}
}

func TestPoolNeverUsesAClientConcurrently(t *testing.T) {
	fakes := []*poolFake{{}, {}, {}}
	clients := make([]messenger.Messenger, len(fakes))
	for i := range fakes {
		clients[i] = fakes[i]
	}
	p := newPoolFromMessengers(clients)
	for _, c := range clients {
		p.ready <- c
	}
	p.started = true

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.SendSilentMessage(context.Background(), "123", "x", messenger.SendOptions{}); err != nil {
				t.Errorf("send: %v", err)
			}
		}()
	}
	wg.Wait()

	var total int64
	for _, f := range fakes {
		if f.maxSeen > 1 {
			t.Fatalf("a client saw %d concurrent sends, want at most 1", f.maxSeen)
		}
		total += f.sent
	}
	if total != 50 {
		t.Fatalf("expected 50 sends across the pool, got %d", total)
	}
}

func TestPoolDistributesAcrossDevices(t *testing.T) {
	fakes := []*poolFake{{}, {}, {}}
	clients := make([]messenger.Messenger, len(fakes))
	for i := range fakes {
		clients[i] = fakes[i]
	}
	p := newPoolFromMessengers(clients)
	for _, c := range clients {
		p.ready <- c
	}
	p.started = true

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.SendSilentMessage(context.Background(), "123", "x", messenger.SendOptions{})
		}()
	}
	wg.Wait()

	used := 0
	for _, f := range fakes {
		if f.sent > 0 {
			used++
		}
	}
	if used < 2 {
		t.Fatalf("expected work spread across devices, only %d used", used)
	}
}
