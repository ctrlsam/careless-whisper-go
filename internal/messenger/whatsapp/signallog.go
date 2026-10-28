package whatsapp

import (
	"sync"

	signallog "go.mau.fi/libsignal/logger"
)

// silenceSignalOnce guards one-time suppression of the libsignal global logger.
var silenceSignalOnce sync.Once

// noopSignalLogger discards all libsignal log output.
type noopSignalLogger struct{}

func (noopSignalLogger) Debug(string, string)   {}
func (noopSignalLogger) Info(string, string)    {}
func (noopSignalLogger) Warning(string, string) {}
func (noopSignalLogger) Error(string, string)   {}
func (noopSignalLogger) Configure(string)       {}

// silenceSignalLogging mutes the libsignal package logger. It logs decryption
// failures for inbound messages through its own global logger, separate from
// whatsmeow. This tool only ever sends, so inbound decrypt noise (for example
// own-device sync or history-sync messages) is irrelevant and would otherwise
// flood the console during a run.
func silenceSignalLogging() {
	silenceSignalOnce.Do(func() {
		var logger signallog.Loggable = noopSignalLogger{}
		signallog.Setup(&logger)
	})
}
