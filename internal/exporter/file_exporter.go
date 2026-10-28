package exporter

import (
	"fmt"
	"path/filepath"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

// Exporter persists live capture records and analysis snapshots.
type Exporter interface {
	SaveReport(model.ReceiptReport) error
	SaveReceiptEvent(model.ReceiptEvent) error
	SaveSentMessage(model.SentMessage) error
	SaveAnalysis(model.Analysis) error
}

// NewFileExporter returns an Exporter writing under baseDir for the given
// format ("jsonl" or "csv") and target. Files are named after a sanitised
// target identifier, mirroring the Python project's export layout.
func NewFileExporter(format, baseDir, target string) (Exporter, error) {
	id := SafeIdentifier(target)
	switch format {
	case "jsonl":
		return &jsonlExporter{path: filepath.Join(baseDir, "jsonl", id+"_analysis.jsonl")}, nil
	case "csv":
		return &csvExporter{
			delaysPath:   filepath.Join(baseDir, "csv", id+"_delays.csv"),
			receiptsPath: filepath.Join(baseDir, "csv", id+"_delays.receipts.jsonl"),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported exporter %q: choose jsonl or csv", format)
	}
}

type jsonlExporter struct {
	path string
}

func (e *jsonlExporter) SaveReport(report model.ReceiptReport) error {
	return AppendJSONL(e.path, report)
}

func (e *jsonlExporter) SaveReceiptEvent(event model.ReceiptEvent) error {
	return AppendJSONL(e.path, event)
}

func (e *jsonlExporter) SaveSentMessage(message model.SentMessage) error {
	return AppendJSONL(e.path, message)
}

func (e *jsonlExporter) SaveAnalysis(analysis model.Analysis) error {
	record := map[string]any{"type": "analysis", "analysis": analysis}
	return AppendJSONL(e.path, record)
}

type csvExporter struct {
	delaysPath   string
	receiptsPath string
}

func (e *csvExporter) SaveReport(report model.ReceiptReport) error {
	return AppendCSV(e.delaysPath, report)
}

func (e *csvExporter) SaveReceiptEvent(event model.ReceiptEvent) error {
	return AppendJSONL(e.receiptsPath, event)
}

func (e *csvExporter) SaveSentMessage(message model.SentMessage) error {
	return AppendJSONL(e.receiptsPath, message)
}

func (e *csvExporter) SaveAnalysis(model.Analysis) error {
	return nil
}
