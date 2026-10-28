// Package messenger defines the protocol-agnostic adapter surface used by the
// live fingerprint and flood tools.
//
// RESEARCH USE ONLY. The adapters in this package drive real messaging
// backends against a real target phone number. Only run them against devices
// and accounts you own or are explicitly authorised to test.
package messenger

import (
	"context"
	"errors"
	"fmt"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

// DeliveryFunc is invoked with a message ID when a delivery receipt arrives.
type DeliveryFunc func(msgID string)

// ReceiptFunc is invoked for each observed protocol receipt batch.
type ReceiptFunc func(event model.ReceiptEvent)

// SentMessageFunc is invoked for each observed outgoing message to the target.
type SentMessageFunc func(message model.SentMessage)

// ProbeKind selects the protocol primitive used for a silent probe. All kinds
// elicit a delivery receipt; they differ in whether the target's client renders
// anything. Which kinds are genuinely silent is a property of the current
// client build and must be confirmed on-device, not assumed.
type ProbeKind string

const (
	// ProbeReaction reacts to a phantom message with a valid emoji. The target
	// renders a "Waiting for this message" placeholder per probe, so this is
	// NOT silent; it is kept for A/B comparison and matches the flood payload.
	ProbeReaction ProbeKind = "reaction"
	// ProbeEmptyReaction sends a reaction with empty text (a reaction removal)
	// referencing a phantom message. There is no reaction to attach, so it may
	// not spawn a placeholder.
	ProbeEmptyReaction ProbeKind = "empty-reaction"
	// ProbeRevoke sends a delete-for-everyone for a phantom message the target
	// never received. There is nothing to delete, so it should render nothing.
	ProbeRevoke ProbeKind = "revoke"
)

// SendOptions tunes a single send.
type SendOptions struct {
	// NoAck returns as soon as the message has been written to the socket,
	// without waiting for the server send acknowledgement. This raises flood
	// throughput at the cost of losing per-message send confirmation. It does
	// not affect delivery receipts, which arrive as separate events.
	NoAck bool

	// ProbeKind selects the probe primitive. The zero value is ProbeReaction,
	// which keeps the flood path (oversized reaction payload) unchanged.
	ProbeKind ProbeKind
}

// Messenger is an authorised adapter over an end-to-end encrypted messenger.
//
// Implementations must only ever act against the configured target. They send
// silent messages that trigger a delivery receipt without surfacing a visible
// notification, which is the mechanism the research describes.
type Messenger interface {
	// Start brings up the backend connection. It may return before the
	// connection is fully ready; callers must wait with WaitUntilReady.
	Start(ctx context.Context) error

	// WaitUntilReady blocks until the backend is connected and able to send.
	WaitUntilReady(ctx context.Context) error

	// SendSilentMessage sends one silent reaction to phoneNumber and returns
	// the sent message ID. An empty reaction uses the adapter default; the
	// flood tool passes an oversized payload here. opts tunes ack behaviour.
	SendSilentMessage(ctx context.Context, phoneNumber, reaction string, opts SendOptions) (msgID string, err error)

	// IsOnPlatform reports whether phoneNumber is registered. known is false
	// when the backend could not determine registration.
	IsOnPlatform(ctx context.Context, phoneNumber string) (registered bool, known bool, err error)

	// Stop tears down the backend connection and releases resources.
	Stop(ctx context.Context) error

	// SetReceiptTarget restricts receipt and sent-message observation to one
	// target number. Evidence for any other chat is discarded.
	SetReceiptTarget(phoneNumber string)

	// SetDeliveryCallback registers the delivery-receipt handler.
	SetDeliveryCallback(DeliveryFunc)

	// SetReceiptCallback registers the protocol-receipt handler.
	SetReceiptCallback(ReceiptFunc)

	// SetSentMessageCallback registers the outgoing-message handler.
	SetSentMessageCallback(SentMessageFunc)
}

// ErrUnregistered is returned by ConfirmNumber when the target is not
// registered and the caller did not opt to ignore the warning.
var ErrUnregistered = errors.New("phone number is not registered on the platform")

// ErrSessionLost is returned by a send when the adapter's session is no longer
// usable - the account has been logged out or unlinked by the server. It is a
// fatal, non-retryable condition: the caller should stop rather than retry,
// because every further send will fail the same way. Adapters wrap it so
// callers can detect it with errors.Is.
var ErrSessionLost = errors.New("messenger session lost; the account must be re-paired")

// ConfirmNumber verifies the target is registered before any messages are
// sent. When registration cannot be determined it logs through warn and
// proceeds. When the number is unregistered it returns ErrUnregistered unless
// ignoreUnregistered is set.
func ConfirmNumber(ctx context.Context, m Messenger, phoneNumber string, ignoreUnregistered bool, warn func(string)) error {
	registered, known, err := m.IsOnPlatform(ctx, phoneNumber)
	if err != nil {
		return fmt.Errorf("check registration: %w", err)
	}
	if !known {
		if warn != nil {
			warn(fmt.Sprintf("could not verify whether %s is registered on the platform", phoneNumber))
		}
		return nil
	}
	if !registered {
		if warn != nil {
			warn(fmt.Sprintf("the phone number %s is not registered on the platform", phoneNumber))
		}
		if !ignoreUnregistered {
			return fmt.Errorf("%w: use --ignore-unregistered to override", ErrUnregistered)
		}
	}
	return nil
}
