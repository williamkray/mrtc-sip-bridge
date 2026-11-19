# CLAUDE.md

Guidance for agents working in this repository.

## What this is

mrtc-sip-bridge bridges a **SIP phone account** (first target: JMP.chat SIP credentials) to **Matrix
calls via MatrixRTC** (Element Call / LiveKit). Inbound calls ring every Matrix client of the owner.
Outbound calls are placed from portal rooms. It handles calls only: texting for the same numbers lives in
the sibling project `../mautrix-jabber` (Cheogram).

## Status

Phases 1 (skeleton), 2 (inbound calls), 3 (outbound calls, DTMF) and J (embedding in jmpchat-matrix-bridge (formerly mautrix-jabber),
`docs/PHASEJ-SPEC.md`) are implemented and were validated against a live JMP account and a
Synapse/LiveKit deployment. Host contract: `docs/EMBEDDING.md`.
See the **Current state** section of `docs/PLAN.md`. Start here:

1. `docs/PLAN.md`: architecture, call flows, phased checklist, and the **Current state** section.
   Keep that section and the checkboxes up to date as you work.
2. `docs/DECISIONS.md`: why SIP rather than Jingle, why MatrixRTC rather than legacy `m.call`, why a
   self-contained Go media path, why bridgev2. Don't quietly go against an ADR. Add a new ADR that
   supersedes it.
3. `docs/RESEARCH.md`: JMP SIP specifics, prior art (lhns/matrix-sip-bridge is the closest; read its
   `pkg/calls`), library choices, and sources. Items marked **unverified** have to be confirmed before
   code depends on them.
4. `docs/CREDITS.md`: dependency and prior-art licenses and attribution rules. Read it **before
   copying any outside code**. Element Call is AGPL and mumabridge has no license: don't copy either.

The call core is a **library with two hosts** (ADR-006): this repo's standalone bridgev2 bridge, and an
optional embedded mode inside jmpchat-matrix-bridge (unified text and call rooms). Keep `pkg/` core packages
free of host-specific imports.


## Conventions

- Go, mautrix bridgev2 (same version line as `../mautrix-jabber`, currently v0.33.x). Copy that project's
  build approach (Docker, cgo) and its telemetry conventions (`../mautrix-jabber/pkg/telemetry`,
  described in its CLAUDE.md).
- Build, vet and test with `-tags "nocrypto nolibopusfile"` (no libolm, rooms are unencrypted; hraban/opus
  without opusfile). cgo and libopus (`pkg-config opus`) are required.
- Portal rooms must be **unencrypted** (encrypted rooms give silent Element Call audio, see ADR-002).
- Get LiveKit tokens through the MatrixRTC auth service (lk-jwt-service) as the ghost user. Never mint
  them from the LiveKit API secret.
- Keep MatrixRTC unstable event type names and field names in one place, citing the MSC and the
  Element Call version they were checked against.
- Legacy `m.call.*` is out of scope. Only LiveKit-based MatrixRTC is supported.
- Add every new dependency to `docs/CREDITS.md` with its license.
- Never log or trace audio, DTMF digits or credentials. Phone numbers are allowed.
- Your real JMP SIP credentials, and any ATA config, are the source of truth for registrar
  host/port. The 2021 values in RESEARCH.md may be stale.
- The repo is public under the MIT license (`LICENSE`). Third-party licenses: `THIRD-PARTY-NOTICES.md` and
  `docs/CREDITS.md`. Never commit secrets, real phone numbers or real hostnames; use the `example.org` /
  `+1555...` placeholders.

## Telemetry

Optional OpenTelemetry tracing and metrics plus log correlation. Spec and source of truth for span and
metric names: `docs/TELEMETRY-SPEC.md`. User docs: README "Tracing and metrics (OpenTelemetry)". Two
packages, split per ADR-006:

- **`pkg/obs/`** (core, **API only**): `Use(Providers)`, `Enabled`/`MetricsEnabled`, `Start`/`StartChild`/
  `StartLinkedRoot`, `CarrySpan`/`WithParent`, `Fail`, `Logger`/`LoggerSC`, `Scrub`,
  `WrapHTTPTransport`/`HTTPAttrs`, and the instrument registry (`obs.M()`, `mrtcsip.*` metrics). It
  imports only the OTel API modules (`otel`, `attribute`, `codes`, `trace`, `metric`, `propagation`, and
  the `noop` packages). Its export list is fixed by the spec; no transitional exports.
- **`pkg/telemetry/`** (host, SDK): `Init`/`Shutdown`, env resolution for both signals, resource, OTLP
  exporters, temporality, error handler, logr sink, `WrapMatrixTransport`, Go runtime metrics. Only the
  standalone host (`cmd/mrtc-sip-bridge`) uses it, and it calls `obs.Use`. The embedded host
  (jmpchat-matrix-bridge `sipcalls` build) also goes through `telemetry.Init` with `Options.ServiceName`.

Rules:

- Core packages (`pkg/core`, `pkg/sipua`, `pkg/matrixrtc`, `pkg/phone`, `pkg/obs`, later `calls`, `rtc`,
  `media`) never import `go.opentelemetry.io/otel/sdk...`, `.../otel/exporters/...`,
  `go.opentelemetry.io/contrib/...` or `pkg/telemetry`, and never call `otel.SetTracerProvider`,
  `SetMeterProvider`, `SetTextMapPropagator`, `SetLogger` or `SetErrorHandler`. `internal/importcheck`
  enforces the imports. Core packages get tracers, meters and log helpers only through `pkg/obs`.
- Off unless an OTLP endpoint env var is set (config is env-only, no config-file keys); when off nothing
  is installed and helpers are no-ops that return the input ctx and logger unchanged. `Init` never fails
  the bridge (problems log one warning and leave that signal off). Traces and metrics are enabled
  independently.
- Every span is **root** (entry points only) or **child-only** (`obs.StartChild`, no-op without a parent)
  as classified in the spec catalogue, to avoid orphan one-span traces. One span name per operation (use
  an attribute for the trigger). Goroutines never receive a live span: use `obs.CarrySpan` and start
  their own span, or a linked root. No span with children stays open past call answer. Successful
  REGISTER refreshes are metrics only, never spans.
- Never block or do I/O or round trips from telemetry on the sipgo/diago handler goroutines, RTP loops,
  LiveKit callbacks, the appservice event path or `sipua.Config.OnStateChange`. Hot loops update atomics
  and flush to instruments on a ticker; never call an instrument per packet.
- Privacy: never record audio, SDP bodies, raw SIP, DTMF digits (also not via bot command arguments),
  SIP passwords/digest values, OpenID tokens, LiveKit JWTs, AS/HS tokens, Matrix event content, message
  bodies or full URLs; the SIP username only masked (`sip.user.masked`). Phone numbers and MXIDs are
  allowed on spans and logs, **never on metrics** (closed-enum, low-cardinality attributes only). Route
  errors through `obs.Fail` and strings through `obs.Scrub`.
- Log correlation: root spans annotate the ctx logger (`zerolog.Ctx`) with `trace_id`/`span_id`/
  `dd.trace_id`/`dd.span_id`; our own lines under a span use the component's `logFor(ctx)` =
  `obs.Logger(ctx, &c.log)` where `c.log` is a clean logger, never `c.log` directly. Never pass a logger
  derived from `zerolog.Ctx(ctx)` to `obs.Logger`. Goroutines with only a stored span context use
  `obs.LoggerSC`.
- Docs stay in step: new spans or metrics are added to the README "Tracing and metrics" section in the
  same change (definition of done per phase: `docs/TELEMETRY-SPEC.md` §8.2). New OTel-related
  dependencies go in `docs/CREDITS.md`.
