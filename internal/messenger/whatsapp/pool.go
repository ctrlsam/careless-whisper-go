package whatsapp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
)

// Pool fans sends across several independent WhatsApp accounts.
//
// whatsmeow holds a single global send lock per client, held across encrypt,
// socket write, and ack, because the Signal session to a recipient is not safe
// to use concurrently. That caps one client to a single in-flight send.
//
// Each client in the pool links to a separate WhatsApp account (a separate
// phone number), paired as that account's companion device. Separate accounts
// do not share the multi-device sync fan-out that linking several devices to
// one account would, so there is no cross-client decryption traffic. Each
// account has its own identity, Signal session to the target, socket, and send
// lock, giving up to Nx aggregate message rate. It does not raise the data
// ceiling when a large payload already saturates the single shared uplink.
//
// A client is only ever handed to one goroutine at a time through the ready
// channel, so no client's session is used concurrently.
type Pool struct {
	clients []messenger.Messenger
	ready   chan messenger.Messenger

	mu      sync.Mutex
	started bool
}

// devicePath derives a per-account session store path from a base path by
// inserting the account index before the extension: careless-whisper.db ->
// careless-whisper-0.db.
func devicePath(base string, index int) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return fmt.Sprintf("%s-%d%s", stem, index, ext)
}

// PoolConfig configures a multi-account Pool.
type PoolConfig struct {
	// Size is the number of accounts to pair. A value below one is treated as
	// one. Ignored when Paths is set.
	Size int
	// BasePath is the session store path; per-account files are suffixed with
	// the account index. Ignored when Paths is set.
	BasePath string
	// Paths optionally supplies an explicit session store path per account.
	// When non-empty it overrides Size and BasePath: one client is built per
	// entry, in order, so PairPhones still aligns by index.
	Paths []string
	// QR receives the account index and a QR code string for each pairing.
	// Ignored for accounts paired by phone number (see PairPhones).
	QR func(account int, code string)
	// PairPhones optionally supplies a per-account WhatsApp number to link by
	// phone-pairing code instead of QR. An empty or missing entry for an index
	// leaves that account on QR pairing.
	PairPhones []string
	// PairCode receives the account index and an 8-character phone-pairing code
	// for each account paired via PairPhones.
	PairCode func(account int, code string)
	// Status receives the account index and human-readable pairing/connection
	// updates.
	Status func(account int, message string)
	// PairTimeout bounds how long to wait for each account to connect, allowing
	// for a manual QR scan. Zero uses the adapter default.
	PairTimeout time.Duration
	// RequirePaired makes each account refuse to drive pairing and fail when its
	// session is not already linked. Used by run commands.
	RequirePaired bool
	// Logger is the whatsmeow logger shared by every client.
	Logger waLog.Logger
}

// NewPool builds a Pool from cfg.
func NewPool(cfg PoolConfig) *Pool {
	size := cfg.Size
	if len(cfg.Paths) > 0 {
		size = len(cfg.Paths)
	} else if size < 1 {
		size = 1
	}
	logger := cfg.Logger
	if logger == nil {
		logger = waLog.Noop
	}
	p := &Pool{
		clients: make([]messenger.Messenger, 0, size),
		ready:   make(chan messenger.Messenger, size),
	}
	for i := 0; i < size; i++ {
		index := i
		path := devicePath(cfg.BasePath, index)
		if len(cfg.Paths) > 0 {
			path = cfg.Paths[index]
		}
		opts := []Option{
			WithSessionPath(path),
			WithLogger(logger),
		}
		if cfg.QR != nil {
			opts = append(opts, WithQRHandler(func(code string) { cfg.QR(index, code) }))
		}
		if index < len(cfg.PairPhones) && strings.TrimSpace(cfg.PairPhones[index]) != "" {
			opts = append(opts, WithPhonePairing(strings.TrimSpace(cfg.PairPhones[index])))
			if cfg.PairCode != nil {
				opts = append(opts, WithPairCodeHandler(func(code string) { cfg.PairCode(index, code) }))
			}
		}
		if cfg.Status != nil {
			opts = append(opts, WithStatusHandler(func(message string) { cfg.Status(index, message) }))
		}
		if cfg.PairTimeout > 0 {
			opts = append(opts, WithConnectTimeout(cfg.PairTimeout))
		}
		if cfg.RequirePaired {
			opts = append(opts, WithRequirePaired())
		}
		p.clients = append(p.clients, New(opts...))
	}
	return p
}

// newPoolFromMessengers wraps pre-built messengers. Used by tests.
func newPoolFromMessengers(clients []messenger.Messenger) *Pool {
	return &Pool{
		clients: clients,
		ready:   make(chan messenger.Messenger, len(clients)),
	}
}

var _ messenger.Messenger = (*Pool)(nil)

// Start pairs and connects each account sequentially so QR codes are presented
// one at a time, then marks every ready client available for sending.
func (p *Pool) Start(ctx context.Context) error {
	for i, child := range p.clients {
		if err := child.Start(ctx); err != nil {
			return fmt.Errorf("start account %d: %w", i, err)
		}
		if err := child.WaitUntilReady(ctx); err != nil {
			return fmt.Errorf("account %d not ready: %w", i, err)
		}
		p.ready <- child
	}
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	return nil
}

// WaitUntilReady is a no-op because Start blocks until every account is ready.
func (p *Pool) WaitUntilReady(_ context.Context) error {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return fmt.Errorf("pool not started")
	}
	return nil
}

// SendSilentMessage checks out a free client, sends, and returns it so the next
// send can reuse it. With more workers than accounts, extra workers block here
// until an account frees up.
func (p *Pool) SendSilentMessage(ctx context.Context, phoneNumber, reaction string, opts messenger.SendOptions) (string, error) {
	var client messenger.Messenger
	select {
	case client = <-p.ready:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { p.ready <- client }()
	return client.SendSilentMessage(ctx, phoneNumber, reaction, opts)
}

// IsOnPlatform delegates to the first account.
func (p *Pool) IsOnPlatform(ctx context.Context, phoneNumber string) (bool, bool, error) {
	if len(p.clients) == 0 {
		return false, false, fmt.Errorf("pool has no accounts")
	}
	return p.clients[0].IsOnPlatform(ctx, phoneNumber)
}

// Stop tears down every account and returns the first error, if any.
func (p *Pool) Stop(ctx context.Context) error {
	var firstErr error
	for _, child := range p.clients {
		if err := child.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *Pool) SetReceiptTarget(phoneNumber string) {
	for _, child := range p.clients {
		child.SetReceiptTarget(phoneNumber)
	}
}

func (p *Pool) SetDeliveryCallback(fn messenger.DeliveryFunc) {
	for _, child := range p.clients {
		child.SetDeliveryCallback(fn)
	}
}

func (p *Pool) SetReceiptCallback(fn messenger.ReceiptFunc) {
	for _, child := range p.clients {
		child.SetReceiptCallback(fn)
	}
}

func (p *Pool) SetSentMessageCallback(fn messenger.SentMessageFunc) {
	for _, child := range p.clients {
		child.SetSentMessageCallback(fn)
	}
}
