# Careless Whisper Go

Go port of the Careless Whisper delivery-receipt research proof of concept. It
reads existing captures for offline analysis, and drives an authorised live
WhatsApp adapter to fingerprint device state from receipt timing and to run the
silent-reaction denial-of-service flood described in the research.

> [!CAUTION]
> RESEARCH USE ONLY. The live `fingerprint` and `dos` commands send real
> messages to a real phone number. Only run them against devices and phone
> numbers you own or are explicitly authorised to test. Unauthorised use may be
> illegal.

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

Every command prints its flags with `--help`:

```sh
go run ./cmd/careless-whisper fingerprint --help
```

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

## Pair a research account

Live commands need a linked WhatsApp "companion" session. Pair one once with the
`pair` command and reuse it across runs:

```sh
go run ./cmd/careless-whisper pair --account '<research-account-number>'
```

Scan the QR from that account under WhatsApp -> Linked devices. On success the
session is saved as `<number>.db` in the session directory (default
`~/.careless-whisper`; override with `--session-dir` or the
`CARELESS_WHISPER_HOME` environment variable) and the command exits cleanly,
checkpointing the store.

Session files are named by the number they are bound to, so
`ls ~/.careless-whisper` shows which accounts are linked. Re-run `pair` any time
to confirm a session is still good - a valid one reconnects with no QR and
reports the bound number; an unlinked one fails fast and tells you to re-pair.
`--repair` discards the stored session and links fresh.

Link by phone-number code instead of QR with
`--pair-phone '<research-account-number>'`: the command prints an 8-character
code to enter on that phone under Linked devices -> Link a device -> Link with
phone number instead. Code pairing uses the same device-linking backend as QR,
so an account WhatsApp is refusing to link ("Can't link new devices right now")
is refused either way; it is a different on-phone flow, not a throttle bypass.

A session persists across runs on its own: keep the research phone linked (do
not sign out or unlink), powered on within WhatsApp's ~14-day linked-device
window, and do not delete the file.

## Fingerprint a device you own (live)

The `fingerprint` command sends a continuous stream of silent probe messages to
the target and infers phone/app/online state from receipt timing.

```sh
go run ./cmd/careless-whisper fingerprint \
  --phone-number '<your-test-number>' \
  --concurrent 5 \
  --delay 1s \
  --exporter jsonl \
  --metrics
```

The research session is resolved from the session directory: with a single
paired session it is used automatically, otherwise pass `--account '<number>'`
to pick one (or `--session-path` for an explicit file). The session must already
be linked with `pair` (see above); run commands never pair, so a missing or
unlinked session fails fast rather than dropping into a QR prompt. A healthy
session connects quickly; `--connect-timeout` (default 30s) bounds the wait.
With `--metrics` the latest analysis is exposed as Prometheus text at
`http://localhost:8000/metrics` (`--metrics-port` to change). Press Ctrl+C to
stop.

Each analysis line reports phone/app/online state, the mean RTT, jitter, and a
best-effort access-network guess (`net`). The Wi-Fi/cellular guess follows the
paper's jitter and baseline-step signal, is only set when the app is foreground
and online (a backgrounded app's RTT is push-wake latency, not the radio), and
marks a likely handover with a trailing `*`. Treat the static Wi-Fi/cellular
label as qualitative, not measured.

### Probe primitive

`--probe-kind` selects the silent probe (default `revoke`). `revoke` and
`empty-reaction` both reference a phantom message with nothing to show, so they
elicit a delivery receipt without rendering anything on the target. `reaction`
reacts to a phantom message with an emoji, which makes the target display a
"Waiting for this message" placeholder per probe - so it is NOT silent and is
kept only for A/B comparison. All kinds produce the same receipt-timing signal.

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
  --concurrent 20 \
  --duration 60s
```

Stop conditions: `--duration`, `--max-messages`, or Ctrl+C. Tune throughput with
`--concurrent`, `--delay`, and `--payload-size`. Progress is logged every
`--report-interval`.

The reported data figure is `sent x payload-size` - an upper bound, not a
measurement. It overstates the real figure by whatever share of sends the
server rejected or never delivered. Two things make the gap large: a payload
much over WhatsApp's accepted message size (roughly 64 KiB) is rejected
server-side and never reaches the target, and `--no-ack` counts a message as
sent the moment it is written to the socket, so server rejections are not even
counted as failures. The tool warns when `--payload-size` is likely to be
rejected, and labels the figure "unconfirmed" under `--no-ack`. For a figure
that tracks what the target actually received, run without `--no-ack` (so
rejected sends count as failures) and keep `--payload-size` within the accepted
range, then confirm against the device's own data counter.

Per-client throughput is bounded by the WhatsApp send lock, which is held until
the server acks each message. `--no-ack` returns as soon as the message is
written to the socket, skipping that wait for higher throughput at the cost of
per-message send confirmation. Delivery receipts are unaffected.

The send lock is per client, and a single client's Signal session to the target
cannot be used concurrently, so `--concurrent` alone cannot push past one
in-flight send. Fanning across several accounts - each with its own identity,
session, socket, and lock - gives up to Nx on the lock-bound path, and avoids
the own-device sync fan-out of linking many devices to one account. Pair each
account first with `pair` (one per number), then select them with
`--account '<num0>,<num1>'`; each resolves to its `<number>.db` in the session
directory. With neither `--account` nor `--session-path`, a single paired
session in the directory is used automatically; if several are paired the
command stops and asks you to name them with `--account`, so it never floods
from more accounts than you intended. `--session-path` selects one explicit
session file. As with `fingerprint`, every account must already be linked with
`pair` - `dos` does not pair - and `--connect-timeout` (default 30s) bounds the
per-account connect wait. This scales message rate, not data volume: a payload
large enough to saturate the uplink stays uplink-bound because all accounts
share one uplink.

```sh
go run ./cmd/careless-whisper dos \
  --phone-number '<your-test-number>' \
  --account '<num0>,<num1>' \
  --concurrent 20 \
  --no-ack \
  --payload-size 4096
```

## Packages

- `internal/input` - Python-compatible JSONL and CSV capture ingestion.
- `internal/analyzer` - RTT, receipt structure, OS, companion, access-network, and impact analysis.
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
