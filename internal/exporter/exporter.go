package exporter

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

func SafeIdentifier(target string) string {
	var builder strings.Builder
	for _, character := range target {
		if character <= unicode.MaxASCII && (unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_') {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "unknown"
	}
	return builder.String()
}

func WriteAnalysis(writer io.Writer, analysis model.Analysis, pretty bool) error {
	encoder := json.NewEncoder(writer)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(analysis)
}

func AppendJSONL(path string, records ...any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}

func AppendCSV(path string, reports ...model.ReceiptReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	for _, report := range reports {
		if err := writer.Write([]string{report.SentAt.String(), fmt.Sprintf("%.6f", report.DelayMillis())}); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}
