package collector

import (
	"sync"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

const retentionLimit = 1000

type Tracker struct {
	mu          sync.RWMutex
	phoneNumber string
	sendTimes   map[string]time.Time
	pending     map[string]time.Time
	completed   map[string]struct{}
	order       []string
	capture     model.Capture
}

func NewTracker(phoneNumber string) *Tracker {
	return &Tracker{
		phoneNumber: phoneNumber,
		sendTimes:   make(map[string]time.Time),
		pending:     make(map[string]time.Time),
		completed:   make(map[string]struct{}),
		capture:     model.NewCapture(),
	}
}

func (t *Tracker) TrackMessage(messageID string, sentAt time.Time) *model.ReceiptReport {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.capture.SentMessages[messageID] = model.SentMessage{
		Type: "sent_message", MessageID: messageID,
		PhoneNumber: t.phoneNumber, SentAt: sentAt,
	}
	if deliveredAt, ok := t.pending[messageID]; ok {
		delete(t.pending, messageID)
		return t.recordLocked(messageID, sentAt, deliveredAt)
	}
	t.sendTimes[messageID] = sentAt
	return nil
}

func (t *Tracker) RecordDelivery(messageID string, deliveredAt time.Time) *model.ReceiptReport {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, duplicate := t.completed[messageID]; duplicate {
		return nil
	}
	sentAt, ok := t.sendTimes[messageID]
	if !ok {
		if len(t.pending) >= retentionLimit {
			for id := range t.pending {
				delete(t.pending, id)
				break
			}
		}
		if _, exists := t.pending[messageID]; !exists {
			t.pending[messageID] = deliveredAt
		}
		return nil
	}
	delete(t.sendTimes, messageID)
	return t.recordLocked(messageID, sentAt, deliveredAt)
}

const sentMessageLimit = 10000

// ObserveSentMessage records an outgoing message's send order without adding it
// to the probe RTT reports. Existing authoritative records are not overwritten.
func (t *Tracker) ObserveSentMessage(message model.SentMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.capture.SentMessages[message.MessageID]; exists {
		return
	}
	message.Type = "sent_message"
	message.PhoneNumber = t.phoneNumber
	t.capture.SentMessages[message.MessageID] = message
	if len(t.capture.SentMessages) > sentMessageLimit {
		for id := range t.capture.SentMessages {
			delete(t.capture.SentMessages, id)
			break
		}
	}
}

func (t *Tracker) ObserveReceipt(event model.ReceiptEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	event.Type = "receipt_event"
	t.capture.Receipts = append(t.capture.Receipts, event)
	if len(t.capture.Receipts) > retentionLimit {
		t.capture.Receipts = t.capture.Receipts[len(t.capture.Receipts)-retentionLimit:]
	}
}

func (t *Tracker) Capture() model.Capture {
	t.mu.RLock()
	defer t.mu.RUnlock()
	copyCapture := model.NewCapture()
	copyCapture.Reports = append(copyCapture.Reports, t.capture.Reports...)
	copyCapture.Receipts = append(copyCapture.Receipts, t.capture.Receipts...)
	for id, message := range t.capture.SentMessages {
		copyCapture.SentMessages[id] = message
	}
	return copyCapture
}

func (t *Tracker) recordLocked(messageID string, sentAt, deliveredAt time.Time) *model.ReceiptReport {
	report := model.ReceiptReport{
		Type: "receipt", MessageID: messageID, PhoneNumber: t.phoneNumber,
		SentAt: sentAt, DeliveredAt: deliveredAt,
		RTTMillis: float64(deliveredAt.Sub(sentAt)) / float64(time.Millisecond),
	}
	t.capture.Reports = append(t.capture.Reports, report)
	t.completed[messageID] = struct{}{}
	t.order = append(t.order, messageID)
	if len(t.order) > retentionLimit {
		delete(t.completed, t.order[0])
		t.order = t.order[1:]
	}
	return &report
}
