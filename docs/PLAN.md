# Implementation plan

> Note: the host bridge mautrix-jabber was renamed to jmpchat-matrix-bridge in 2026-10 (v0.6.0); this document keeps the old name.

Status: **Phases 0-3 and J implemented and validated live; Phase 4 (robustness) next.** Read `DECISIONS.md` (why) and `RESEARCH.md` (facts and sources)
first. Update the checkboxes and the "Current state" section as work lands, so the next agent can pick up.

## Current state

- Phase 0 (spikes), Phase 1 (skeleton), Phase 2 (inbound calls), Phase 3 (outbound calls, DTMF) and
  Phase J (embedding in mautrix-jabber) are implemented and were validated against a live JMP account
  and a Synapse/LiveKit deployment: inbound and outbound calls, DTMF, decline and ring-out, and
  hang-up from either side all work. Phase J also works in shared Cheogram rooms.
- Key decisions from the spikes: production pre-joins LiveKit while ringing. Ring `lifetime` = ring
  timeout. Pin the registrar to one resolved IP. Unregister only our own Contact.
- Element X is affected by an upstream Element Call pickup bug for outbound calls; use `!sip call`
  there. Sable and other clients can press the call button in a number's room.
- The jabber-side embedding lives on a `local/sip-calls` branch of mautrix-jabber that is not published.
- v0.2.0: the Dialpad widget and PWA gain optional host features (`Deps.Features`, `pkg/hostkit/contacts.go`,
  WIDGET-SPEC §7.5): roster-only toggle, Message, Name / Save contact and a "Saved as" line. Only
  mautrix-jabber offers them; the standalone bridge is unchanged.
- Widget Contacts tab (unreleased, issue 6 phase 2): optional `hostkit.ContactLister` (`ListContacts`, plus
  `DisplayContactName`), `GET api/v1/contacts` (paged search, per-MXID 5/s, no span), `features.contacts`, and a
  Dial / Contacts tab bar in the room widget and the PWA. Call history is deferred. WIDGET-SPEC §7, §7.5, §9.
- MatrixRTC 2.0 (unreleased, `docs/MATRIXRTC2-SPEC.md`, ADR-014): the bridge reads sticky `m.rtc.member`
  and `rtc.slot` events (both type names, both slot spellings) and the ghost can publish the sticky
  membership with an MSC4140 delayed leave, open the room slot, and get its LiveKit token from the
  homeserver (MSC4195 `get_token`, falling back to `/sfu/get`). Config `matrixrtc_format` and
  `livekit_auth` (both default `auto`); optional `core.GhostRTCIntent`, provided by hostkit. Code, tests
  and docs are done (WP1-WP6); live verification (spec WP0, §9) is pending and happens through the new
  spans. Host side (WP7) waits for a tag.
- Next: Phase 4 (robustness).

## Goal

A SIP phone account (first target: JMP.chat SIP credentials) becomes Matrix calls where:

- **Inbound:** a call to the number rings **every Matrix client** of the owner (Element X, Element Web,
  Element Call) through MatrixRTC/LiveKit. Answering on any client connects audio, and the others stop
  ringing.
- **Outbound:** starting a call in a room for a number, or using `call <number>`, places a SIP call from
  the account.
- Each call leaves a timeline notice (missed / answered / outgoing + duration).
- **Optionally** (mautrix-jabber embedded mode): calls ring in the *same room* as that number's Cheogram
  texts.

Out of scope: legacy `m.call.*`, encrypted rooms, video, SMS/MESSAGE.

## Architecture

The call core is a library. Two "hosts" supply rooms and ghosts (ADR-006).

```
 PSTN ── SIP provider (JMP/Bandwidth) ──SIP + RTP(PCMU)──┐
                                                         │
          ┌──────────────────────────────────────────────▼───────────────┐
          │ core library: github.com/williamkray/mrtc-sip-bridge/pkg/...  │
          │  sipua/      REGISTER, INVITE/CANCEL/BYE, digest auth, SDP    │
          │  media/      RTP ⇄ PCM16 ⇄ Opus, 8k⇄48k, jitter, DTMF 4733    │
          │  rtc/        LiveKit participant as ghost; lk-jwt-service      │
          │  matrixrtc/  call.member, MSC4075 ring, MSC4310 decline        │
          │  calls/      per-call state machine: SIP dialog ⇄ RTC session  │
          │  callhost.go CallHost interface (rooms, ghosts, notices)       │
          └───────────────┬──────────────────────────────┬────────────────┘
                          │                              │
       ┌──────────────────▼─────────────┐   ┌────────────▼──────────────────────────┐
       │ Standalone host (this repo)    │   │ Embedded host (in mautrix-jabber,      │
       │ cmd/mrtc-sip-bridge,           │   │ behind network.calls.enabled)          │
       │ pkg/connector: bridgev2,       │   │ number → Cheogram portal/ghost; SIP    │
       │ portals per E.164, own ghosts  │   │ creds in jabber login metadata         │
       └──────────────────┬─────────────┘   └────────────┬──────────────────────────┘
                          └──── appservice API ──────────┘
                     Synapse ── Element X / Web / Call ── LiveKit SFU ◄─ lk-jwt-service
```

### Inbound call flow (target)

1. INVITE arrives on the registered contact. Reply `100 Trying`.
2. Caller number → E.164 → `host.ResolveCallRoom` (standalone: own portal; embedded: Cheogram portal).
3. Reply `180 Ringing`. The ghost sends `call.member` and then the MSC4075 `ring` notification.
   Every owner client rings.
4. Watch the room for the owner's `call.member`:
   - Owner joins → ghost joins LiveKit (OpenID → lk-jwt-service token), send `200 OK` with a PCMU/PCMA
     SDP answer, pipe media both ways.
   - MSC4310 decline / ring timeout → `603 Decline` / `480`. Notice: "Missed call" / "Declined".
   - Caller CANCEL → clear the ghost membership. Notice: "Missed call".
5. Teardown either way: SIP BYE ⇄ owner leaves (membership cleared or expired). Leave LiveKit, clear
   membership, post a notice with the duration.

### Outbound call flow (target)

1. Owner starts a call in a number's room (their `call.member` appears with no active call), or runs
   `call <number>`.
2. Ghost joins LiveKit, INVITE (digest auth) to the provider. Ringback: relay 183 early media, or play
   a generated tone.
3. 200 OK → ACK, media flows. Failure → notice with the SIP reason, ghost leaves.

## Roadmap

Each phase ends with something working and testable. Tick boxes as they're done.

### Phase 0 — Spikes (de-risk before structuring the code)

Throwaway code under `spikes/`. Write findings back into RESEARCH.md.

- [x] **S1 MatrixRTC ringing from an appservice ghost.** Ghost in an unencrypted room with the owner
      publishes `call.member` and the MSC4075 ring. Confirm Element X and Element Web ring. Get an
      OpenID token as the ghost, exchange it at lk-jwt-service, join LiveKit with server-sdk-go,
      publish a looping tone. Owner answers and hears it. *Find out:* exact event types, content and
      state keys (take them from the MSCs and check against current Element Call and lhns
      `pkg/calls`), membership expiry, how clients stop ringing.
- [x] **S2 SIP UA against the real JMP account.** sipgo: REGISTER with digest auth, receive INVITE,
      answer PCMU, record inbound RTP, send a tone back, place one outbound call. *Find out:* the
      current registrar host/port, NAT needs on the deployment host, codecs offered, re-register
      interval, whether `diago` is worth adopting (MPL-2.0, fine for us).
- [x] **S3 bridgev2 event hook.** How a bridgev2 connector receives `org.matrix.msc3401.call.member`
      and the MSC4310 decline. Needed by **both** hosts, so write it up so mautrix-jabber can reuse it.

Exit: S1 + S2 demonstrated. If media/NAT looks bad, re-open ADR-003.

### Phase 1 — Core library skeleton + standalone host

- [x] Go module `github.com/williamkray/mrtc-sip-bridge`, same mautrix version line as mautrix-jabber.
- [x] Define `CallHost`, `CallRoom`, `GhostIntent`, `LoginRef` in `pkg/` (ADR-006 sketch). Core
      packages import only `id`/`event` from mautrix plus these interfaces. Add a lint/test check that
      `pkg/{sipua,media,rtc,matrixrtc,calls}` never import `pkg/connector`.
- [x] Standalone host: `cmd/mrtc-sip-bridge` (bridgev2 `mxmain`), `pkg/connector` with a login flow
      (SIP username, password, registrar host:port, transport, display number), portals per E.164,
      ghosts per number.
- [x] Config: RTP port range, external IP, lk-jwt-service URL override (otherwise discover via the
      homeserver's RTC transports), ring timeout, default country code.
- [x] Phone normalisation (reimplement from mautrix-jabber `pkg/connector/cheogram.go`; same author).
- [x] Dockerfile/build.sh like mautrix-jabber (cgo + libopus). Publish the RTP UDP range.
- [x] REGISTER keep-alive with backoff. Bridge state reporting.
- [x] **T6 telemetry foundation**, per `docs/TELEMETRY-SPEC.md` §8.1: `pkg/obs` (core,
      OTel API only) and `pkg/telemetry` (host, SDK), import-boundary check, Phase 1 instrumentation
      (bridge start, login/connect/disconnect, SIP registration, MatrixRTC event ingest, bot commands,
      outbound Matrix HTTP) and metrics, and docs (README "Tracing and metrics", CLAUDE.md, CREDITS).
      Sub-tasks T6a..T6e, then an Opus review. Telemetry is part of every later phase's definition of
      done (below).
- [x] Live test against JMP + dev Synapse (needs the bridge reachable from Synapse).

### Phase 2 — Inbound calls MVP (standalone)

- [x] `calls/` inbound state machine. One active call per login at first.
- [x] `matrixrtc/`: membership publish/clear, ring, owner-membership watch, decline, ring timeout
      (default 30 s).
- [x] `rtc/`: token fetch, LiveKit join/leave, publish Opus from SIP audio, subscribe to the owner's
      track → PCMU.
- [x] `media/`: RTP send/receive, jitter buffer, resampling, silence fill.
- [x] Rooms are created unencrypted. If a room turns out to be encrypted, post a notice and log an
      error instead of producing a silent call.
- [x] Timeline notices.
- [x] Ring hygiene (from S1): pre-join LiveKit while ringing. Ring `lifetime` = ring timeout (~30 s).
      On answer, send a read receipt for the ring as the owner (double puppet) so push-driven clients
      (Sable Android) dismiss the ring. On CANCEL/timeout, clear membership **and** redact the ring.
- [x] Manual test: call the JMP number from a mobile, answer in Element X, 2+ minutes of two-way audio,
      hang up from each side.
- [x] **Telemetry (TELEMETRY-SPEC §8.2):** spans of §5.6 and §5.10 (`call.inbound`, `call.ring`,
      `call.answer`, `rtc.*`, `matrixrtc.membership.*`, `matrixrtc.ring.*`, `media.start/stop`,
      `call.teardown`, `call.session`, `call.notice.send`); call, rtc and media metrics of §4.3;
      `call_id` log field; lk-jwt-service client wrapped; hot-path rule (§4.2) with a benchmark showing
      zero allocations per RTP packet from telemetry; state-machine tests assert the trace shape with
      fake SIP/RTC/host, including CANCEL, decline, timeout and answer branches, and that the setup span
      ends at ACK.

### Phase 3 — Outbound calls (standalone)

- [x] Owner-initiated call detection, `call <number>` command, `ResolveIdentifier`.
- [x] INVITE with digest auth, early media / ringback, failure notices.
- [x] DTMF out (RFC 4733) through a room command while a call is active (for IVRs).
- [x] Manual live test of `docs/PHASE3-SPEC.md` §12.2 (outbound from Element X/Sable, `!sip call`, DTMF,
      failure notices).
- [x] **Telemetry (TELEMETRY-SPEC §8.2):** §5.7 and §5.8 (`cmd.call`, `cmd.dtmf`, `cmd.hangup` through
      `withSpan`), `sip.invite` with auth-challenge event, `call.ringback`, `mrtcsip.dtmf.sent`; a test
      proves DTMF digits never reach spans or logs.

### Phase J — mautrix-jabber embedded mode (unified rooms)

`[~]` = implemented on `phasej-embedding` / `local/sip-calls`, in progress until the live test (PHASEJ-SPEC §12).

Starts once Phases 2–3 work standalone. Work happens mostly in `../mautrix-jabber`, on a feature branch
there.

- [x] **J1 API freeze-lite:** tag `v0.x` of the core library (optional: jabber uses a local `replace`, PHASEJ-SPEC D2). Write a short `docs/EMBEDDING.md` here
      covering what a host has to implement, how to feed it MatrixRTC events, and lifecycle
      (start/stop per login).
- [x] **J2 jabber config + creds:** `network.calls.enabled` (default false) in `JabberConfig`.
      SIP credentials in `JabberLoginMeta` (registrar, username, password), set with a new bot command
      (`!jabber sip-login` / `sip-logout`). Calls start for a login only when both the flag and creds
      are present.
- [x] **J3 jabber CallHost:** `ResolveCallRoom` maps E.164 → Cheogram JID (`+1555…@<cheogram
      domain>`) → existing portal key, creating the portal like `ResolveIdentifier` does. Ghost = the
      Cheogram ghost. `SendCallNotice` goes through the portal. Owner = the login's Matrix user.
- [x] **J4 event plumbing:** reuse the S3 hook in mautrix-jabber to forward `call.member`/decline
      events to the core.
- [x] **J5 dedupe:** when calls are enabled, skip mautrix-jabber's XEP-0353 JMI "Incoming/Missed call"
      notices for calls the core is handling (or for all Cheogram numbers), so they aren't doubled.
- [x] **J6 build:** add libopus to mautrix-jabber's Dockerfile. Check that the binary still builds
      without calls enabled at runtime. Optionally add a `nocalls` build tag to drop the dependency.
- [x] **J7 docs:** README for both repos. Warn against running the standalone bridge and the embedded
      mode on the same SIP account.
- [x] **J8 telemetry (TELEMETRY-SPEC §8.2):** jabber calls `obs.Use` with its providers; jabber's
      `pkg/telemetry` gains metrics (port T6b back) and its span/log helpers delegate to `pkg/obs`
      (log-annotation convergence, spec §3.2); an embedded test asserts core spans carry jabber's
      resource `service.name` and nest under a jabber-created span passed in ctx; jabber README documents
      the new `mrtcsip.*` metrics and that setting `OTEL_EXPORTER_OTLP_ENDPOINT` now also enables metrics
      (`OTEL_METRICS_EXPORTER=none` to keep the old behaviour). Call `obs.Use` before `SIPConnector.Start`:
      `Use` rebuilds the instruments, which drops the registrations gauge callback registered earlier.

Things to sort out before J3 (record answers in an ADR):
- Cheogram portals for group MMS (comma-separated numbers) never get calls. Calls map only to 1:1
  portals.
- mautrix-jabber portals must be unencrypted for calls to work. Document it, and warn at runtime.

### Phase 4 — Robustness

- [ ] Persist call state. On restart, mark stale calls ended and clear leftover ghost memberships.
- [ ] Re-REGISTER on network change, SIP session timers / re-INVITE, membership refresh on long calls.
- [ ] Multiple SIP accounts per Matrix user. Concurrent calls. Hold/call waiting.
- [ ] **Telemetry (TELEMETRY-SPEC §8.2):** `call.membership.refresh`, `call.reinvite`,
      `sip.registrar.switch` on network change; restart cleanup emits one `bridge.cleanup_stale_calls`
      root with counts; concurrent-call tests assert spans of different calls never share a trace.

### Phase 5 — Polish

- OpenTelemetry tracing, metrics and log correlation moved out of Phase 5: see
  `docs/TELEMETRY-SPEC.md`. Foundation is task T6 in Phase 1, and instrumentation is part of the
  definition of done of Phases 2, 3, 4 and J (setup latency, RTP loss and jitter are metrics there).

### Phase 6 — Optional: bridge-issued credentials for ancillary SIP phones (ADR-007)

Not a blocker, nice-to-have.

- [ ] Built-in registrar: generate per-device SIP credentials (bot command), accept REGISTER from LAN
      devices.
- [ ] Fork inbound calls to registered devices alongside Matrix clients. First answer wins, others get
      CANCEL / ring stopped.
- [ ] Outbound from the device through the bridge's upstream registration (B2BUA, separate dialogs,
      RTP relay).

### Future / unscheduled

- **Dialer UX for new numbers (ADR-008).** Progress: the dialpad widget (option 1, `docs/WIDGET-SPEC.md`)
  is code complete for the standalone host on branch `widget-dialpad` (W1-W5: calls snapshot, hostkit
  actions and config, `pkg/widget`, UI, standalone wiring); jabber wiring (W6) and the live test (W7) are
  open. The Dialpad PWA (`docs/PWA-SPEC.md`, ADR-013) is code complete for the standalone host on the
  same branch (P1-P5: device store, hostkit commands, server API, front end, standalone wiring and docs);
  jabber wiring (P6) and the live test (P7) are open. Most clients have no dialpad. Baseline: bot command /
  DM-the-ghost (Phase 3). Cheap experiment: PSTN third-party protocol lookup (Element Web dialpad).
  Later: dialpad widget. Long term: an Android helper app with a Telecom `ConnectionService` /
  `PhoneAccount` (Cheogram-style native dialer integration), or a dialer added to existing/forked
  clients.
- G.722 / Opus over SIP if the provider offers them. Video is out of scope unless needed.

## Risks

| Risk | Mitigation |
|---|---|
| NAT / RTP reachability | External IP + port range config, symmetric RTP, keepalives. Validate in S2. |
| MatrixRTC unstable event types change | One file of constants citing the MSC and the Element Call version checked. |
| Encrypted rooms → silent calls | Create unencrypted rooms. Detect encryption and post a notice instead. |
| mautrix version skew between hosts (Phase J) | Keep the core's mautrix surface tiny. Bump both repos together. |
| Two hosts registering one SIP account | Documented. Optionally detect via registrar contact list. |
| JMP SIP minute caps (unverified) | Document. Out of our control. |
| Audio quality / jitter | Simple jitter buffer first. Measure. Fall back per ADR-003. |
| lk-jwt-service room-name mismatch | Always fetch tokens via the auth service as the ghost. |
| Sticky/slot/delayed events need experimental Synapse flags; AS masquerade for `get_token` and delayed events is unverified | `auto` falls back to the legacy membership and `/sfu/get`; telemetry shows the path taken (`matrixrtc.plan.*`, `rtc.token.fallback`). |
| Clients disagree on the sticky dialect (`m.rtc.member`/`m.call#room` vs unstable/`ROOM`) | Read all, write in the owner's dialect; dual for bridge-initiated calls (ADR-014). |

## Testing strategy

- Unit: SDP offer/answer, phone normalisation, call state machine (fake SIP + fake RTC + fake
  CallHost), MatrixRTC event fixtures.
- Integration: `sipp` or local Asterisk as a fake provider plus local Synapse + LiveKit +
  lk-jwt-service in docker-compose.
- Manual: real JMP account, Element X on a phone, Element Web.
