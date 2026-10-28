package exporter

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

func TestSafeIdentifier(t *testing.T) {
	if got := SafeIdentifier("../../target"); got != "______target" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteAnalysis(t *testing.T) {
	var output bytes.Buffer
	analysis := model.Analysis{AnalyzedAt: time.Now(), DeviceType: model.DeviceIPhone}
	if err := WriteAnalysis(&output, analysis, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"device_type":"iPhone"`) {
		t.Fatalf("unexpected output: %s", output.String())
	}
}
