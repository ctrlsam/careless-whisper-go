package input

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

func Load(path, format string) (model.Capture, error) {
	file, err := os.Open(path)
	if err != nil {
		return model.Capture{}, err
	}
	defer file.Close()
	if format == "auto" || format == "" {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".csv":
			format = "csv"
		case ".jsonl", ".ndjson":
			format = "jsonl"
		default:
			return model.Capture{}, fmt.Errorf("cannot infer input format from %q", path)
		}
	}
	switch format {
	case "csv":
		return loadCSV(file)
	case "jsonl":
		return loadJSONL(file)
	default:
		return model.Capture{}, fmt.Errorf("unsupported input format %q", format)
	}
}

func loadJSONL(reader io.Reader) (model.Capture, error) {
	capture := model.NewCapture()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			return model.Capture{}, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		switch envelope.Type {
		case "receipt":
			var raw struct {
				Type        string  `json:"type"`
				MessageID   string  `json:"msg_id"`
				PhoneNumber string  `json:"phone_number"`
				SentAt      string  `json:"sent_at"`
				DeliveredAt string  `json:"delivered_at"`
				RTTMillis   float64 `json:"rtt_ms"`
			}
			if err := json.Unmarshal(line, &raw); err != nil {
				return model.Capture{}, fmt.Errorf("line %d receipt: %w", lineNumber, err)
			}
			sentAt, err := parseTime(raw.SentAt)
			if err != nil {
				return model.Capture{}, fmt.Errorf("line %d receipt sent_at: %w", lineNumber, err)
			}
			deliveredAt, err := parseTime(raw.DeliveredAt)
			if err != nil {
				return model.Capture{}, fmt.Errorf("line %d receipt delivered_at: %w", lineNumber, err)
			}
			report := model.ReceiptReport{
				Type: raw.Type, MessageID: raw.MessageID, PhoneNumber: raw.PhoneNumber,
				SentAt: sentAt, DeliveredAt: deliveredAt, RTTMillis: raw.RTTMillis,
			}
			capture.Reports = append(capture.Reports, report)
		case "sent_message":
			var raw struct {
				Type        string `json:"type"`
				MessageID   string `json:"msg_id"`
				PhoneNumber string `json:"phone_number"`
				SentAt      string `json:"sent_at"`
			}
			if err := json.Unmarshal(line, &raw); err != nil {
				return model.Capture{}, fmt.Errorf("line %d sent message: %w", lineNumber, err)
			}
			sentAt, err := parseTime(raw.SentAt)
			if err != nil {
				return model.Capture{}, fmt.Errorf("line %d sent message sent_at: %w", lineNumber, err)
			}
			message := model.SentMessage{
				Type: raw.Type, MessageID: raw.MessageID,
				PhoneNumber: raw.PhoneNumber, SentAt: sentAt,
			}
			capture.SentMessages[message.MessageID] = message
		case "receipt_event":
			var raw struct {
				Type        string   `json:"type"`
				PhoneNumber string   `json:"phone_number"`
				SenderJID   string   `json:"sender_jid"`
				DeviceID    int      `json:"device_id"`
				ReceiptType string   `json:"receipt_type"`
				MessageIDs  []string `json:"message_ids"`
				ReceivedAt  string   `json:"received_at"`
			}
			if err := json.Unmarshal(line, &raw); err != nil {
				return model.Capture{}, fmt.Errorf("line %d receipt event: %w", lineNumber, err)
			}
			receivedAt, err := parseTime(raw.ReceivedAt)
			if err != nil {
				return model.Capture{}, fmt.Errorf("line %d receipt event received_at: %w", lineNumber, err)
			}
			event := model.ReceiptEvent{
				Type: raw.Type, PhoneNumber: raw.PhoneNumber, SenderJID: raw.SenderJID,
				DeviceID: raw.DeviceID, ReceiptType: raw.ReceiptType,
				MessageIDs: raw.MessageIDs, ReceivedAt: receivedAt,
			}
			capture.Receipts = append(capture.Receipts, event)
		case "analysis":
			continue
		default:
			return model.Capture{}, fmt.Errorf("line %d: unsupported record type %q", lineNumber, envelope.Type)
		}
	}
	return capture, scanner.Err()
}

func loadCSV(reader io.Reader) (model.Capture, error) {
	capture := model.NewCapture()
	records, err := csv.NewReader(reader).ReadAll()
	if err != nil {
		return model.Capture{}, err
	}
	for index, record := range records {
		if len(record) != 2 {
			return model.Capture{}, fmt.Errorf("row %d: expected timestamp,rtt_ms", index+1)
		}
		sentAt, err := parseTime(record[0])
		if err != nil {
			return model.Capture{}, fmt.Errorf("row %d: %w", index+1, err)
		}
		rtt, err := strconv.ParseFloat(strings.TrimSpace(record[1]), 64)
		if err != nil {
			return model.Capture{}, fmt.Errorf("row %d RTT: %w", index+1, err)
		}
		capture.Reports = append(capture.Reports, model.ReceiptReport{
			Type: "receipt", MessageID: fmt.Sprintf("csv-%d", index+1),
			SentAt: sentAt, DeliveredAt: sentAt.Add(time.Duration(rtt * float64(time.Millisecond))),
			RTTMillis: rtt,
		})
	}
	return capture, nil
}

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("timestamp is not RFC3339 or Python datetime format")
}
