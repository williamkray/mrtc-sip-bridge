# Telemetry spec: OpenTelemetry tracing, metrics and log correlation

Status: **proposed** (written 2026-10-16). Audience: the coordinator and implementer agents for task
T6 (telemetry foundation) and for the per-phase instrumentation in Phases 2, 3, 4 and J.
Supersedes the Phase 5 "OpenTelemetry tracing" bullets in `docs/PLAN.md`: telemetry moves to right
after Phase 1 T4 and becomes part of the definition of done of every later phase.

Inputs: `docs/PLAN.md`, ADR-003/004/006/009 in `docs/DECISIONS.md`, `docs/PHASE1-SPEC.md` (APIs this
spec instruments; that spec is not changed by this document), and the sibling project
`../mautrix-jabber` (`pkg/telemetry/*.go`, its CLAUDE.md "Telemetry" section, README "Tracing
(OpenTelemetry)", commits `e4b3f24`..`e812e20`).

Goals:

- Full observability of the stack (SIP signalling, MatrixRTC signalling, LiveKit, media quality,
  Matrix HTTP) with **vanilla OpenTelemetry** (OTLP to any collector) and OTel-compatible commercial
  backends, Datadog first among them.
- Zero cost and zero behaviour change when telemetry is not configured.
- Works identically when the call core is embedded in jmpchat-matrix-bridge (formerly mautrix-jabber) (ADR-006): core spans and
  metrics join jabber's pipeline, and service identity is jabber's.

---

## 1. Lessons from jmpchat-matrix-bridge and how this project avoids them

| # | What went wrong in jabber | Fixed in | What we do from day one |
|---|---|---|---|
| L1 | **Orphan root spans.** Leaf operations called outside a traced event (e.g. `ConvertMessage` called by the framework without a `handle_remote_event` parent) each started their own root trace, flooding the backend with one-span traces. | `a3848fa` (child-only `bridge.convert_message`) | Every span is classified **root** (entry points only) or **child-only** in the catalogue (§5). Child-only spans use `obs.StartChild`, which returns a no-op span when ctx has no valid span. Tests assert no unexpected roots. |
| L2 | **Trace-shape seams.** A send-triggered backfill started `mam.backfill_on_send` and then a nested `mam.backfill_on_view`, two names for one operation; the session span was planned but missing. | `a3848fa` | One span name per operation, with a trigger attribute (`call.trigger`, `cmd.name`) instead of per-trigger names. Background work triggered by traced work is a **linked root**, never a nested second root. The catalogue is the contract; reviews diff instrumentation against it. |
| L3 | **Blocking the I/O goroutine.** The new-portal backfill trigger did a DB lookup plus a disco#info IQ from the mellium Serve goroutine. The IQ reply could only be read by the goroutine it was blocking, so inbound XMPP stalled 5 s per new contact. | `81c1045` | Telemetry never adds I/O, locks held across I/O, or round trips to: sipgo/diago transaction handlers, the RTP read/write loops, LiveKit track callbacks, the appservice sync event path (`RTCEventSink` must not block, PHASE1-SPEC §4.4), or `sipua.Config.OnStateChange`. Only the batching span processor and periodic metric reader are used (never `SimpleSpanProcessor`). Hot loops record into atomics and flush to OTel instruments on a ticker (§4.5). |
| L4 | **OTLP env values leaking into logs.** The OTel SDK's default stderr logger printed raw key/value pairs, including a malformed `OTEL_EXPORTER_OTLP_HEADERS` carrying an API key. | `5cb368d` | `otel.SetLogger` is replaced with a zerolog sink that drops all key/value pairs **before** any exporter is constructed (exporters parse env at construction). Ported verbatim with its test. |
| L5 | **URLs in span errors.** Errors from media downloads embedded `mxc://` URIs (and http URLs with tokens). | `5cb368d` | `obs.Fail` scrubs URLs to `scheme://host`, and additionally scrubs `sip:`/`sips:` userinfo passwords, `Bearer` tokens and JWT-shaped strings (§6.3). |
| L6 | **Log correlation added late, then a duplicate-field bug class.** Correlation first only put `trace_id`/`dd.trace_id` on root ctx loggers. Adding `span_id` meant rebuilding loggers without writing a key twice, solved by the `logAnnot` identity scheme and the rule "never pass a logger derived from `zerolog.Ctx(ctx)` to `telemetry.Logger`". Four follow-up commits switched call sites to `x.logFor(ctx)`. | `a0d9022`, `ff19901`, `bb9e1a0`, `a0ea782`, `43547f3` | Port the final helpers (`Logger`, `LoggerSC`, `withLogFields`, all four fields) in T6 before any instrumented code exists. Every component holds one clean logger and logs under spans via `logFor(ctx)` from the first line (§7). |
| L7 | **Temporary shim left behind.** `AddLogFields` was exported as a shim for one caller, then removed. | `cb7e610` | No exported transitional helpers. If a migration needs one, it lives unexported in the caller's package. The export list of `pkg/obs` is fixed by §3.3. |
| L8 | **Missing build metadata.** `.git` is excluded from the Docker context, so `Commit`/`Tag`/`BuildTime` were never set; no `git.commit.sha` on the resource for Datadog source-code integration. | `e812e20` | PHASE1-SPEC §7.14/§8 already passes git metadata as build args. T6 wires `Commit` and `RepositoryURL` into the resource (`git.commit.sha`, `git.repository_url`, overridable by `DD_GIT_COMMIT_SHA`/`DD_GIT_REPOSITORY_URL`). |
| L9 | **Docs drifted from the instrumentation** (README described spans that did not ship exactly as written). | `394aeb4` | The README "Tracing and metrics" section is generated from the catalogue in this spec and updated in the same change as the instrumentation. Definition of done includes it (§8). |
| L10 | **Protocol guessing.** Users pointed `http/protobuf` at `:4317`. | `e4b3f24` (port hint warning) | Same rule: protocol defaults to `http/protobuf` per the OTel spec, never inferred from the port; a mismatched port logs one startup warning. |
| L11 | **Framework-handled events lose the trace.** bridgev2 handles queued remote events on its own goroutine, so jabber needed `MutateContextFunc`/`PostHandleFunc` hooks (`bridge.handle_remote_event`) to join the trace. | `e4b3f24` | We have few remote events (call notices only). Notices are sent by the host inside the core's span ctx (`CallHost.SendCallNotice(ctx, ...)`), so the HTTP spans are children directly. If a host ever queues remote events, it reuses jabber's hook pattern. |

---

## 2. Configuration and initialisation (host side)

### 2.1 Environment only, identical to jmpchat-matrix-bridge

No config-file keys. Telemetry is configured only through standard environment variables. Traces and
metrics are decided independently.

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Enables traces **and** metrics (unless disabled per signal). Base URL; for `http/protobuf` the exporters append `/v1/traces` and `/v1/metrics`. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Per-signal endpoint, used as-is; enables only that signal if the base is unset. |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_{TRACES,METRICS}_PROTOCOL` | `grpc` or `http/protobuf` (default `http/protobuf`, never inferred from the port). Other values (e.g. `http/json`) warn and disable that signal. Port 4317 with http, or 4318 with grpc, logs one warning. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `_TIMEOUT`, `_COMPRESSION`, `_CERTIFICATE`, `_INSECURE` (+ per-signal) | Handled by the OTLP exporters themselves. |
| `OTEL_SDK_DISABLED=true` | Disables everything. Unparseable value warns and counts as false. |
| `OTEL_TRACES_EXPORTER` | Unset or `otlp`: normal. `none`: traces off. Anything else: warn, traces off. `otlp` alone does not enable. |
| `OTEL_METRICS_EXPORTER` | Same rules for metrics (`prometheus` and `console` are not supported: warn, metrics off). |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Read by the SDK (default `parentbased_always_on`). |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT` | Read by the SDK periodic reader (default 60000 ms / 30000 ms). |
| `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` | Parsed **by us** (`cumulative` default, `delta`, `lowmemory`, case-insensitive; other values warn and use cumulative) and passed as an explicit temporality selector, so behaviour does not depend on exporter version. Datadog recommends `delta`. |
| `OTEL_BSP_*` | Batch span processor tuning (SDK). |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Override everything below (`resource.WithFromEnv`). |
| `DD_SERVICE`, `DD_VERSION`, `DD_ENV` | Fallbacks for `service.name`, `service.version`, `deployment.environment.name` + `deployment.environment`. |
| `DD_GIT_COMMIT_SHA`, `DD_GIT_REPOSITORY_URL` | Override the build-stamped `git.commit.sha` / `git.repository_url`. |

`OTEL_LOGS_EXPORTER` is not supported: logs stay on stdout as JSON, correlated by ids (§7).
`DD_AGENT_HOST` alone does nothing (documented, as in jabber).

Resource precedence: defaults (`service.name=mrtc-sip-bridge`, `service.version=<bridge version>`,
`git.*` from linker vars, telemetry SDK attrs) < `DD_*` < `OTEL_*`.

Off unless an endpoint is set: when neither signal is enabled, **nothing is installed** (no provider,
goroutine, socket, transport wrapping or global registration) and every `pkg/obs` helper returns its
input ctx unchanged (identity, tested).

### 2.2 Init never fails the bridge

- `telemetry.Init` returns nothing. Every problem logs **one** warning and leaves that signal off.
- Exporter construction never dials synchronously (gRPC dials lazily); an unreachable collector must
  not delay startup (test: `TestInitUnreachableShutdown`, ported).
- The OTel error handler is rate-limited (first error, then one line per 5 min with a suppressed count),
  URL-stripped.
- One info line per enabled signal: protocol, `scheme://host:port` only (never the path, query or
  headers), sampler / export interval.
- `Shutdown` flushes both providers, bounded to 3 s total, idempotent, a no-op when off. Called from
  `SIPConnector.Stop()` (`bridgev2.StoppableNetwork`, called after all clients disconnect, so the
  unregister spans are flushed).

### 2.3 Port `pkg/telemetry` from jmpchat-matrix-bridge: yes, split in two

Porting is fine (same author, no licence issue) and is the fastest way to inherit the fixes in §1.
But jabber's package mixes two things that ADR-006 needs separated: SDK installation (host only) and
span/log helpers (needed by the core). So the port is split:

| Package | Kind | Contents | Ported from |
|---|---|---|---|
| `pkg/obs` | **core** (API only) | `Use(Providers)`, `Enabled`, `Start`, `StartChild`, `StartLinkedRoot`, `CarrySpan`, `WithParent`, `Fail`, `Logger`, `LoggerSC`, log annotation, URL/secret scrubbing, `WrapHTTPTransport` (generic client spans + traceparent), `HTTPAttrs`, instrument registry (§4). | jabber `span.go`, `http.go` (generalised), parts of `telemetry.go` |
| `pkg/telemetry` | **host** (SDK) | `Init(ctx, Options)`, `Shutdown`, env resolution for both signals, resource, error handler, `otelLogSink`, `WrapMatrixTransport` (thin wrapper over `obs.WrapHTTPTransport` with the Matrix path templater). Calls `obs.Use` on success. | jabber `telemetry.go`, `env.go`, `http.go` |

Changes relative to jabber:

1. `TracerName` becomes the instrumentation scope `github.com/williamkray/mrtc-sip-bridge` with
   `trace.WithInstrumentationVersion(<module version>)`; same scope name for the meter.
2. Default `service.name` is `mrtc-sip-bridge`.
3. Metrics support (OTLP metric exporters, periodic reader, temporality selector, `OTEL_METRICS_*`).
4. The package-global tracer and `enabled` flag move to `pkg/obs` and are set via `obs.Use`, so the
   core never needs to import the host package.
5. `WrapHTTPTransport` takes a path templater (`func(path string) string`); the Matrix templater is
   jabber's `urlTemplate`. lk-jwt-service uses a templater that maps everything to its fixed endpoint
   paths. The transport also records `http.client.request.duration` (§4).
6. `stripURLs` grows the extra scrubbers of §6.3.
7. `StartRemoteEvent`/`EndRemoteEvent` are not ported until a host needs them (L11).

---

## 3. Library vs host split (ADR-006)

### 3.1 Rules

1. **Core packages** (`pkg/core`, `pkg/sipua`, `pkg/matrixrtc`, `pkg/phone`, `pkg/obs`, and later
   `pkg/calls`, `pkg/rtc`, `pkg/media`) may import only the OTel **API** modules:
   `go.opentelemetry.io/otel` (for `otel.Handle` only, not `otel.SetTracerProvider` etc.),
   `.../otel/attribute`, `.../otel/codes`, `.../otel/trace`, `.../otel/trace/noop`,
   `.../otel/metric`, `.../otel/metric/noop`, `.../otel/propagation`. They never import
   `go.opentelemetry.io/otel/sdk...`, `.../otel/exporters/...`, `go.opentelemetry.io/contrib/...`, or
   `pkg/telemetry`, and never call `otel.SetTracerProvider`, `otel.SetMeterProvider`,
   `otel.SetTextMapPropagator`, `otel.SetLogger` or `otel.SetErrorHandler`.
2. Core packages other than `pkg/obs` get tracers, meters and log helpers **only through `pkg/obs`**,
   never `otel.Tracer(...)`/`otel.GetTracerProvider()` directly (one choke point for the enabled gate
   and naming).
3. The **host** owns SDK init, resource, exporters, propagator choice and shutdown: the standalone
   `cmd/mrtc-sip-bridge` via `pkg/telemetry`, or jmpchat-matrix-bridge via its own `pkg/telemetry`.
4. Enforced by `internal/importcheck` (T6 extends it: skip `pkg/telemetry` like `pkg/connector` when
   collecting core dirs, and forbid the prefixes above in core files; `_test.go` files may import
   `sdk/trace/tracetest` and `sdk/metric`).

### 3.2 Injection contract

```go
package obs

// Providers are installed by the host once at startup, before any login starts.
// A nil Tracer disables tracing; a nil Meter disables metrics. Never calling Use is valid (all off).
type Providers struct {
	Tracer trace.TracerProvider
	Meter  metric.MeterProvider
}

// Use installs p. Safe to call once; a second call replaces the providers (tests). It creates the
// tracer and (re)creates every instrument of §4 from p.Meter.
func Use(p Providers)

func Enabled() bool        // tracing on
func MetricsEnabled() bool // metrics on
```

Logger injection: core types take a **clean** `zerolog.Logger` in their config (as `sipua.Config.Log`
already does, PHASE1-SPEC §6.1; `calls`, `rtc`, `media` configs follow suit). Core code logs under a
span with `obs.Logger(ctx, &c.log)` (wrapped as an unexported `c.logFor(ctx)`), never `c.log` directly,
never `zerolog.Ctx(ctx)` as the base.

Standalone host, in `PostInit`:

```go
telemetry.Init(context.Background(), telemetry.Options{
	ServiceVersion: br.Version, CommitSHA: knownOrEmpty(Commit), RepositoryURL: knownOrEmpty(RepositoryURL),
	Log: *br.Log,
}) // calls obs.Use internally when a signal is enabled
if obs.Enabled() {
	hc := br.Matrix.AS.HTTPClient
	hc.Transport = telemetry.WrapMatrixTransport(hc.Transport)
}
```

Embedded host (jmpchat-matrix-bridge, Phase J): jabber's sipcalls telemetry backend calls `telemetry.Init` of
this repo with `Options.ServiceName = "jmpchat-matrix-bridge"`, which calls `obs.Use`; jabber's annotation
helpers delegate to `obs.AnnotateLogger`/`Logger`/`LoggerSC` (PHASEJ-SPEC §4.3).

Result: core spans use the same provider, resource (`service.name=jmpchat-matrix-bridge`), sampler and
exporter as jabber's own spans. Core span names are prefixed (`sip.`, `call.`, `rtc.`, `matrixrtc.`, `media.`) so they do not
collide with jabber's (`xmpp.`, `mam.`, `bridge.`, `cmd.`). The instrumentation scope tells them apart.
Core calls into the host (`CallHost.ResolveCallRoom`, `SendCallNotice`, `GhostIntent.*`) always pass
the span ctx, so jabber's `WrapMatrixTransport` (child-only) produces HTTP spans under core spans.

**Log-annotation convergence (resolved by PHASEJ-SPEC §4.3).** jabber's sipcalls build has no
annotation key of its own: its root-span annotation calls `obs.AnnotateLogger` and its `Logger`/
`LoggerSC` delegate to `pkg/obs`, so a jabber root over an `obs`-annotated ctx (and the reverse) writes
the trace fields exactly once. jabber's untagged (public) build keeps its own tracing-only backend and
never sees an `obs`-annotated ctx.

### 3.3 `pkg/obs` exported API (fixed; no transitional exports, L7)

`Providers`, `Use`, `Enabled`, `MetricsEnabled`, `Start`, `StartChild`, `StartLinkedRoot`,
`CarrySpan`, `WithParent`, `Fail`, `Logger`, `LoggerSC`, `AnnotateLogger` (amendment, Phase J: the
root-span log annotation of `Start`, exported for hosts with their own tracer), `Scrub` (string scrubber
of §6.3),
`WrapHTTPTransport`, `HTTPAttrs`, `Instruments` accessor (`obs.M()` returning the struct of §4.3),
attribute key constants (`obs.AttrCallID`, ..., including the MatrixRTC 2.0 keys of §5.2: `AttrMatrixRTCFormat`, `AttrMatrixRTCSlotID`, `AttrMatrixRTCSlotAction`, `AttrMatrixRTCMemberType`, `AttrMatrixRTCDelayedLeave`, `AttrMatrixRTCLeaveVia`, `AttrMatrixRTCPlan*`, `AttrMatrixRTCStickySupported`, `AttrMatrixRTCDelayedSupported`, `AttrMatrixRTCTransports`, `AttrRTCTokenEndpoint`, `AttrRTCTokenFallback`). `Instruments` gains `DelayedLeaveRestarts` (§4.3). Semantics of the span/log helpers are exactly jabber's
(`span.go` at `e812e20`), including: `Start` annotates the ctx logger only for new roots or new traces;
annotation is skipped for disabled loggers and `zerolog.DefaultContextLogger`; only sampled, valid span
contexts produce log fields.

---

## 4. Metrics

### 4.1 Recommendation: yes, add OTel metrics, from T6 on

Reasons: call quality questions ("how many calls were missed this week", "is jitter getting worse",
"how long does LiveKit join take") are aggregates that traces answer badly, especially with sampling.
OTLP metrics are accepted by vanilla collectors (and Prometheus via the collector) and by the Datadog
Agent's OTLP receiver on the same ports as traces, so there is no extra deployment cost. Metrics are
the only signal that is safe to keep at full fidelity for media quality without per-packet spans.

Constraints:

- **Low cardinality, no identities.** Metric attributes never contain phone numbers, MXIDs, room ids,
  event ids, call ids, SIP Call-IDs, login ids, SIP usernames, IP addresses or LiveKit room names.
  Allowed attribute values are closed enums listed below (tested with an allowlist, §8.4).
- Units are UCUM (`s`, `{call}`, `{packet}`, `1`). Durations are seconds (float histograms), per OTel
  semantic conventions.
- Histogram buckets are set at the API level with `metric.WithExplicitBucketBoundaries` (advisory, so
  the core needs no SDK views). The Datadog Agent maps OTLP histograms to distributions by default;
  vanilla backends get explicit-bucket histograms.
- Datadog: set `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta`. Cumulative also works (the
  Agent converts monotonic sums, dropping the first point).
- In embedded mode metrics are a no-op until jabber's telemetry installs a MeterProvider (J task).

### 4.2 Hot-path rule

RTP and LiveKit sample loops never call OTel instruments per packet. They update per-call atomics; a
per-call 5 s ticker (and call end) adds the deltas to the counters/histograms using pre-built
`metric.WithAttributeSet` options (no per-call allocation of attribute sets). Observable instruments
read cheap in-memory state only.

### 4.3 Instruments

Prefix `mrtcsip.` (namespaced so they are distinguishable in a jabber-hosted pipeline; Datadog keeps the
dots). Attributes are listed with their closed value sets.

| Name | Instrument | Unit | Attributes | Recorded when | Phase |
|---|---|---|---|---|---|
| `mrtcsip.sip.register.attempts` | Counter | `{attempt}` | `sip.register.kind` (full, refresh, unregister), `outcome` (ok, auth, transient) | every REGISTER transaction result | T6 |
| `mrtcsip.sip.register.duration` | Histogram | `s` | `sip.register.kind`, `outcome` | per attempt, digest round included. Buckets 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 | T6 |
| `mrtcsip.sip.registrations` | ObservableGauge | `{registration}` | `state` (registering, registered, auth_failed, transient_error) | count of active UAs per state (reads `UA.State()`) | T6 |
| `mrtcsip.matrixrtc.events` | Counter | `{event}` | `matrixrtc.kind` (call_member, notification, decline, rtc_member, rtc_slot, ...: the `matrixrtc.EventKind` strings), `routed` (true, false) | every ingest handler call | T6 |
| `mrtcsip.bridge.logins` | Counter | `{login}` | `outcome` (ok, auth_failed, transient_error, timeout, error) | login submit result | T6 |
| `mrtcsip.call.started` | Counter | `{call}` | `call.direction` (inbound, outbound) | INVITE received / outbound call accepted for dialing | 2/3 |
| `mrtcsip.call.ended` | Counter | `{call}` | `call.direction`, `call.outcome` (answered, missed, declined, cancelled, timeout, busy, failed, rejected), `sip.response.status_class` (1xx..6xx, none) | call fully torn down | 2/3 |
| `mrtcsip.call.active` | UpDownCounter | `{call}` | `call.direction` | +1 on start, -1 on end | 2/3 |
| `mrtcsip.call.setup.duration` | Histogram | `s` | `call.direction`, `call.outcome` | inbound: INVITE received to 200 OK sent (or final failure); outbound: dial start to 200 OK received. Buckets 0.25, 0.5, 1, 2, 3, 5, 10, 20, 30, 60 | 2/3 |
| `mrtcsip.call.ring.duration` | Histogram | `s` | `call.direction`, `call.outcome` | inbound: ring sent to owner join / decline / cancel / timeout; outbound: first 180/183 to 200 or failure. Same buckets as setup | 2/3 |
| `mrtcsip.call.duration` | Histogram | `s` | `call.direction`, `media.codec` (PCMU, PCMA, other) | answered calls, answer to hangup. Buckets 10, 30, 60, 120, 300, 600, 1800, 3600, 7200 | 2/3 |
| `mrtcsip.rtc.join.duration` | Histogram | `s` | `outcome` (ok, openid_error, token_error, connect_error, publish_error, timeout) | OpenID request through track published. Buckets 0.1, 0.25, 0.5, 1, 2, 3, 5, 10 | 2 |
| `mrtcsip.matrixrtc.delayed_leave.restarts` | Counter | `{restart}` | `outcome` (ok, gone, error) | each restart of a ghost's MSC4140 delayed leave (every 7 s per sticky call). Metric only, never a span (like REGISTER refresh). `gone` = the delayed event no longer exists (404 `M_NOT_FOUND`); other failures, including 429, are `error`. `member.id` is never an attribute | MatrixRTC 2.0 |
| `mrtcsip.media.rtp.packets` | Counter | `{packet}` | `media.direction` (rx, tx) | ticker flush; the `mrtcsip.media.rtp.*` instruments count the **SIP** RTP leg | 2 |
| `mrtcsip.media.rtp.lost` | Counter | `{packet}` | (none) | rx sequence gaps not later filled, ticker flush | 2 |
| `mrtcsip.media.rtp.late` | Counter | `{packet}` | (none) | rx packets discarded by the jitter buffer | 2 |
| `mrtcsip.media.rtp.jitter` | Histogram | `s` | (none) | RFC 3550 interarrival jitter sampled per call every 5 s. Buckets 0.001, 0.0025, 0.005, 0.01, 0.02, 0.04, 0.08, 0.16 | 2 |
| `mrtcsip.media.concealed` | Counter | `{frame}` | `media.leg` (sip_to_rtc, rtc_to_sip) | silence/PLC frames inserted on underrun | 2 |
| `mrtcsip.dtmf.sent` | Counter | `{digit}` | `dtmf.method` (rfc4733) | count of DTMF events sent; **never the digits** | 3 |
| `mrtcsip.widget.requests` | Counter | `{request}` | `widget.action` (session, state, normalize, call, dtmf, hangup, prefs, chat, contact, contacts; PWA: enroll, device_session, device_confirm, forget), `outcome` (ok, unauthorized, forbidden, rate_limited, invalid, busy, not_ready, error; PWA: expired, used, reuse), `widget.client` (widget, pwa) | every Dialpad widget and PWA API request (WIDGET-SPEC §7, PWA-SPEC §7), including rejected ones | W3, P3 |
| `mrtcsip.widget.sessions` | ObservableGauge | `{session}` | (none) | unexpired widget sessions of the running widget servers | W3 |
| `mrtcsip.widget.installs` | Counter | `{room}` | `outcome` (installed, updated, unchanged, removed, failed) | per room the widget installer checks (Ensure, sweeps, removal) | W3 |
| `mrtcsip.widget.device_links` | ObservableGauge | `{link}` | `state` (pending, active) | unexpired Dialpad PWA links, from the device store's cached counts (refreshed by the janitor; no I/O in the callback). Only reported while a server has the PWA enabled | P3 |
| `mrtcsip.widget.device_events` | Counter | `{event}` | `event` (issued, enrolled, rotated, grace_used, reuse_revoked, revoked, expired, orphan_purged, notice_redacted) | Dialpad PWA device lifecycle (PWA-SPEC §10) | P3 |
| `http.client.request.duration` | Histogram | `s` | `http.request.method`, `http.response.status_code`, `server.address`, `url.template`, `error.type` | `obs.WrapHTTPTransport` (Matrix and lk-jwt-service), semconv buckets | T6 |
| Go runtime metrics | contrib `runtime` | | | standalone host only, when metrics are on | T6 (optional, see §9) |

The `mrtcsip.widget.*` instruments live in `pkg/widget` (host-class), created from `obs.M().Meter` and
rebuilt when `obs.Use` swaps the registry; they carry no MXIDs, numbers or room ids.

`server.address` on `http.client.request.duration` is the homeserver or lk-jwt-service host: bounded
by deployment, not by user, so allowed. `url.template` is bounded by the templater (ids become `*`).

---

## 5. Span catalogue

### 5.1 Conventions

- **Root** spans: entry points only (inbound SIP request, register loop attempt, Matrix event routed
  to a login, bot command, login submit, framework `Connect`/`Disconnect`/`ResolveIdentifier`, timers,
  startup). Created with `obs.Start` (root if ctx has no span, child otherwise, so a host that is
  already tracing gets one trace).
- **Child-only** spans: everything else, with `obs.StartChild`.
- **Linked roots**: work that outlives or is triggered by another trace (`obs.StartLinkedRoot`).
  Goroutines **never receive a live span**: they get `obs.CarrySpan(context.Background(), ctx)` and
  start their own span (child of the carried span context only if the parent is guaranteed to wait
  for it; otherwise a linked root).
- Span names are static, lower-case, dot-separated, never contain ids or numbers (Datadog groups by
  span name/resource; ids belong in attributes).
- Kinds: SERVER for handling an inbound SIP request, CLIENT for outbound SIP/HTTP/LiveKit, CONSUMER for
  ingesting Matrix events and bot commands, INTERNAL otherwise.
- `obs.Fail(span, err)` for every error (scrubbed message, `error.type`).
- Every span and log line of a call carries `call.id` (bridge-generated ULID, created at INVITE receipt
  or dial start). It is the cross-trace correlation key (the call spans several traces, §5.10).

### 5.2 Attribute keys

| Key | Example / values | Notes |
|---|---|---|
| `call.id` | `01JA...` | bridge-generated |
| `call.direction` | inbound, outbound | |
| `call.outcome` | answered, missed, declined, cancelled, timeout, busy, failed, rejected | final |
| `call.trigger` | invite, command, owner_membership | |
| `call.hangup.by` | remote, owner, bridge | teardown |
| `call.end_reason` | bye, cancel, decline, ring_timeout, rtc_lost, rtp_timeout, rejected, remote_reject, bridge_stop, error | `rejected`: setup refused by the bridge (busy, unsupported codec, encrypted room, withheld caller, disallowed destination); `remote_reject`: outbound INVITE answered with a final non-2xx (PHASE3-SPEC §6.6); `bridge_stop`: logout or shutdown (PHASE2-SPEC §8.3) |
| `call.duration_s` | 83.4 | double, talk time |
| `call.remote.e164` | `+15551234567` | allowed on spans/logs only |
| `call.remote.withheld` | true | anonymous / unparseable caller |
| `call.local.e164` | account display number | |
| `sip.method`, `sip.call_id`, `sip.cseq` | INVITE, `a84b4c76e66710@...`, 1 | Call-ID is an id, allowed |
| `sip.response.status_code`, `sip.response.reason` | 486, Busy Here | |
| `sip.register.kind`, `sip.register.attempt`, `sip.register.next_retry_ms` | full / 3 / 4000 | |
| `sip.registrar.address` | `203.0.113.7:5008` | pinned IP:port |
| `sip.contact` | `198.51.100.2:40123` | NAT-rewritten host:port only |
| `sip.user.masked` | `12***78` | `sipua.MaskUsername`; the raw SIP username and the raw `LoginRef` are never recorded |
| `sip.auth.challenged` | true | digest round happened; never the challenge or response |
| `network.transport`, `network.peer.address`, `network.peer.port`, `server.address`, `server.port` | udp | OTel semconv |
| `matrix.room_id`, `matrix.event_id`, `matrix.event.type`, `matrix.sender`, `matrix.ghost_id`, `matrix.owner_id` | | MXIDs allowed |
| `matrixrtc.kind`, `matrixrtc.membership.device_id`, `matrixrtc.membership.expires_ms` | call_member, notification, decline, rtc_member, rtc_slot | |
| `matrixrtc.membership.format` | legacy, sticky | `matrixrtc.ingest` (member kinds), `matrixrtc.member.publish`, `matrixrtc.member.leave`, `call.membership.refresh` |
| `matrixrtc.slot_id` | `m.call#room`, `m.call#ROOM`, `other` | spans only, never metrics. Closed set: the two room spellings, any other slot id (sender-controlled on ingest) is recorded as `other` (`matrixrtc.SlotIDLabel`); `matrixrtc.ingest` of slot and member kinds, slot/member/ring spans |
| `matrixrtc.slot.action` | noop, opened, refused_encrypted, failed | `matrixrtc.slot.open` |
| `matrixrtc.member.event_type` | `m.rtc.member`, `org.matrix.msc4143.rtc.member` | the sticky event type sent (the dialect) |
| `matrixrtc.delayed_leave` | true | bool: a delayed leave is scheduled (`matrixrtc.member.publish`) |
| `matrixrtc.member.leave_via` | delayed_send, explicit | `matrixrtc.member.leave` |
| `matrixrtc.plan.format` | legacy, sticky, dual | `matrixrtc.plan.choose`, setup span |
| `matrixrtc.plan.reason` | config_legacy, no_rtc_intent, no_sticky_support, sticky_unsupported_recently, owner_legacy, owner_sticky_mirror, bridge_dialect, bridge_stable, config_sticky, config_dual, msc4195_failed_recently | the row of MATRIXRTC2-SPEC §2 that decided. `bridge_stable` (auto, bridge-initiated, no dialect yet) means: legacy ring shape, `sfu_get`, sticky membership with a `livekit_service_url` transport. `msc4195_failed_recently`: `auto` would use MSC4195 but a `get_token` attempt of this login failed before the LiveKit connect within 10 min, so the call goes straight to `sfu_get` (new value) |
| `matrixrtc.plan.downgrade` | slot_error, sticky_unsupported, sticky_error | setup span: a dual plan fell back to legacy only; an owner-mirror plan never fails the call: a slot problem is `slot_error` (sticky still published), a sticky failure is `sticky_unsupported` / `sticky_error` and the call goes on with the legacy membership |
| `matrixrtc.sticky.supported`, `matrixrtc.delayed.supported` | true | bool: homeserver capabilities (`/versions`, cached 10 min); sticky is false while a refused send pauses sticky for the login (10 min) |
| `matrixrtc.transports.status` | not_needed, ok, no_livekit_url, error | `/rtc/transports` lookup of the plan |
| `rtc.token.endpoint` | sfu_get, get_token_cs | `rtc.jwt_fetch` and `matrixrtc.plan.choose` |
| `rtc.token.fallback` | true | bool: MSC4195 `get_token` failed (stage `token_error`, or a `timeout` before the LiveKit connect) and `sfu_get` was used (setup span, re-publish spans). The switch does not consume a join attempt, and the login skips MSC4195 for 10 min afterwards (`matrixrtc.plan.reason=msc4195_failed_recently`). The `get_token` call itself is bounded to 4 s (stable plus unstable path) |
| `livekit.room` | hashed room name returned by lk-jwt-service | not secret |
| `livekit.participant.identity`, `livekit.server.address` | | |
| `media.codec`, `media.ptime_ms`, `media.telephone_event` | PCMU, 20, true | from SDP, not the SDP |
| `rtp.packets.rx`, `rtp.packets.tx`, `rtp.lost`, `rtp.late`, `rtp.jitter.max_ms`, `rtp.jitter.mean_ms` | | session summary |
| `cmd.name` | call | never arguments |
| `sip.early_media` | true | `call.ringback`: 183 early media was relayed (PHASE3-SPEC) |
| `dtmf.digit_count` | 4 | `cmd.dtmf`: number of digits, never the digits |
| `dtmf.method` | rfc4733 | metric attribute of `mrtcsip.dtmf.sent` |
| `bridge.version` and config summary booleans | | `bridge.start` |

### 5.3 (a) SIP registration lifecycle

REGISTER refreshes run every 25 s (ADR-009), about 3,500 per login per day. Successful refreshes
produce **no span** (only `mrtcsip.sip.register.attempts`/`.duration`); spans are kept for events
worth reading.

| Span | Kind | Root? | When | Attributes / notes |
|---|---|---|---|---|
| `sip.ua.start` | INTERNAL | child-only | `UA.Start` (socket bind) | `network.transport`, bind port; bind error fails it |
| `sip.register` | CLIENT | **root**, except the first attempt after `Start` (child of the `Start` caller's span context, see below) | every **full** REGISTER, and every **failed** refresh | `sip.register.kind`, `sip.register.attempt`, `sip.registrar.address`, `sip.auth.challenged`, `sip.response.status_code`, `outcome`, `sip.contact` (on success), `sip.register.next_retry_ms`, `sip.user.masked`. Event `sip.registrar.switch` when the pinner advances (old and new address). |
| `sip.register.resolve` | CLIENT | child-only | registrar DNS lookup | `server.address`, count of IPs |
| `sip.registration.state` | span **event** | n/a | on every `OnStateChange` transition, added to the current `sip.register` span | `state`, `previous_state` |
| `sip.unregister` | CLIENT | child-only (under `bridge.disconnect` / `bridge.logout`); root if called without a span | `UA.Stop` when last state was Registered | status, `outcome` |

First-attempt parenting: `UA.Start(ctx)` captures `trace.SpanContextFromContext(ctx)`. The register
goroutine creates the first `sip.register` as a child of that span context (the login/connect caller
waits for the first result, and `attemptTimeout` must stay below the 30 s login wait, which it does).
All later attempts are roots, so a long-lived registration never hangs off a login trace.

From `Connect`, the first `sip.register` may outlive `bridge.connect` (legal for a child span; accepted).

Deferred: the `sip.auth.challenged` attribute is not emitted yet (diago exposes no hook for the digest round).

### 5.4 (e) Login, connect, disconnect

| Span | Kind | Root? | Children |
|---|---|---|---|
| `bridge.login.submit` | CONSUMER | root | `sip.ua.start`, first `sip.register` (+ `sip.register.resolve`), `bridge.login.save` (DB, `NewLogin`), Matrix HTTP spans. Attributes: `sip.user.masked`, `server.address` (registrar host), `network.transport`, `call.local.e164`, `outcome`. Never the input map. |
| `bridge.connect` | INTERNAL | root | `sip.ua.start`, first `sip.register` |
| `bridge.disconnect` / `bridge.logout` | INTERNAL | root | `sip.unregister` |
| `bridge.resolve_identifier` | INTERNAL | root (framework-driven, e.g. `start-chat`) | `call.local.e164`, `call.remote.e164`, `create_chat` bool |
| `bridge.start` | INTERNAL | root, zero-length, emitted in `PostStart` | `bridge.version`, `stun.enabled`, `external_ip.set`, `ring_timeout_s`, `rtp.port_range_size`, `sip.port_range_size`, `encryption.forced_off` (bool). Smoke test for the pipeline. |

### 5.5 (d) Matrix event ingest (call.member / notification / decline hook)

Deferred: `matrixrtc.membership.device_id` is not emitted in Phase 1 (the device id is not parsed yet).

The hook runs synchronously on the appservice transaction path (PHASE1-SPEC §4.4/§4.5).

| Span | Kind | Root? | Notes |
|---|---|---|---|
| `matrixrtc.ingest` | CONSUMER | root (child if the host ctx is already traced) | Started **after** the resolver accepts the event, with `trace.WithTimestamp(t0)` where t0 is handler entry, so dropped events (ghost echoes, bot, unknown rooms) create no span but are counted (`routed=false`). Attributes: `matrix.room_id`, `matrix.event_id`, `matrix.event.type`, `matrixrtc.kind`, `matrix.sender`, `matrixrtc.membership.device_id`, `call.local.e164` (the login's number), and, once the sink routes it, `call.id` and `matrixrtc.routed_to` (call, none). Never the content. Ends before the handler returns. |
| `bridge.portal_lookup` | INTERNAL | child-only | not created (the resolver runs before the span); its duration is recorded as attribute `matrixrtc.resolve_ms` |

The sink hands the event to the call goroutine with `obs.CarrySpan`. The call state machine does not
continue the ingest trace; instead the span it creates in reaction (`call.ring` end, `call.answer`,
`call.teardown`) gets a **link** to the ingest span context. Phase 1's `core.LogSink` switches to
`obs.Logger(ctx, &base)` with a clean injected base logger (T6).

### 5.6 (b) Inbound call end to end

Setup trace (root `call.inbound`, bounded by ring timeout plus join timeouts, so under ~2 minutes):

```
call.inbound (SERVER, root)                       INVITE received .. ACK received or final failure sent
├─ event sip.response.sent {status 100}
├─ call.resolve_room (INTERNAL)                   CallHost.ResolveCallRoom
│   └─ PUT/POST /_matrix/... (CLIENT)             host Matrix transport, child-only
├─ event sip.response.sent {status 180}
├─ matrixrtc.membership.publish (CLIENT)          ghost call.member state event
├─ matrixrtc.ring.send (CLIENT)                   MSC4075 notification, lifetime = ring timeout
├─ rtc.join (INTERNAL)                            pre-join while ringing
│   ├─ rtc.openid_token (CLIENT)                  GhostIntent.RequestOpenIDToken (token never recorded)
│   ├─ rtc.jwt_fetch (CLIENT)                     lk-jwt-service via obs.WrapHTTPTransport (JWT never recorded)
│   ├─ rtc.livekit.connect (CLIENT)               signalling + ICE; livekit.room, livekit.server.address
│   └─ rtc.track.publish (INTERNAL)
├─ call.ring (INTERNAL)                           ring sent .. owner join / decline / CANCEL / timeout
│                                                 links: matrixrtc.ingest that ended it
├─ call.answer (INTERNAL)                         200 OK with SDP; media.codec, media.ptime_ms
│   ├─ media.start (INTERNAL)                     RTP socket + transcoder ready
│   └─ event sip.ack.received
│  or, on failure:
├─ event sip.response.sent {status 603|480|487}
├─ matrixrtc.membership.clear (CLIENT)
├─ matrixrtc.ring.redact (CLIENT)
├─ rtc.leave (CLIENT)
└─ call.notice.send (INTERNAL)                    CallHost.SendCallNotice; "Missed call"
```

Attributes on `call.inbound`: `call.id`, `call.direction=inbound`, `call.trigger=invite`,
`sip.call_id`, `call.remote.e164` / `call.remote.withheld`, `call.local.e164`, `matrix.room_id`,
`matrix.ghost_id`, `network.peer.address` (provider), final `sip.response.status_code`,
`call.outcome` (for failures; `answered` when ending at ACK). A CANCEL received during ringing is a
span event `sip.cancel.received` plus the failure branch.

The INVITE handler in diago runs on a sipgo transaction goroutine: it sends `100 Trying`, starts
`call.inbound`, and hands the call to the call goroutine with the **live span ownership transferred**
(the handler does not touch the span afterwards). That is the one allowed transfer of a live span: the
creator never uses it again and the receiver ends it. Equivalent and preferred (PHASE2-SPEC D10): the
handler only records the receipt and `100 Trying` times, and the call goroutine starts `call.inbound`
itself with `trace.WithTimestamp(<INVITE receipt>)` and adds the `100` event with its real timestamp, so
no live span crosses goroutines. The resulting trace is identical.

Mid-call (each a short **linked root** to the setup span, all with `call.id`):
`call.reinvite` (SERVER), `call.membership.refresh` (CLIENT, Phase 4), `call.owner.left`,
`call.media.degraded` (emitted once when loss > 5 % or no RTP for 5 s; attributes summarise, no packet
data).

Teardown and summary: see §5.10.

### 5.7 (c) Outbound call

```
cmd.call (CONSUMER, root) or matrixrtc.ingest (owner_membership trigger)
   └── (goroutine) call.outbound (CLIENT, linked root -> trigger span)   dial start .. ACK sent or failure
       ├─ call.resolve_room
       ├─ matrixrtc.membership.publish        ghost call.member, published before dialling
       ├─ matrixrtc.ring.send, call.ring      command path only: callback ring of the owner's devices
       ├─ rtc.join (… same children as inbound)
       ├─ sip.invite (CLIENT)                 sip.call_id; events sip.response.received {100|180|183|final}
       │   └─ event sip.auth.challenge        407/401 then re-send; never the challenge contents
       ├─ call.ringback (INTERNAL)            first 180/183 .. 200 or failure; sip.early_media bool
       │   └─ media.start                     early-media bridge (183 with SDP)
       ├─ event sip.ack.sent
       ├─ media.start                         when no early-media bridge was started
       └─ on failure: matrixrtc.decline.send (decline of the owner's notification), matrixrtc.membership.clear,
          rtc.leave, call.notice.send ("Call failed: 486 Busy Here")
```

`call.outbound` is a linked root because the command handler (or the appservice event path) returns long
before the call is set up. Attributes as inbound, with `call.trigger` (command, owner_membership). Details:
PHASE3-SPEC §10.

#### MatrixRTC 2.0 spans (MATRIXRTC2-SPEC §3 WP5)

All are child-only: they appear under the setup or teardown span of a call and never start a root. The
plan is decided once per call, in `publishMembership` (before the ring and the join), so these spans sit
beside `matrixrtc.membership.publish`.

| Span | Kind | Notes |
|---|---|---|
| `matrixrtc.plan.choose` | INTERNAL | Not created when the ghost has no `core.GhostRTCIntent` (legacy, nothing to observe). Attributes: `call.id`, `matrixrtc.plan.format`, `matrixrtc.plan.reason`, `matrixrtc.sticky.supported`, `matrixrtc.delayed.supported`, `matrixrtc.transports.status`, `rtc.token.endpoint`; for sticky plans also `matrixrtc.slot_id`, `matrixrtc.member.event_type`. `matrixrtc.plan.format` and `.reason` are repeated on the setup span. Its children are the cached `/versions` and `/rtc/transports` requests (CLIENT, transport wrapping). |
| `matrixrtc.slot.open` | CLIENT | One per (slot event type, slot id) pair the plan needs (bridge-initiated: both spellings under both `m.rtc.slot` and `org.matrix.msc4143.rtc.slot` = 4; owner-initiated mirror: the owner's type and spelling = 1). Every pair is read first; nothing is opened if any existing slot declares encryption. Attributes: `call.id`, `matrixrtc.slot_id`, `matrixrtc.slot.action` (noop, opened, refused_encrypted, failed). A slot with `encryption` is never rewritten and refuses the call (Warn log, same path as an encrypted room). Slots are never closed. |
| `matrixrtc.member.publish` | CLIENT | Sticky join: delayed leave scheduled first (when `FeatureDelayed`), then the sticky send. Attributes: `call.id`, `matrix.room_id`, `matrixrtc.membership.format=sticky`, `matrixrtc.slot_id`, `matrixrtc.member.event_type`, `matrixrtc.delayed_leave`, `matrix.event_id`. Also emitted when a token fallback re-publishes the join (with `rtc.token.fallback=true`). |
| `matrixrtc.member.leave` | CLIENT | Teardown: keeper stopped, then the delayed leave is fired (`delayed_send`) or the leave is sent explicitly. Attributes: `call.id`, `matrixrtc.membership.format=sticky`, `matrixrtc.slot_id`, `matrixrtc.member.event_type`, `matrixrtc.member.leave_via`. |
| `matrixrtc.ring.withdraw` | CLIENT | Ends a sticky ring (the sticky key with no other fields). `call.id`, `matrixrtc.slot_id`. `matrixrtc.ring.redact` still runs. |

Other changes: `matrixrtc.ring.send` carries `matrixrtc.slot_id` for a sticky ring. `rtc.jwt_fetch` gets
`rtc.token.endpoint` (`sfu_get` or `get_token_cs`); with `get_token_cs` there is no `rtc.openid_token`
span. A token fallback sets `rtc.token.fallback=true` on the setup span. A dual plan that dropped to
legacy sets `matrixrtc.plan.downgrade` on the setup span. `call.membership.refresh` (linked root) also
covers the 40 min sticky re-send, with `matrixrtc.membership.format=sticky`. `matrixrtc.ingest` adds
`matrixrtc.membership.format` (member kinds) and `matrixrtc.slot_id` (member and slot kinds).

The delayed-leave keeper (one goroutine per sticky call) starts no span. It only counts
`mrtcsip.matrixrtc.delayed_leave.restarts` and logs state changes; its log lines carry the trace fields of
the call's setup span (`obs.LoggerSC` with the stored span context). `matrixrtc.member.leave` is not
created when the ghost has no `GhostRTCIntent`. The `member.id` is a random pseudonym,
not a credential: debug logs may carry it, metrics never do.

### 5.8 (f) Bot commands

`cmd.<name>` (CONSUMER, root) via a `withSpan` wrapper identical to jabber's (`cmd.` + name with `-`
→ `_`; attribute `cmd.name` only; **arguments are never recorded**, which also keeps DTMF digits and
numbers typed into commands out of traces; `ce.Ctx` and `ce.Log` swapped for the handler's duration).
Background work started by a command is a linked root (`cmd.<name>.background` or the call span).
Framework built-ins (`login`, `logout`, `start-chat`) are covered by `bridge.login.submit`,
`bridge.logout`, `bridge.resolve_identifier`. Phase 1 has no custom commands; the wrapper lands in T6
with a test, ready for Phase 3 (`call`, `dtmf`, `hangup`).

### 5.9 (g) Outbound Matrix HTTP and lk-jwt-service HTTP

- `WrapMatrixTransport` on `br.Matrix.AS.HTTPClient` (standalone host). Child-only: requests without a
  traced ctx get no span. Span name `METHOD /_matrix/client/v3/rooms/*/state/*/*` (jabber's
  templater), CLIENT, attributes `http.request.method`, `server.address`, `server.port`, `url.template`,
  `http.response.status_code`, `error.type`; status Error for 5xx and transport errors. W3C
  `traceparent` is injected (harmless if the homeserver ignores it). The URL, query and headers
  (Authorization with the AS token) are never recorded.
- `pkg/rtc` builds its lk-jwt-service client with `obs.WrapHTTPTransport(base, rtcTemplater)`. Same
  attributes. Request and response bodies (OpenID token in, LiveKit JWT out) are never recorded.
- LiveKit signalling is a WebSocket inside `server-sdk-go`; it is covered by `rtc.livekit.connect`
  and `rtc.leave` spans around the SDK calls, not by transport wrapping.

### 5.10 Long-lived calls: no span stays open through the media phase

Many backends assemble traces in time windows and show incomplete traces while a parent is open; a
process crash loses an open span entirely. Rule: **no span with children stays open past answer**.
Hard cap: any span open longer than 10 minutes is a bug (asserted in state-machine tests with a fake
clock where possible, and reviewed).

A call therefore spans several short traces tied by `call.id` and span links:

1. Setup trace: `call.inbound` / `call.outbound` (§5.6/§5.7), ends at ACK.
2. Mid-call linked roots (§5.6).
3. `call.teardown` (INTERNAL, linked root to the setup span): `call.hangup.by`, `call.end_reason`.
   Children: `sip.bye` (CLIENT when we send BYE; when the remote sends it, an event
   `sip.bye.received` on `call.teardown`), `media.stop`, `rtc.leave`, `matrixrtc.membership.clear`,
   `call.notice.send`.
4. `call.session` (INTERNAL, linked root, links to setup and teardown): created **post hoc** at the end
   of teardown with `trace.WithTimestamp(callStart)` and ended immediately, like jabber's
   `xmpp.session`. No children, so its length cannot break trace assembly. Attributes: `call.id`,
   direction, outcome, `call.duration_s`, `media.codec`, `rtp.*` summary, `call.end_reason`. Up to 32
   buffered past events (re-INVITE, degradation, membership refresh) are added with their original
   timestamps.

If the bridge stops mid-call, Shutdown emits `call.session` with `call.end_reason=bridge_stop` for
every active call before flushing.

### 5.11 Embedded host (Phase J)

Host spans of the jmpchat-matrix-bridge embedded host (sipcalls build, PHASEJ-SPEC §8), created with
`obs`/`hostkit` helpers. Core spans (`sip.*`, `call.*`, `rtc.*`, `matrixrtc.*`, `media.*`) are unchanged.

| Span | Kind | Class | Notes |
|---|---|---|---|
| `bridge.sip_connect` | INTERNAL | root | SIP account start for a jabber login; the single retry after 30 s is a linked root with `retry=true`. Attributes: `sip.user.masked`, `server.address`, `network.transport`, `xmpp.login.jid`. |
| `bridge.sip_disconnect` | INTERNAL | root | `EndLogin` then account stop when the owning client disconnects. |
| `bridge.calls.pl_sweep` | INTERNAL | linked root | Call power-level sweep of the login's Cheogram 1:1 portals; `portals.checked`, `portals.updated`, `portals.failed`. |
| `bridge.calls.open_chat` | INTERNAL | linked root | The widget's Message: room creation detached from the request (linked to `widget.chat`); `xmpp.login.jid`, `call.remote.e164`, `created`. The MAM backfill that follows is its own `mam.backfill_on_view` linked root (`mam.trigger=widget_chat`). |
| `bridge.calls.inbound_filtered` | SERVER | root | Roster-only rejection of an inbound call (480): `xmpp.login.jid`, `call.remote.e164`, `outcome=rejected`, `reason=not_in_roster`. No metric. |
| `cmd.roster_only_calls`, `cmd.save_contact` | CONSUMER | root | `cmd.roster_only_calls` via `hostkit.WithSpan` (sipcalls build); `cmd.save_contact` via jabber's own `withSpan` (all builds, same naming and `cmd.name` attribute). Arguments (the contact name) never recorded. |
| `cmd.sip_login` | CONSUMER | root | `hostkit.WithSpan`; arguments never recorded. |
| `cmd.sip_login.submit` | CONSUMER | root | The prompt submit arrives as a separate message. `outcome`, `sip.user.masked`, `server.address`, `network.transport`, `call.local.e164`. |
| `cmd.sip_logout`, `cmd.sip_status` | CONSUMER | root | `hostkit.WithSpan`. |
| `cmd.call`, `cmd.hangup`, `cmd.dtmf` | CONSUMER | root | Shared with the standalone host (`hostkit.RegisterCallCommands`); DTMF digits never recorded. |

### 5.12 Dialpad widget (WIDGET-SPEC §10)

Spans of `pkg/widget` (host-class, both hosts). `GET state`, `POST normalize`, `GET contacts` and static files get **no
spans** (polling and typed-ahead search would flood traces; they appear only in `mrtcsip.widget.requests`).
Contact names and search queries are never put on spans, logs or metrics. Action spans start
only after the session is authenticated, so unauthenticated traffic cannot create traces. The trace
context of incoming HTTP requests is never extracted (every widget span is a fresh root). Root spans
annotate the ctx logger; handlers log through `obs.Logger(ctx, &s.log)`.

| Span | Kind | Class | Attributes |
|---|---|---|---|
| `widget.session` | SERVER | root | `matrix.sender` (on success), `matrix.room_id`, `widget.room.kind` (portal, management), `outcome` (ok, bad_key, openid_invalid, openid_error, no_user, forbidden, rate_limited, error). Started after the per-IP limits and body decoding pass. |
| `widget.call` | SERVER | root | `matrix.sender`, `matrix.room_id`, `call.remote.e164`, `widget.call.form` (room, number), `widget.contact.saved` (bool, only when a name was sent), `outcome` (an API error code or ok). `call.outbound` links to it (PlaceCall reads the ctx span, as for `cmd.call`). |
| `widget.dtmf` | SERVER | root | `matrix.sender`, `matrix.room_id`, `dtmf.digit_count`, `outcome`. **Never the digits.** |
| `widget.hangup` | SERVER | root | `matrix.sender`, `matrix.room_id`, `outcome` |
| `widget.prefs` | SERVER | root | `matrix.sender`, `matrix.room_id`, `widget.prefs.roster_only` (the new value), `outcome`. Optional host feature. |
| `widget.chat` | SERVER | root | `matrix.sender`, `matrix.room_id`, `call.remote.e164`, `widget.chat.created`, `widget.chat.pending`, `widget.contact.saved` (only when a name was sent), `outcome`. Optional host feature. |
| `widget.contact` | SERVER | root | `matrix.sender`, `matrix.room_id`, `call.remote.e164`, `widget.contact.form` (room, number), `widget.contact.changed`, `outcome`. Optional host feature. |
| `widget.openid.verify` | CLIENT | child-only (under `widget.session`) | `server.address` (the token's homeserver name), `outcome` (ok, openid_invalid, openid_error). The HTTP client span below it uses the fixed `url.template` `/_matrix/federation/v1/openid/userinfo`, so the token (a query parameter) never reaches a span. |
| `bridge.widget.sweep` | INTERNAL | root (startup, disable) / linked root (login, linked to the caller's span) | `trigger` (startup, login, periodic, disable), `rooms.checked`, `rooms.installed`, `rooms.updated`, `rooms.failed`, `rooms.removed` (disable only). Periodic passes start a span **only** when they have rooms not yet done. |

Dialpad PWA (PWA-SPEC §10). Device spans start after the per-IP limits and body decoding pass; malformed
codes and tokens are refused before the device store is queried.

| Span | Kind | Class | Attributes |
|---|---|---|---|
| `widget.device.enroll` | SERVER | root | `matrix.sender` (once known), `widget.device.id`, `widget.device.confirm` (bool), `outcome` (ok, peek, invalid, used, expired, forbidden, rate_limited, error) |
| `widget.device.session` | SERVER | root | `matrix.sender`, `widget.device.id`, `widget.device.token_state` (current, unconfirmed, grace), `outcome` (ok, invalid, expired, revoked, reuse, forbidden, rate_limited, error) |
| `widget.device.confirm` | SERVER | root | `matrix.sender`, `widget.device.id`, `outcome` (ok, repeat, invalid, expired, revoked, reuse, rate_limited, error) |
| `widget.device.forget` | SERVER | root | `matrix.sender`, `widget.device.id`, `outcome` (ok, error) |
| `bridge.widget.device_janitor` | INTERNAL | root, recorded **only** when it redacted or purged something | `devices.purged`, `notices.redacted`, `notices.failed` |
| `cmd.dialpad_link`, `cmd.dialpad_links`, `cmd.dialpad_revoke` | CONSUMER | root (`WithSpan`) | `cmd.name`; `Issue`/`Revoke` add span events `dialpad.link.issue` / `dialpad.link.revoke` with `widget.device.id` and `outcome` (ok, not_private, limit, not_found, error), never the name or code |

`widget.call`, `widget.dtmf`, `widget.hangup`, `widget.prefs`, `widget.chat` and `widget.contact` carry `widget.client` (widget, pwa); for PWA sessions
they have `widget.device.id` instead of `matrix.room_id`. `GET state` from the PWA stays span-free.

Privacy (in addition to §6): OpenID tokens, session tokens, the per-room widget key `k`, the widget
secret, PWA enrollment codes and device tokens, device names, the User-Agent (only the closed client
enum), request bodies and DTMF digits are never logged or traced. Link IDs are allowed on spans and
logs, never on metrics. Transport errors of the userinfo
request (which embed the URL with the token) are mapped to fixed strings before they reach a log or
span.

---

## 6. Privacy rules

### 6.1 Never recorded (spans, span events, errors, logs, metrics)

- Audio, RTP payloads, SDP bodies (only parsed codec/ptime/telephone-event facts), raw SIP messages.
- DTMF digits, in any form (RFC 4733 events, bot command arguments, logs). Only counts.
- SIP passwords, `Authorization` / `Proxy-Authorization` headers, digest challenge/response values,
  the raw SIP username and raw `LoginRef` (use `sip.user.masked`).
- Matrix OpenID tokens, LiveKit JWTs, AS/HS tokens, double-puppet tokens, any `Bearer` value.
- OTLP header values (L4), full URLs (only `scheme://host` and templated paths).
- Matrix event content (call.member bodies may hold LiveKit service URLs and device keys).
- Contact names (and previous names) typed in the widget or given to `save-contact`: not on spans, logs
  or metrics. The number is allowed on spans and logs.

### 6.2 Allowed

Phone numbers (E.164) and MXIDs on **spans and logs** (as JIDs are in jabber), SIP Call-IDs, Matrix
room/event ids, LiveKit room names (hashed by lk-jwt-service), IP:port of our Contact and the provider,
codec, timings, status codes, error messages after scrubbing. **Never on metrics** (§4.1): caller
numbers, MXIDs and every other identity are span/log-only. The README states that the trace backend
sees call metadata (who called whom, when, for how long) and must be access-controlled.

### 6.3 Scrubbing (`obs.Scrub`, used by `obs.Fail`, the OTel error handler and the logr sink)

1. `(?:https?|mxc|wss?)://\S+` → `scheme://host` (jabber's rule plus `ws`/`wss` for LiveKit).
2. `sips?:([^:@\s]+):[^@\s]+@` → `sip:$1:***@` (deprecated password-in-URI form).
3. `(?i)bearer\s+\S+` → `Bearer ***`.
4. JWT-shaped `eyJ[\w-]+\.[\w-]+\.[\w-]*` → `<jwt>`.
5. `(?i)(access_token|password|secret|token)=[^&\s]+` → `$1=***`.

### 6.4 SIP header rules (if raw SIP ever appears in a span or log)

Default: raw SIP is never attached (diago logs stay capped at Warn, `SIPDebug` is forbidden, per
PHASE1-SPEC). If a debug feature ever attaches headers, it goes through `sipua.ScrubHeaders`, an
**allowlist**: `Via` (branch kept), `From`/`To` (numbers allowed, tags kept), `Call-ID`, `CSeq`,
`Contact` (host:port only), `Max-Forwards`, `Expires`, `Reason`, `User-Agent`, `Server`,
`Content-Type`, `Content-Length`, `Allow`, `Supported`, `Session-Expires`. `P-Asserted-Identity` is kept
(it carries the caller number, which is allowed). Everything else is dropped, in particular
`Authorization`, `Proxy-Authorization`, `WWW-Authenticate`, `Proxy-Authenticate`, any header whose name
contains `auth`, `token`, `key` or `secret`, and any `X-` provider header. Bodies are always dropped.

Tests seed known secrets (SIP password, a JWT, an OpenID token, DTMF digits `1234#`, an AS token) through
every instrumented flow and assert none appears in any recorded span attribute, event, status
description, or captured log output (§8.4).

---

## 7. Log correlation

Identical to jabber (same field names, so one Datadog/collector pipeline config serves both bridges):

- Lines logged inside a **sampled, valid** span carry, each exactly once: `trace_id` (32 hex),
  `span_id` (16 hex), `dd.trace_id` (decimal, low 64 bits), `dd.span_id` (decimal).
- Root spans (`obs.Start` starting a new trace, `obs.StartLinkedRoot`) annotate the ctx logger
  (`zerolog.Ctx`), so bridgev2 framework lines under the span correlate.
- Our own lines use `c.logFor(ctx)` = `obs.Logger(ctx, &c.log)` where `c.log` is the component's clean
  logger. Never `c.log` directly under a span; never pass a logger derived from `zerolog.Ctx(ctx)` as
  the base (it may already hold the keys).
- Goroutines without a ctx but with a stored span context use `obs.LoggerSC`.
- Call lines additionally carry `call_id` (added to the call's clean logger at call creation), so a whole
  call can be found in logs even across its several traces.
- When tracing is off, `obs.Logger` returns the base pointer unchanged (no allocation, tested).
- Correlation needs JSON logs on stdout (`logging.writers: [{type: stdout, format: json}]`); the README
  repeats jabber's Datadog notes (both pairs recognised by default; fallback pipeline settings;
  `com.datadoghq.tags.service` label).

---

## 8. Roadmap placement

### 8.1 T6: telemetry foundation (after Phase 1 T4, before Phase 2)

Dependency order: T6a → (T6b, T6c in parallel) → T6d → T6e → review.

| Task | Tier | Files | Content |
|---|---|---|---|
| **T6a** `pkg/obs` | Sonnet | `pkg/obs/*.go` + tests | Port jabber `span.go` and the generic part of `http.go`; `Providers`/`Use`/`Enabled`/`MetricsEnabled`; instrument registry for the T6 instruments of §4.3 (Phase 2/3 instruments are added by those phases); `Scrub` (§6.3); attribute key constants. API-only imports. |
| **T6b** `pkg/telemetry` | Sonnet | `pkg/telemetry/*.go` + tests | Port jabber `telemetry.go`/`env.go`; add metrics (OTLP metric exporters grpc/http, periodic reader, temporality selector, `OTEL_METRICS_EXPORTER`), per-signal resolution, `obs.Use` call, combined bounded `Shutdown`; `WrapMatrixTransport` over `obs.WrapHTTPTransport`; optional runtime metrics. |
| **T6c** import check | Haiku | `internal/importcheck/*` | Skip `pkg/telemetry` when collecting core dirs; forbid `go.opentelemetry.io/otel/sdk`, `.../otel/exporters`, `go.opentelemetry.io/contrib`, `<module>/pkg/telemetry` in core non-test files; detection test cases for each. |
| **T6d** instrumentation | Sonnet | `cmd/mrtc-sip-bridge/main.go`, `pkg/connector/{start,login,client,rtchook}.go`, new `pkg/connector/telemetry.go`, `pkg/sipua/{loop,diago}.go`, `pkg/core/{ingest,hook}.go` | `telemetry.Init` in `PostInit` with build metadata; wrap AS HTTP client; `bridge.start` in `PostStart`; `SIPConnector.Stop` → `telemetry.Shutdown`; `withSpan` command wrapper (+ test); spans of §5.3, §5.4, §5.5; T6 metrics; `logFor` everywhere a span is active; `LogSink` uses a clean injected logger. No behavioural changes. |
| **T6e** docs and deps | Haiku/Sonnet | `README.md`, `CLAUDE.md` (Telemetry section, mirroring jabber's), `docs/CREDITS.md`, `docs/PLAN.md` (move Phase 5 telemetry items here, tick T6) | README "Tracing and metrics" section: env table, collector and Datadog examples (incl. temporality), what is traced, privacy, log correlation. |
| Review | Opus | diff | Against this spec: root/child classification, no blocking in I/O paths, privacy, import boundary. |

T6 acceptance criteria:

1. `go build/vet/test -tags nocrypto ./...` pass; tests run offline.
2. With no `OTEL_*` env: no OTel goroutines (test compares `runtime.NumGoroutine` before/after
   `Init`), helpers return identical ctx and logger pointers, `obs.Enabled()==false`, startup log has no
   telemetry line.
3. `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:1` (unreachable): bridge starts normally, one
   rate-limited export warning at most per 5 min, shutdown completes in under 3.5 s.
4. Against a local collector (manual): `bridge.start` appears; `!sip login` produces one
   `bridge.login.submit` trace containing `sip.ua.start` and `sip.register`; ten minutes registered
   produce **no** periodic `sip.register` spans but `mrtcsip.sip.register.attempts{kind=refresh,outcome=ok}`
   grows; an Element call in a portal produces `matrixrtc.ingest` roots; bridge logs under those spans
   carry the four correlation fields once each.
5. `TestCorePackagesImportBoundary` passes with the new forbidden prefixes, and the detection test
   proves each prefix is caught.
6. Privacy tests (§8.4) pass. `grep -rn "otel.Set" pkg/ --include=*.go | grep -v _test.go` matches only
   `pkg/telemetry`.
7. README, CLAUDE.md and CREDITS updated in the same change.

### 8.2 Per-phase instrumentation requirements (definition of done)

- **Phase 2 (inbound):** spans of §5.6 and §5.10 (`call.inbound`, `call.ring`, `call.answer`,
  `rtc.*`, `matrixrtc.membership.*`, `matrixrtc.ring.*`, `media.start/stop`, `call.teardown`,
  `call.session`, `call.notice.send`); call, rtc and media metrics of §4.3; `call_id` log field;
  lk-jwt-service client wrapped; hot-path rule (§4.2) with a benchmark showing zero allocations per
  RTP packet from telemetry; state-machine tests assert the trace shape with fake SIP/RTC/host, including
  CANCEL, decline, timeout and answer branches, and that the setup span ends at ACK.
- **Phase 3 (outbound):** §5.7 and §5.8 (`cmd.call`, `cmd.dtmf`, `cmd.hangup` through `withSpan`),
  `sip.invite` with auth-challenge event, `call.ringback`, `matrixrtc.decline.send`, `mrtcsip.dtmf.sent`; a test proves DTMF digits
  never reach spans or logs.
  `call.membership.refresh` (ghost membership extension for calls longer than 4 h) is pulled into
  Phase 2 (PHASE2-SPEC D15).
- **Phase 4 (robustness):** `call.reinvite`, `sip.registrar.switch` on
  network change; restart cleanup emits one `bridge.cleanup_stale_calls` root with counts; concurrent-call
  tests assert spans of different calls never share a trace.
- **Phase J (embedded):** jabber's `sipcalls` telemetry backend delegates to this repo's
  `telemetry.Init` with `Options.ServiceName = "jmpchat-matrix-bridge"` (which calls `obs.Use`, traces and
  metrics) and its span/log helpers delegate to `pkg/obs` (`obs.AnnotateLogger`; log-annotation
  convergence, §3.2); an embedded test asserts core spans carry jabber's resource `service.name` and nest
  under a jabber-created span passed in ctx; `docs/EMBEDDING.md` documents the new `mrtcsip.*` metrics and
  that setting `OTEL_EXPORTER_OTLP_ENDPOINT` now also enables metrics (`OTEL_METRICS_EXPORTER=none` to
  keep the old behaviour); jabber's public README is not changed.

Every phase's definition of done: catalogue entries implemented and listed in the README, metrics
listed, privacy tests extended to the new flows, Opus review checks root/child classification.

### 8.3 Test approach

- In-memory exporters, as in jabber: `tracetest.NewSpanRecorder()` behind an `sdktrace.TracerProvider`,
  and `sdkmetric.NewManualReader()` behind an `sdkmetric.MeterProvider`, installed with `obs.Use` and
  reset in `t.Cleanup`. A small `internal/obstest` helper package (test-only use) builds both and offers
  `SpanByName`, `Children(span)`, `Links(span)`, `Roots()`, `MetricPoints(name)`.
- Port jabber's tests: `TestResolveConfig` (extended with metrics rows and temporality),
  `TestDisabledIdentity`, `TestResourcePrecedence`, `TestStartChild`, `TestLinkedRootAndCarry`,
  `TestLogger*`, `TestStartAnnotatesRootsOnly`, `TestLinkedRootReannotates`,
  `TestDerivedLoggerNotReannotated`, `TestFail`, `TestURLTemplate`, `TestTransport`,
  `TestInitUnreachableShutdown`, `TestStripURLsMXC`, `TestOTelLogSinkDropsValues`.
- New: `TestScrub` table (§6.3); `TestRegisterLoopSpans` (fake `regTxn`: full register → span; refresh ok
  → no span, counter +1; refresh fail → span with status; first attempt parented to the Start ctx span,
  later attempts roots); `TestIngestSpan` (routed → one CONSUMER root with ids and no content; dropped →
  no span, counter `routed=false`); `TestLoginSpan`; `TestWithSpanCommand` (no args recorded);
  `TestEmbeddedParenting` (a "host" span in ctx → core spans are children, no new roots).

### 8.4 Privacy and cardinality tests

- `obstest.AssertNoSecrets(t, rec, logBuf, secrets...)`: walks every recorded span (name, attributes,
  events and their attributes, status description, links' attributes) and the captured JSON log output.
- `obstest.AssertMetricAttrsAllowed(t, reader, allowlist)`: every data point's attribute keys are in the
  §4.3 allowlist and string values match the closed enums; fails on any value that looks like an E.164
  number, MXID, room id or IP address.

---

## 9. Dependencies and licences

Licences checked 2026-10-16 with `gh api repos/<repo> --jq .license.spdx_id`. Pin OTel to **v1.47.0**,
the same version as jmpchat-matrix-bridge, so the embedded build resolves one version. All are compatible with
the project's planned licences and with being imported by jmpchat-matrix-bridge (ADR-006). T6e adds them to
`docs/CREDITS.md`.

| Module | Licence | Used by | Notes |
|---|---|---|---|
| `go.opentelemetry.io/otel`, `/attribute`, `/codes`, `/trace`, `/metric`, `/propagation` (v1.47.0) | Apache-2.0 (open-telemetry/opentelemetry-go) | `pkg/obs` (core) and hosts | API only in core |
| `go.opentelemetry.io/otel/sdk`, `/sdk/metric` (v1.47.0) | Apache-2.0 | `pkg/telemetry` (host), tests | |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace{,/otlptracegrpc,/otlptracehttp}` (v1.47.0) | Apache-2.0 | `pkg/telemetry` | as jabber |
| `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetric{grpc,http}` (v1.47.0) | Apache-2.0 | `pkg/telemetry` | new vs jabber |
| `go.opentelemetry.io/proto/otlp` | Apache-2.0 (open-telemetry/opentelemetry-proto-go) | indirect | |
| `go.opentelemetry.io/contrib/instrumentation/runtime` (release train matching v1.47.0) | Apache-2.0 (open-telemetry/opentelemetry-go-contrib) | standalone host only, optional | Go runtime metrics; skip if it drags in a mismatched otel version |
| `github.com/go-logr/logr` v1.4.x, `github.com/go-logr/stdr` | Apache-2.0 | `pkg/telemetry` (`otel.SetLogger`), indirect | |
| `google.golang.org/grpc` | Apache-2.0 | indirect (otlp grpc exporters) | |
| `google.golang.org/protobuf` | BSD-3-Clause | indirect | keep notice |
| `github.com/grpc-ecosystem/grpc-gateway/v2` | BSD-3-Clause | indirect | keep notice |
| `github.com/cenkalti/backoff` | MIT | indirect (exporter retry) | keep notice |

Apache-2.0 obligations: keep NOTICE/attribution in the third-party licences file planned in
`docs/CREDITS.md`. No OTel source is copied; jabber's `pkg/telemetry` is our own code (same author).

---

## 10. Open items for the coordinator

1. PHASE1-SPEC §4.4 has `LogSink` log the raw `login` (a `LoginRef` = `username@registrar`), while §7.13
   says the SIP username is only logged masked. T6 masks it (logs `sip.user.masked`); the Phase 1 review
   should decide whether to fix it already in Phase 1.
2. PLAN.md Phase 5 telemetry bullets should point here once T6 is accepted (T6e does it).
3. Resolved (Phase J): embedded metrics come from this repo's `pkg/telemetry` (jabber's sipcalls build
   initialises through it with `Options.ServiceName = "jmpchat-matrix-bridge"`), so no jabber MeterProvider is
   needed.
