package model

import "time"

type PhoneState string

const (
	ScreenOn  PhoneState = "SCREEN_ON"
	ScreenOff PhoneState = "SCREEN_OFF"
)

type AppState string

const (
	AppForeground AppState = "APP_FOREGROUND"
	AppBackground AppState = "APP_BACKGROUND"
	AppStandby    AppState = "APP_STANDBY"
)

type OnlineStatus string

const (
	Online  OnlineStatus = "ONLINE"
	Offline OnlineStatus = "OFFLINE"
)

type DeviceType string

const (
	DeviceIPhone  DeviceType = "iPhone"
	DeviceAndroid DeviceType = "Android"
	DeviceUnknown DeviceType = "Unknown"
)

type ReceiptReport struct {
	Type        string    `json:"type,omitempty"`
	MessageID   string    `json:"msg_id"`
	PhoneNumber string    `json:"phone_number"`
	SentAt      time.Time `json:"sent_at"`
	DeliveredAt time.Time `json:"delivered_at"`
	RTTMillis   float64   `json:"rtt_ms"`
}

func (r ReceiptReport) DelayMillis() float64 {
	if r.RTTMillis != 0 {
		return r.RTTMillis
	}
	return float64(r.DeliveredAt.Sub(r.SentAt)) / float64(time.Millisecond)
}

type SentMessage struct {
	Type        string    `json:"type,omitempty"`
	MessageID   string    `json:"msg_id"`
	PhoneNumber string    `json:"phone_number"`
	SentAt      time.Time `json:"sent_at"`
}

type ReceiptEvent struct {
	Type        string    `json:"type,omitempty"`
	PhoneNumber string    `json:"phone_number"`
	SenderJID   string    `json:"sender_jid"`
	DeviceID    int       `json:"device_id"`
	ReceiptType string    `json:"receipt_type"`
	MessageIDs  []string  `json:"message_ids"`
	ReceivedAt  time.Time `json:"received_at"`
}

type Capture struct {
	Reports      []ReceiptReport
	SentMessages map[string]SentMessage
	Receipts     []ReceiptEvent
}

func NewCapture() Capture {
	return Capture{SentMessages: make(map[string]SentMessage)}
}

type ReceiptStructure struct {
	DeliveryReceiptHandling string `json:"delivery_receipt_handling"`
	ReadReceiptHandling     string `json:"read_receipt_handling"`
	ReceiptOrdering         string `json:"receipt_ordering"`
	Platform                string `json:"platform"`
	Messenger               string `json:"messenger"`
	EvidenceBatches         int    `json:"evidence_batches"`
}

type CompanionDevice struct {
	DeviceIndex      int       `json:"device_index"`
	Type             string    `json:"type"`
	AverageRTTMillis float64   `json:"avg_rtt_ms"`
	StdDevRTTMillis  float64   `json:"stdev_rtt_ms"`
	Network          string    `json:"network"`
	SampleCount      int       `json:"sample_count"`
	Activity         string    `json:"activity"`
	LastSeen         time.Time `json:"last_seen"`
	Switching        bool      `json:"switching_detected,omitempty"`
}

// AccessNetwork is a best-effort guess at the target's access network, inferred
// from delivery-receipt RTT jitter and baseline step-changes. It reproduces the
// qualitative Wi-Fi/cellular signal from the Careless Whisper paper: the
// discriminator is jitter and a handover step-change, not absolute RTT. The
// static Type is only set when the app is foreground and online, because a
// backgrounded app's RTT is dominated by push-wake latency rather than the
// radio. Thresholds are heuristic and need per-device calibration.
type AccessNetwork struct {
	Type               string  `json:"type"`
	RecentMedianRTTMs  float64 `json:"recent_median_rtt_ms"`
	RecentJitterMs     float64 `json:"recent_jitter_ms"`
	CoeffVariation     float64 `json:"coeff_variation"`
	TransitionDetected bool    `json:"transition_detected"`
	TransitionNote     string  `json:"transition_note,omitempty"`
	Confidence         float64 `json:"confidence"`
}

const (
	NetworkWiredLAN  = "Wired/LAN"
	NetworkWiFi      = "Wi-Fi"
	NetworkCellular  = "Cellular"
	NetworkUndecided = "Unknown"
)

type Analysis struct {
	AnalyzedAt                time.Time          `json:"analyzed_at"`
	PhoneState                PhoneState         `json:"phone_state"`
	AppState                  AppState           `json:"app_state"`
	TotalDataUsedBytes        int                `json:"total_data_used_bytes"`
	EstimatedDataRateBytesSec float64            `json:"estimated_data_rate_bytes_per_second"`
	EstimatedBatteryDrainMin  float64            `json:"estimated_battery_drain_pct_per_hour_min"`
	EstimatedBatteryDrainMax  float64            `json:"estimated_battery_drain_pct_per_hour_max"`
	DeviceType                DeviceType         `json:"device_type"`
	AverageRTTMillis          float64            `json:"avg_rtt_ms"`
	MedianRTTMillis           float64            `json:"median_rtt_ms"`
	StdDevRTTMillis           float64            `json:"rtt_stdev_ms"`
	TotalRequests             int                `json:"total_requests"`
	OnlineStatus              OnlineStatus       `json:"online_status"`
	ConfidenceScores          map[string]float64 `json:"confidence_scores"`
	ReceiptStructure          *ReceiptStructure  `json:"receipt_structure,omitempty"`
	CompanionDevices          []CompanionDevice  `json:"companion_devices,omitempty"`
	AccessNetwork             *AccessNetwork     `json:"access_network,omitempty"`
}
