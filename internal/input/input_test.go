package input

import (
	"strings"
	"testing"
)

func TestLoadPythonCSV(t *testing.T) {
	capture, err := loadCSV(strings.NewReader("2026-01-01 00:00:00.123456,125.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := capture.Reports[0].DelayMillis(); got != 125 {
		t.Fatalf("got %v", got)
	}
}

func TestLoadJSONLReceiptEvidence(t *testing.T) {
	data := strings.Join([]string{
		`{"type":"sent_message","msg_id":"z","phone_number":"1","sent_at":"2026-01-01T00:00:00.123456"}`,
		`{"type":"receipt_event","phone_number":"1","sender_jid":"1:0@s.whatsapp.net","device_id":0,"receipt_type":"READ","message_ids":["z"],"received_at":"2026-01-01T00:00:01.123456"}`,
	}, "\n")
	capture, err := loadJSONL(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.SentMessages) != 1 || len(capture.Receipts) != 1 {
		t.Fatalf("unexpected capture: %#v", capture)
	}
}
