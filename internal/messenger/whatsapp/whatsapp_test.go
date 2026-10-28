package whatsapp

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func TestNormalizePhoneNumber(t *testing.T) {
	cases := map[string]string{
		"+1 (555) 123-4567": "15551234567",
		"15551234567":       "15551234567",
		"+44-20-7946-0958":  "442079460958",
	}
	for input, want := range cases {
		got, err := normalizePhoneNumber(input)
		if err != nil {
			t.Fatalf("normalizePhoneNumber(%q) error: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizePhoneNumber(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizePhoneNumberRejectsInvalid(t *testing.T) {
	for _, input := range []string{"", "not-a-number", "+12a45"} {
		if _, err := normalizePhoneNumber(input); err == nil {
			t.Fatalf("expected error for %q", input)
		}
	}
}

func TestReceiptTypeName(t *testing.T) {
	cases := map[types.ReceiptType]string{
		types.ReceiptTypeDelivered: "DELIVERED",
		types.ReceiptTypeInactive:  "INACTIVE",
		types.ReceiptTypeRead:      "READ",
	}
	for input, want := range cases {
		if got := receiptTypeName(input); got != want {
			t.Fatalf("receiptTypeName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsTargetChatDirect(t *testing.T) {
	m := New()
	direct := types.NewJID("15551234567", types.DefaultUserServer)
	if !m.isTargetChat(direct, types.JID{}, "+1 555 123 4567") {
		t.Fatal("expected direct s.whatsapp.net JID to match the target")
	}
	other := types.NewJID("19998887777", types.DefaultUserServer)
	if m.isTargetChat(other, types.JID{}, "15551234567") {
		t.Fatal("expected a different number not to match")
	}
}

func TestIsTargetChatLIDViaAlternate(t *testing.T) {
	m := New()
	lid := types.NewJID("abc123", types.HiddenUserServer)
	alt := types.NewJID("15551234567", types.DefaultUserServer)
	if !m.isTargetChat(lid, alt, "15551234567") {
		t.Fatal("expected LID with matching alternate PN to match")
	}
}

func TestConsumeQRReturnsTerminalEvent(t *testing.T) {
	cases := map[string]struct {
		items []whatsmeow.QRChannelItem
		want  string
	}{
		"success after codes": {
			items: []whatsmeow.QRChannelItem{
				{Event: whatsmeow.QRChannelEventCode, Code: "code-1"},
				{Event: "success"},
			},
			want: "success",
		},
		"batch expiry closes channel": {
			items: []whatsmeow.QRChannelItem{
				{Event: whatsmeow.QRChannelEventCode, Code: "code-1"},
				whatsmeow.QRChannelTimeout,
			},
			want: "timeout",
		},
		"closed without terminal event is treated as timeout": {
			items: []whatsmeow.QRChannelItem{
				{Event: whatsmeow.QRChannelEventCode, Code: "code-1"},
			},
			want: "timeout",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var codes []string
			m := New(WithQRHandler(func(code string) { codes = append(codes, code) }))
			ch := make(chan whatsmeow.QRChannelItem, len(tc.items))
			for _, item := range tc.items {
				ch <- item
			}
			close(ch)
			if got := m.consumeQR(context.Background(), nil, ch); got != tc.want {
				t.Fatalf("consumeQR = %q, want %q", got, tc.want)
			}
			if len(codes) == 0 {
				t.Fatalf("expected at least one QR code forwarded to the handler")
			}
		})
	}
}
