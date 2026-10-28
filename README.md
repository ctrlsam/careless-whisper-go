# Careless Whisper Go

Go port of the Careless Whisper delivery-receipt research proof of concept. It
reads existing captures for offline analysis, and drives an authorised live
WhatsApp adapter to fingerprint device state from receipt timing and to run the
silent-reaction denial-of-service flood described in the research.

> [!CAUTION]
> RESEARCH USE ONLY. The live `fingerprint` and `dos` commands send real
> messages to a real phone number. Only run them against devices and phone
> numbers you own or are explicitly authorised to test. Unauthorised use may be
> illegal. Both live commands refuse to run until you pass `--i-own-the-target`
> to confirm ownership or authorisation.

This tool exists to reproduce and demonstrate published research. It is not a
product and ships no guarantees. The attack abuses the fact that delivery
receipts are generated only after a message is decrypted, so their timing leaks
device activity. The same mechanism, driven at high concurrency with oversized
reaction payloads, drives excessive CPU, battery, and mobile-data consumption on
the target.

All credit for the discovery goes to the original authors (see Credits).

## Build

```sh
make build
make test
```

Go 1.26 or later is required. Offline analysis uses only the standard library.
The live adapter depends on `go.mau.fi/whatsmeow` and a pure-Go SQLite driver.

## Analyse an existing capture (offline)

The CLI accepts the Python project's JSON Lines output directly:

```sh
go run ./cmd/careless-whisper analyze \
  --input ../careless-whisper-python/exports/jsonl/<target>_analysis.jsonl
```

The Python two-column RTT CSV format is also supported:

```sh
go run ./cmd/careless-whisper analyze \
  --input ../careless-whisper-python/exports/csv/<target>_delays.csv
```

Input format is inferred from the extension. Override it with
`--format jsonl` or `--format csv`. Prometheus text output is available with
`--output prometheus --phone-number '<target>'`.

## Fingerprint a device you own (live)

The `fingerprint` command sends a continuous stream of silent probe messages to
the target and infers phone/app/online state from receipt timing.

```sh
go run ./cmd/careless-whisper fingerprint \
  --phone-number '<your-test-number>' \
  --i-own-the-target \
  --concurrent 5 \
  --delay 1s \
  --exporter jsonl \
  --metrics
```

On first run the adapter prints a QR code; scan it from WhatsApp under Linked
devices to pair the research account. The code is refreshed automatically until
it is scanned, and the adapter keeps generating fresh codes for the whole
`--pair-timeout` window (default 5m) rather than giving up after one batch, so
there is time to scan several lab phones in turn. The session is cached in
`careless-whisper.db` (override with `--session-path`). With `--metrics` the
latest analysis is exposed as Prometheus text at `http://localhost:8000/metrics`
(`--metrics-port` to change). Press Ctrl+C to stop.

Instead of a QR scan you can link by phone number. Pass `--pair-phone
'<account-number>'` (the research account's own number) and the adapter prints
an 8-character code; on that phone open Linked devices -> Link a device -> Link
with phone number instead, and enter the code. This is the same device-linking
backend as QR, so an account that WhatsApp is temporarily refusing to link
("Can't link new devices right now") can still be refused this way; it is a
different on-phone flow, not a throttle bypass. The code is tied to the same
login socket as the QR batch, so it is refreshed on the same `--pair-timeout`
window.

Exporters write under `--export-dir` (default `exports/`): `jsonl` writes RTT
reports, receipt batches, outgoing message metadata, and analysis snapshots;
`csv` writes the two-column delay file plus a sibling receipt-evidence JSONL.

### Device OS identification

Device OS identification uses observed receipt behaviour from
[Careless Whisper, Section V-D and Table V](https://arxiv.org/html/2411.11194v4#S5.SS4),
not RTT thresholds. To collect the ordinary read receipts needed for this, start
the fingerprint tool, then from the paired account manually send at least three
ordinary visible messages (spaced more than a second apart) to the offline test
phone, reconnect it, and open the chat with read receipts enabled. Silent
reactions alone do not supply this evidence.

## Flood a device you own (live DoS)

The `dos` command floods the target with silent reaction messages, each carrying
a large reaction payload (1 MiB by default), at high concurrency with no receipt
tracking. This forces the target to download and decrypt large volumes of data
per message.

```sh
go run ./cmd/careless-whisper dos \
  --phone-number '<your-test-number>' \
  --i-own-the-target \
  --concurrent 20 \
  --duration 60s
```

Stop conditions: `--duration`, `--max-messages`, or Ctrl+C. Tune throughput with
`--concurrent`, `--delay`, and `--payload-size`. Progress (sent, failed, rate,
estimated data forced onto the target) is logged every `--report-interval`.

Per-client throughput is bounded by the WhatsApp send lock, which is held until
the server acks each message. `--no-ack` returns as soon as the message is
written to the socket, skipping that wait for higher throughput at the cost of
per-message send confirmation. Delivery receipts are unaffected.

The send lock is per client, and a single client's Signal session to the target
cannot be used concurrently, so `--concurrent` alone cannot push past one
in-flight send. `--accounts N` pairs N separate WhatsApp accounts (a distinct
phone number each) and fans sends across them, each with its own identity,
session, socket, and lock, for up to Nx on the lock-bound path. Separate
accounts avoid the own-device sync fan-out of linking many devices to one
account. You scan one QR per account at startup - use a different number for
each - and per-account session files are suffixed `-0`, `-1`, and so on. Each
account gets its own `--pair-timeout` window to scan, so there is time to pair
the lab phones one by one. To link accounts by phone number instead of QR, pass
`--pair-phones '<num0>,<num1>'` (one entry per account in order; leave an entry
empty to keep that account on QR). This scales message rate, not data volume: a
payload large enough to saturate the uplink stays uplink-bound because all
accounts share one uplink.

```sh
go run ./cmd/careless-whisper dos \
  --phone-number '<your-test-number>' \
  --i-own-the-target \
  --accounts 2 \
  --concurrent 20 \
  --no-ack \
  --payload-size 4096
```

## Packages

- `internal/input` - Python-compatible JSONL and CSV capture ingestion.
- `internal/analyzer` - RTT, receipt structure, OS, companion, and impact analysis.
- `internal/collector` - concurrency-safe send/receipt correlation.
- `internal/messenger` - authorised adapter interface and registration gate.
- `internal/messenger/whatsapp` - live whatsmeow WhatsApp adapter.
- `internal/runner` - live fingerprint orchestration.
- `internal/flood` - silent-reaction denial-of-service flood engine.
- `internal/exporter` - JSON, JSONL, and CSV output helpers.
- `internal/metrics` - Prometheus text exposition and a live metrics server.
- `internal/model` - shared capture and analysis types.

## Implementing another authorised adapter

Keep protocol-specific code behind the `messenger.Messenger` interface. Feed
outgoing message IDs via the send path, delivery events via the delivery
callback, and receipt batches via the receipt callback. The runner and flood
engine are provider-agnostic and work against any compliant adapter. Adapters
must enforce a target allowlist, only act against the configured target, and
stop on a bounded duration or request count.

## Credits

This exploit was discovered by Gabriel Karl Gegenhuber, Maximilian Günther,
Markus Maier, Aljosha Judmayer, Florian Holzbauer, Philipp É Frenzel, and
Johanna Ullrich in their paper "Careless Whisper: Exploiting Silent Delivery
Receipts to Monitor Users on Mobile Instant Messengers" (2025).

```bibtex
@article{gegenhuber2025careless,
  title={Careless Whisper: Exploiting Silent Delivery Receipts to Monitor Users on Mobile Instant Messengers},
  author={Gegenhuber, Gabriel Karl and G{\"u}nther, Maximilian and Maier, Markus and Judmayer, Aljosha and Holzbauer, Florian and Frenzel, Philipp {\'E} and Ullrich, Johanna},
  year={2025}
}
```
