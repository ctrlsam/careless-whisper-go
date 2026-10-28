// Package whatsapp implements an authorised WhatsApp adapter over whatsmeow.
//
// RESEARCH USE ONLY. This adapter sends real silent reaction messages to a
// real phone number and observes real delivery/read receipts. Only run it
// against devices and accounts you own or are explicitly authorised to test.
package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver registered as "sqlite"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

// DefaultReaction is the reaction payload used when no override is supplied.
const DefaultReaction = "👍"

// DefaultSessionPath is the SQLite session store used by the adapter.
const DefaultSessionPath = "careless-whisper.db"

// defaultConnectTimeout bounds how long WaitUntilReady waits for the first
// authenticated connection, and how long the adapter keeps regenerating QR
// codes while pairing a fresh device. whatsmeow hands out a fixed batch of QR
// codes (roughly 2m40s of codes) and then gives up; interactive pairing - more
// so when scanning several lab phones for separate accounts - routinely needs
// longer, so this default is generous and is overridable per call.
const defaultConnectTimeout = 5 * time.Minute

// Messenger is a whatsmeow-backed implementation of messenger.Messenger.
type Messenger struct {
	sessionPath    string
	log            waLog.Logger
	qr             func(code string)
	pairPhone      string
	pairCode       func(code string)
	status         func(message string)
	connectTimeout time.Duration

	mu            sync.RWMutex
	container     *sqlstore.Container
	client        *whatsmeow.Client
	receiptTarget string
	onDelivery    messenger.DeliveryFunc
	onReceipt     messenger.ReceiptFunc
	onSent        messenger.SentMessageFunc

	ready chan struct{}
	once  sync.Once
}

// Option configures a Messenger.
type Option func(*Messenger)

// WithSessionPath overrides the SQLite session store path.
func WithSessionPath(path string) Option {
	return func(m *Messenger) { m.sessionPath = path }
}

// WithQRHandler sets a handler that receives each new login QR code string.
func WithQRHandler(handler func(code string)) Option {
	return func(m *Messenger) { m.qr = handler }
}

// WithPhonePairing links by phone number instead of QR. number is the WhatsApp
// account being paired (the companion's own number). When set, Start requests
// an 8-character pairing code the user enters on that phone under Link a device
// -> Link with phone number instead, rather than printing a QR code. Code
// pairing and QR pairing hit the same device-linking backend, so an account
// that is being throttled ("Can't link new devices right now") can still be
// refused here; it is a different on-phone flow, not a throttle bypass.
func WithPhonePairing(number string) Option {
	return func(m *Messenger) { m.pairPhone = number }
}

// WithPairCodeHandler sets a handler that receives each phone-pairing code. It
// is only used when WithPhonePairing is set.
func WithPairCodeHandler(handler func(code string)) Option {
	return func(m *Messenger) { m.pairCode = handler }
}

// WithStatusHandler sets a handler for human-readable pairing/connection status
// updates (QR scanned, device paired, logged out). It is independent of the
// whatsmeow log level, so progress stays visible even when the log is quiet.
func WithStatusHandler(handler func(message string)) Option {
	return func(m *Messenger) { m.status = handler }
}

// WithConnectTimeout overrides how long WaitUntilReady waits for the first
// authenticated connection.
func WithConnectTimeout(d time.Duration) Option {
	return func(m *Messenger) {
		if d > 0 {
			m.connectTimeout = d
		}
	}
}

// WithLogger sets the whatsmeow logger.
func WithLogger(logger waLog.Logger) Option {
	return func(m *Messenger) { m.log = logger }
}

// New returns a WhatsApp adapter. It does not open any connection until Start.
func New(opts ...Option) *Messenger {
	silenceSignalLogging()
	m := &Messenger{
		sessionPath:    DefaultSessionPath,
		log:            waLog.Noop,
		connectTimeout: defaultConnectTimeout,
		ready:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

var _ messenger.Messenger = (*Messenger)(nil)

func (m *Messenger) SetReceiptTarget(phoneNumber string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receiptTarget = phoneNumber
}

func (m *Messenger) SetDeliveryCallback(fn messenger.DeliveryFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onDelivery = fn
}

func (m *Messenger) SetReceiptCallback(fn messenger.ReceiptFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onReceipt = fn
}

func (m *Messenger) SetSentMessageCallback(fn messenger.SentMessageFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onSent = fn
}

// Start opens the session store, connects the client, and drives QR login when
// the stored session is empty.
func (m *Messenger) Start(ctx context.Context) error {
	container, err := m.openStore(ctx)
	if err != nil {
		return err
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("load device: %w", err)
	}
	client := whatsmeow.NewClient(device, m.log)
	client.AddEventHandler(m.handleEvent)

	m.mu.Lock()
	m.container = container
	m.client = client
	m.mu.Unlock()

	if client.Store.ID == nil {
		go m.runQRLogin(ctx, client)
		return nil
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

// runQRLogin drives interactive pairing, by QR scan or by phone-number code.
// whatsmeow emits a fixed batch of QR codes and then closes the channel with a
// timeout, disconnecting the client, and the phone-pairing code is tied to that
// same login socket, so both modes share the ~2m40s batch limit. A single batch
// is often too short, especially when pairing several separate accounts in turn,
// so this keeps requesting fresh batches (and fresh pairing codes) until the
// device is paired, the context is cancelled, or the connect-timeout pairing
// window elapses. Only genuine code expiry triggers regeneration; real pairing
// errors stop the loop.
func (m *Messenger) runQRLogin(ctx context.Context, client *whatsmeow.Client) {
	pairCtx, cancel := context.WithTimeout(ctx, m.connectTimeout)
	defer cancel()
	for {
		qrChan, err := client.GetQRChannel(pairCtx)
		if err != nil {
			if errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
				return // already paired by a previous batch
			}
			m.log.Errorf("open qr channel: %v", err)
			return
		}
		if err := client.Connect(); err != nil {
			m.log.Errorf("connect: %v", err)
			return
		}
		switch m.consumeQR(pairCtx, client, qrChan) {
		case "success":
			return
		case "timeout":
			if pairCtx.Err() != nil {
				m.notify("pairing window elapsed before the code was used")
				return
			}
			client.Disconnect()
			if m.pairPhone != "" {
				m.notify("pairing code expired; requesting a fresh code")
			} else {
				m.notify("QR code batch expired; generating a fresh code to scan")
			}
		default:
			return // genuine pairing error already logged by consumeQR
		}
	}
}

// openStore opens the SQLite session store tuned for concurrent writers.
//
// whatsmeow writes to the store on the send hot path (token and timestamp
// persistence). Under a high-rate flood, the default rollback journal and
// absent busy handler surface SQLITE_BUSY errors. WAL plus a busy timeout lets
// writers wait briefly instead of failing, and a single open connection keeps
// writes serialised through one SQLite handle.
func (m *Messenger) openStore(ctx context.Context) (*sqlstore.Container, error) {
	address := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		m.sessionPath,
	)
	db, err := sql.Open("sqlite", address)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	db.SetMaxOpenConns(1)
	container := sqlstore.NewWithDB(db, "sqlite", m.log)
	if err := container.Upgrade(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("upgrade session store: %w", err)
	}
	return container, nil
}

// consumeQR drives one pairing batch and returns the terminal event that closed
// the channel: "success" when paired, "timeout" when the current code batch
// expired without being used, or the whatsmeow error-event name for a genuine
// pairing failure. In QR mode it forwards each code to the QR handler. In
// phone-pairing mode it ignores the QR codes and instead, on the first code
// event (which signals the login socket is established), requests a single
// phone-pairing code and forwards it to the pair-code handler.
func (m *Messenger) consumeQR(ctx context.Context, client *whatsmeow.Client, qrChan <-chan whatsmeow.QRChannelItem) string {
	requestedCode := false
	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if m.pairPhone == "" {
				if m.qr != nil {
					m.qr(item.Code)
				}
				continue
			}
			if requestedCode {
				continue
			}
			requestedCode = true
			code, err := client.PairPhone(ctx, m.pairPhone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
			if err != nil {
				m.log.Errorf("request pairing code: %v", err)
				return "error"
			}
			if m.pairCode != nil {
				m.pairCode(code)
			}
		case "success":
			m.notify("linking device (a websocket EOF here is expected while WhatsApp re-establishes the authenticated connection)")
			return item.Event
		default:
			if item.Error != nil {
				m.log.Errorf("pairing error: %v", item.Error)
			}
			return item.Event
		}
	}
	return "timeout"
}

// WaitUntilReady blocks until the client reports a successful connection.
func (m *Messenger) WaitUntilReady(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, m.connectTimeout)
	defer cancel()
	select {
	case <-m.ready:
		return nil
	case <-waitCtx.Done():
		return fmt.Errorf("timed out waiting for WhatsApp connection: %w", waitCtx.Err())
	}
}

func (m *Messenger) signalReady() {
	m.once.Do(func() { close(m.ready) })
}

func (m *Messenger) notify(message string) {
	if m.status != nil {
		m.status(message)
	}
}

// noAckTimeout is the response-wait budget used when a caller opts out of the
// server send ack. whatsmeow writes the node to the socket before this wait, so
// a tiny timeout returns right after the send and releases the global send lock
// without waiting roughly a second for the ack.
const noAckTimeout = time.Millisecond

// SendSilentMessage sends a single probe to phoneNumber referencing a freshly
// generated, non-existent message ID, and returns the sent message ID. All
// probe kinds elicit a delivery receipt, which is the timing signal.
//
// Silence is per-kind and NOT guaranteed. ProbeReaction (a valid emoji
// reaction to a phantom message) makes the target render a "Waiting for this
// message" placeholder, one per probe, so it is not silent. ProbeRevoke and
// ProbeEmptyReaction reference a phantom message with nothing to show and are
// the silent candidates, but this depends on the target's client build and
// must be confirmed on-device.
func (m *Messenger) SendSilentMessage(ctx context.Context, phoneNumber, reaction string, opts messenger.SendOptions) (string, error) {
	client := m.snapshotClient()
	if client == nil {
		return "", errors.New("WhatsApp client is not connected")
	}
	normalized, err := normalizePhoneNumber(phoneNumber)
	if err != nil {
		return "", err
	}
	chat := types.NewJID(normalized, types.DefaultUserServer)
	referencedID := client.GenerateMessageID()

	var message *waE2E.Message
	switch opts.ProbeKind {
	case messenger.ProbeRevoke:
		// Delete-for-everyone of a phantom message we "sent"; empty sender JID
		// means FromMe, so there is nothing on the target to delete.
		message = client.BuildRevoke(chat, types.EmptyJID, referencedID)
	case messenger.ProbeEmptyReaction:
		// Empty reaction text is a reaction removal, so there is no reaction to
		// attach to the phantom parent.
		message = client.BuildReaction(chat, chat, referencedID, "")
	default:
		if reaction == "" {
			reaction = DefaultReaction
		}
		message = client.BuildReaction(chat, chat, referencedID, reaction)
	}

	var extra []whatsmeow.SendRequestExtra
	if opts.NoAck {
		extra = append(extra, whatsmeow.SendRequestExtra{Timeout: noAckTimeout})
	}
	resp, err := client.SendMessage(ctx, chat, message, extra...)
	if err != nil {
		// In no-ack mode the node is already on the wire; a timed-out ack is
		// the expected, successful outcome.
		if opts.NoAck && errors.Is(err, whatsmeow.ErrMessageTimedOut) {
			return string(resp.ID), nil
		}
		return "", fmt.Errorf("send silent message: %w", err)
	}
	return string(resp.ID), nil
}

// IsOnPlatform reports whether phoneNumber is registered on WhatsApp.
func (m *Messenger) IsOnPlatform(ctx context.Context, phoneNumber string) (bool, bool, error) {
	client := m.snapshotClient()
	if client == nil {
		return false, false, errors.New("WhatsApp client is not connected")
	}
	normalized, err := normalizePhoneNumber(phoneNumber)
	if err != nil {
		return false, false, err
	}
	results, err := client.IsOnWhatsApp(ctx, []string{"+" + normalized})
	if err != nil {
		return false, false, fmt.Errorf("is-on-whatsapp lookup: %w", err)
	}
	if len(results) == 0 {
		return false, true, nil
	}
	return results[0].IsIn, true, nil
}

// Stop disconnects the client.
func (m *Messenger) Stop(_ context.Context) error {
	client := m.snapshotClient()
	if client != nil {
		client.Disconnect()
	}
	m.mu.Lock()
	container := m.container
	m.container = nil
	m.mu.Unlock()
	if container != nil {
		return container.Close()
	}
	return nil
}

func (m *Messenger) snapshotClient() *whatsmeow.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.client
}

func (m *Messenger) callbacks() (messenger.DeliveryFunc, messenger.ReceiptFunc, messenger.SentMessageFunc, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onDelivery, m.onReceipt, m.onSent, m.receiptTarget
}

func (m *Messenger) handleEvent(evt any) {
	switch event := evt.(type) {
	case *events.Connected:
		m.signalReady()
	case *events.PairSuccess:
		m.notify(fmt.Sprintf("paired as %s; establishing authenticated connection", event.ID.User))
	case *events.LoggedOut:
		m.notify("logged out by the server; re-pair this account")
	case *events.Receipt:
		m.handleReceipt(event)
	case *events.Message:
		m.handleMessage(event)
	}
}

func (m *Messenger) handleReceipt(receipt *events.Receipt) {
	receivedAt := time.Now()
	onDelivery, onReceipt, _, target := m.callbacks()

	if receipt.Type == types.ReceiptTypeDelivered || receipt.Type == types.ReceiptTypeInactive {
		if onDelivery != nil {
			for _, id := range receipt.MessageIDs {
				onDelivery(string(id))
			}
		}
	}

	if onReceipt == nil || target == "" {
		return
	}
	if receipt.Type != types.ReceiptTypeDelivered &&
		receipt.Type != types.ReceiptTypeInactive &&
		receipt.Type != types.ReceiptTypeRead {
		return
	}
	if receipt.IsFromMe || receipt.IsGroup || len(receipt.MessageIDs) == 0 {
		return
	}
	if !m.isTargetChat(receipt.Chat, receipt.RecipientAlt, target) {
		return
	}
	sender := receipt.Sender
	senderMatches := (sender.User == receipt.Chat.User && sender.Server == receipt.Chat.Server) ||
		m.isTargetChat(sender, receipt.SenderAlt, target)
	if sender.User == "" || !senderMatches {
		return
	}
	normalizedTarget, err := normalizePhoneNumber(target)
	if err != nil {
		return
	}
	ids := make([]string, len(receipt.MessageIDs))
	for i, id := range receipt.MessageIDs {
		ids[i] = string(id)
	}
	onReceipt(model.ReceiptEvent{
		Type:        "receipt_event",
		PhoneNumber: normalizedTarget,
		SenderJID:   fmt.Sprintf("%s:%d@%s", sender.User, sender.Device, sender.Server),
		DeviceID:    int(sender.Device),
		ReceiptType: receiptTypeName(receipt.Type),
		MessageIDs:  ids,
		ReceivedAt:  receivedAt,
	})
}

func (m *Messenger) handleMessage(message *events.Message) {
	_, _, onSent, target := m.callbacks()
	if onSent == nil || target == "" {
		return
	}
	source := message.Info.MessageSource
	if !source.IsFromMe || source.IsGroup {
		return
	}
	if !m.isTargetChat(source.Chat, source.RecipientAlt, target) {
		return
	}
	if message.Info.ID == "" || message.Info.Timestamp.IsZero() {
		return
	}
	normalizedTarget, err := normalizePhoneNumber(target)
	if err != nil {
		return
	}
	onSent(model.SentMessage{
		Type:        "sent_message",
		MessageID:   string(message.Info.ID),
		PhoneNumber: normalizedTarget,
		SentAt:      message.Info.Timestamp,
	})
}

// isTargetChat reports whether jid (optionally via its alternate address or the
// local LID mapping) resolves to the target phone number.
func (m *Messenger) isTargetChat(jid, alternate types.JID, target string) bool {
	normalizedTarget, err := normalizePhoneNumber(target)
	if err != nil || jid.User == "" {
		return false
	}
	if jid.Server == types.DefaultUserServer {
		return jid.User == normalizedTarget
	}
	if jid.Server != types.HiddenUserServer {
		return false
	}
	if alternate.Server == types.DefaultUserServer && alternate.User != "" {
		return alternate.User == normalizedTarget
	}
	client := m.snapshotClient()
	if client == nil {
		return false
	}
	resolved, err := client.Store.LIDs.GetPNForLID(context.Background(), jid)
	if err != nil || resolved.User == "" {
		return false
	}
	return resolved.Server == types.DefaultUserServer && resolved.User == normalizedTarget
}

func receiptTypeName(receiptType types.ReceiptType) string {
	switch receiptType {
	case types.ReceiptTypeDelivered:
		return "DELIVERED"
	case types.ReceiptTypeInactive:
		return "INACTIVE"
	case types.ReceiptTypeRead:
		return "READ"
	default:
		return strings.ToUpper(string(receiptType))
	}
}

// normalizePhoneNumber returns a digits-only international number.
func normalizePhoneNumber(phoneNumber string) (string, error) {
	number := strings.TrimSpace(phoneNumber)
	number = strings.TrimPrefix(number, "+")
	for _, separator := range []string{" ", "-", "(", ")"} {
		number = strings.ReplaceAll(number, separator, "")
	}
	if number == "" {
		return "", errors.New("phone number is empty")
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return "", errors.New("phone number must be an international number containing only digits, optionally prefixed with '+'")
		}
	}
	return number, nil
}
