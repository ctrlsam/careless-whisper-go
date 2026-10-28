package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/analyzer"
	"github.com/ctrlsam/careless-whisper-go/internal/exporter"
	inputpkg "github.com/ctrlsam/careless-whisper-go/internal/input"
	"github.com/ctrlsam/careless-whisper-go/internal/metrics"
)

const version = "0.1.0"

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}
	switch args[0] {
	case "analyze":
		return analyzeCommand(args[1:], stdout)
	case "fingerprint":
		return fingerprintCommand(args[1:], stdout)
	case "dos":
		return dosCommand(args[1:], stdout)
	case "version":
		_, err := fmt.Fprintln(stdout, version)
		return err
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func analyzeCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("analyze", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inputPath := flags.String("input", "", "capture file in JSONL or CSV format")
	format := flags.String("format", "auto", "input format: auto, jsonl, or csv")
	pingInterval := flags.Duration("ping-interval", 2*time.Second, "probe interval represented by the capture")
	outputFormat := flags.String("output", "json", "output format: json or prometheus")
	phoneNumber := flags.String("phone-number", "", "target label for Prometheus output")
	compact := flags.Bool("compact", false, "emit compact JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *inputPath == "" {
		return errors.New("--input is required")
	}
	capture, err := inputpkg.Load(*inputPath, *format)
	if err != nil {
		return fmt.Errorf("load capture: %w", err)
	}
	analysis := analyzer.Analyze(capture, analyzer.Options{PingInterval: *pingInterval})
	switch *outputFormat {
	case "json":
		return exporter.WriteAnalysis(stdout, analysis, !*compact)
	case "prometheus":
		return metrics.WritePrometheus(stdout, *phoneNumber, analysis)
	default:
		return fmt.Errorf("unsupported output format %q", *outputFormat)
	}
}

func printUsage(writer io.Writer) {
	usage := map[string]any{
		"name":        "careless-whisper",
		"description": "Delivery-receipt timing research toolkit (RESEARCH USE ONLY)",
		"disclaimer":  disclaimerText,
		"commands": map[string]string{
			"analyze":     "Analyse a Python-compatible JSONL or CSV capture (offline)",
			"fingerprint": "Live receipt-timing fingerprint against an authorised target",
			"dos":         "Silent-reaction denial-of-service flood against an authorised target",
			"version":     "Print the version",
		},
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(usage)
}
