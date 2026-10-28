package analyzer

import (
	"testing"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

func TestRelativeStates(t *testing.T) {
	t.Run("steady high RTT remains active", func(t *testing.T) {
		capture := captureWithDelays(repeat(1800, 30))
		analysis := Analyze(capture, Options{ReferenceTime: latestReport(capture)})
		if analysis.PhoneState != model.ScreenOn || analysis.AppState != model.AppForeground {
			t.Fatalf("got phone=%s app=%s", analysis.PhoneState, analysis.AppState)
		}
	})
	t.Run("elevated recent RTT indicates inactive states", func(t *testing.T) {
		delays := append(repeat(350, 30), repeat(1000, 12)...)
		analysis := Analyze(captureWithDelays(delays), Options{})
		if analysis.PhoneState != model.ScreenOff || analysis.AppState != model.AppStandby {
			t.Fatalf("got phone=%s app=%s", analysis.PhoneState, analysis.AppState)
		}
	})
}

func TestAccessNetworkClassification(t *testing.T) {
	t.Run("stable low RTT reads as wired companion", func(t *testing.T) {
		analysis := Analyze(captureWithDelays(repeat(90, 30)), Options{})
		if analysis.AccessNetwork == nil || analysis.AccessNetwork.Type != model.NetworkWiredLAN {
			t.Fatalf("got %#v", analysis.AccessNetwork)
		}
	})
	t.Run("low jitter mid RTT reads as wi-fi", func(t *testing.T) {
		analysis := Analyze(captureWithDelays(alternate(240, 260, 30)), Options{})
		if analysis.AccessNetwork == nil || analysis.AccessNetwork.Type != model.NetworkWiFi {
			t.Fatalf("got %#v", analysis.AccessNetwork)
		}
	})
	t.Run("baseline step-change flags a handover", func(t *testing.T) {
		delays := append(repeat(120, 15), repeat(900, 15)...)
		net := Analyze(captureWithDelays(delays), Options{}).AccessNetwork
		if net == nil || !net.TransitionDetected {
			t.Fatalf("expected a transition, got %#v", net)
		}
	})
	t.Run("backgrounded app leaves static type unknown", func(t *testing.T) {
		delays := append(repeat(300, 20), repeat(4000, 15)...)
		net := Analyze(captureWithDelays(delays), Options{}).AccessNetwork
		if net == nil || net.Type != model.NetworkUndecided {
			t.Fatalf("expected unknown static type, got %#v", net)
		}
	})
	t.Run("too few samples yields no guess", func(t *testing.T) {
		if net := Analyze(captureWithDelays(repeat(200, 10)), Options{}).AccessNetwork; net != nil {
			t.Fatalf("expected nil, got %#v", net)
		}
	})
}

func TestReceiptOrderIdentifiesPrimaryOS(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		order []string
		want  model.DeviceType
	}{
		{"reversed", []string{"m", "a", "z"}, model.DeviceIPhone},
		{"natural", []string{"z", "a", "m"}, model.DeviceAndroid},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := model.NewCapture()
			for index, id := range []string{"z", "a", "m"} {
				capture.SentMessages[id] = model.SentMessage{MessageID: id, SentAt: base.Add(time.Duration(index) * time.Second)}
				capture.Receipts = append(capture.Receipts, model.ReceiptEvent{
					DeviceID: 0, ReceiptType: "DELIVERED", MessageIDs: []string{id}, ReceivedAt: base.Add(10 * time.Second),
				})
			}
			capture.Receipts = append(capture.Receipts, model.ReceiptEvent{
				DeviceID: 0, ReceiptType: "READ", MessageIDs: test.order, ReceivedAt: base.Add(11 * time.Second),
			})
			analysis := Analyze(capture, Options{ReferenceTime: base.Add(11 * time.Second)})
			if analysis.DeviceType != test.want {
				t.Fatalf("got %s, want %s", analysis.DeviceType, test.want)
			}
			if analysis.ReceiptStructure == nil || analysis.ReceiptStructure.EvidenceBatches != 1 {
				t.Fatalf("unexpected structure: %#v", analysis.ReceiptStructure)
			}
		})
	}
}

func TestCompanionReceiptCannotIdentifyPhone(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	capture := model.NewCapture()
	for index, id := range []string{"z", "a", "m"} {
		capture.SentMessages[id] = model.SentMessage{MessageID: id, SentAt: base.Add(time.Duration(index) * time.Second)}
		capture.Receipts = append(capture.Receipts, model.ReceiptEvent{DeviceID: 9, ReceiptType: "DELIVERED", MessageIDs: []string{id}})
	}
	capture.Receipts = append(capture.Receipts, model.ReceiptEvent{DeviceID: 9, ReceiptType: "READ", MessageIDs: []string{"m", "a", "z"}})
	if got := Analyze(capture, Options{}).DeviceType; got != model.DeviceUnknown {
		t.Fatalf("got %s, want unknown", got)
	}
}

func captureWithDelays(delays []float64) model.Capture {
	capture := model.NewCapture()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for index, delay := range delays {
		sent := start.Add(time.Duration(index) * time.Second)
		capture.Reports = append(capture.Reports, model.ReceiptReport{
			MessageID: string(rune('a' + index%26)), SentAt: sent,
			DeliveredAt: sent.Add(time.Duration(delay * float64(time.Millisecond))), RTTMillis: delay,
		})
	}
	return capture
}

func alternate(a, b float64, count int) []float64 {
	values := make([]float64, count)
	for index := range values {
		if index%2 == 0 {
			values[index] = a
		} else {
			values[index] = b
		}
	}
	return values
}

func repeat(value float64, count int) []float64 {
	values := make([]float64, count)
	for index := range values {
		values[index] = value
	}
	return values
}

func latestReport(capture model.Capture) time.Time {
	return capture.Reports[len(capture.Reports)-1].DeliveredAt
}
