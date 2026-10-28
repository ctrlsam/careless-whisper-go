package metrics

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

func WritePrometheus(writer io.Writer, phoneNumber string, analysis model.Analysis) error {
	phone := quote(phoneNumber)
	lines := []string{
		"# TYPE carelesswhisper_last_delivery_rtt_ms gauge",
		fmt.Sprintf("carelesswhisper_last_delivery_rtt_ms{phone_number=%s} %s", phone, number(analysis.AverageRTTMillis)),
		"# TYPE carelesswhisper_phone_state gauge",
		enumMetric("carelesswhisper_phone_state", phone, "state", []string{string(model.ScreenOn), string(model.ScreenOff)}, string(analysis.PhoneState)),
		"# TYPE carelesswhisper_app_state gauge",
		enumMetric("carelesswhisper_app_state", phone, "state", []string{string(model.AppForeground), string(model.AppBackground), string(model.AppStandby)}, string(analysis.AppState)),
		"# TYPE carelesswhisper_online_status gauge",
		enumMetric("carelesswhisper_online_status", phone, "state", []string{string(model.Online), string(model.Offline)}, string(analysis.OnlineStatus)),
		fmt.Sprintf("carelesswhisper_estimated_data_used_bytes{phone_number=%s} %d", phone, analysis.TotalDataUsedBytes),
		fmt.Sprintf("carelesswhisper_estimated_data_rate_bytes_per_second{phone_number=%s} %s", phone, number(analysis.EstimatedDataRateBytesSec)),
		fmt.Sprintf("carelesswhisper_companion_devices{phone_number=%s} %d", phone, len(analysis.CompanionDevices)),
	}
	for inference, confidence := range analysis.ConfidenceScores {
		lines = append(lines, fmt.Sprintf(
			"carelesswhisper_analysis_confidence{phone_number=%s,inference=%s} %s",
			phone, quote(inference), number(confidence),
		))
	}
	_, err := fmt.Fprintln(writer, strings.Join(lines, "\n"))
	return err
}

func enumMetric(name, phone, label string, values []string, active string) string {
	lines := make([]string, 0, len(values))
	for _, value := range values {
		sample := 0
		if value == active {
			sample = 1
		}
		lines = append(lines, fmt.Sprintf("%s{phone_number=%s,%s=%s} %d", name, phone, label, quote(value), sample))
	}
	return strings.Join(lines, "\n")
}

func quote(value string) string {
	return strconv.Quote(value)
}

func number(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
