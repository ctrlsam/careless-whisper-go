package collector

import (
	"testing"
	"time"
)

func TestEarlyDeliveryIsBuffered(t *testing.T) {
	tracker := NewTracker("123456789")
	sentAt := time.Now().Add(-50 * time.Millisecond)
	if report := tracker.RecordDelivery("message-1", time.Now()); report != nil {
		t.Fatal("early delivery should be buffered")
	}
	report := tracker.TrackMessage("message-1", sentAt)
	if report == nil || report.MessageID != "message-1" {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestDuplicateDeliveryIsIgnored(t *testing.T) {
	tracker := NewTracker("123456789")
	tracker.TrackMessage("message-1", time.Now())
	tracker.RecordDelivery("message-1", time.Now())
	if report := tracker.RecordDelivery("message-1", time.Now()); report != nil {
		t.Fatalf("duplicate produced report: %#v", report)
	}
	if got := len(tracker.Capture().Reports); got != 1 {
		t.Fatalf("got %d reports", got)
	}
}
