# Embedding the call core in another bridge

Status: implemented and validated against a live JMP account and Synapse/LiveKit deployment (PLAN
Phase J). Design and rationale: `docs/PHASEJ-SPEC.md` and ADR-006/ADR-011
in `docs/DECISIONS.md`. The first embedding host is
[jmpchat-matrix-bridge](https://github.com/williamkray/jmpchat-matrix-bridge) (formerly mautrix-jabber) (build tag `sipcalls`, included in its
default images). Part 1 is for people writing a host; part 2 is
for users of the jabber embedded mode.

---

## Part 1: host contract

The call core (`pkg/core`, `pkg/calls`, `pkg/rtc`, `pkg/media`, `pkg/matrixrtc`, `pkg/sipua`,
`pkg/phone`, `pkg/obs`) is a library. A host is a bridgev2 bridge that supplies Matrix rooms and
identities; the core supplies SIP, MatrixRTC, media and call state. Two host-class helper packages exist
so hosts do not copy tested code:

- `pkg/sipaccount` (core-class, no bridgev2 import): process-wide `Runtime` and per-login `Account`.
- `pkg/hostkit` (host-class, may import bridgev2): config struct and example YAML, ghost intent adapter,
  call-stack construction, call commands, power levels, `WithSpan`, field prompts, RTC login resolver.

### 1.1 Implement `core.CallHost`

`core.CallHost` (`pkg/core/callhost.go`) is the whole host surface:

| Method | Contract |
|---|---|
| `ResolveCallRoom(ctx, login, e164)` | Room and ghost for a number, creating the portal if needed. Must not do slow network I/O (it sits in front of the SIP 180 Ringing). |
| `LookupCallRoom(ctx, login, roomID)` | `CallRoom` for an existing 1:1 call portal of that login, else `core.ErrNotCallRoom`. Creates nothing and changes no state. |
| `SendCallNotice(ctx, room, text)` | Short notice ("Missed call", "Call, 1m20s") as the ghost. |
| `OwnerMXID(login)` | The Matrix user owning the login, or "". |
| `MarkReadAsOwner(ctx, login, roomID, eventID)` | Read receipt as the owner; `core.ErrNoDoublePuppet` when unavailable. |

`LoginRef` is an opaque host login ID (jabber: the `UserLoginID`). The core never parses it; use
`Ingest.SetLocalNumber` so span attributes show the SIP account's number rather than anything derived
from the ref.

Rooms must be able to carry calls: the owner needs power level 0 for `call.member` and for the MSC4143
`rtc.slot` that clients open when the call is started from their own call button
(`matrixrtc.CallPowerLevelTypes`; `hostkit.CallPowerLevelOverrides`, `hostkit.EnsureCallPowerLevels`, `hostkit.WithCallOverrides`), the
ghost must be joined, and the room should be unencrypted (see 1.6).

### 1.2 Feed events into the core

Create one `core.Ingest`, then register the MatrixRTC handlers **in PostInit**, before the event
processor starts:

```go
rtc := core.NewIngest(core.LogSink{Log: &lg})
core.RegisterMatrixRTCHandlers(eventProcessor, hostkit.ResolveRTCLogin(br, accept), rtc)
// later, in Start, once the call stack exists:
rtc.SetSink(callManager)
```

`hostkit.ResolveRTCLogin` maps an event's room to a login and calls `accept(loginID)`; return false for
logins without a SIP account so their rooms produce no spans. Events that arrive before `SetSink` are
dropped by the core, so a late `Start` is safe.

### 1.3 Telemetry before Start

The core obtains tracers and meters only through `pkg/obs`. A host must call `obs.Use` (directly, or by
calling `pkg/telemetry.Init`, which does it) **before** it creates the `sipaccount.Runtime`:
`obs.Use` rebuilds the instrument registry and would drop the registrations gauge registered earlier.
Without it, everything is a no-op. `telemetry.Options.ServiceName` sets the OTel `service.name` for the
host (jabber passes `jmpchat-matrix-bridge`). `obs.AnnotateLogger`, `obs.Logger` and `obs.LoggerSC` are the
shared log-correlation primitives; a host's own log helpers should delegate to them so
`trace_id`/`span_id` are never written twice.

### 1.4 One `sipaccount.Runtime` per process

`sipaccount.NewRuntime(ctx, cfg.NetConfig(), log)` owns the bind and external IP, STUN, the RTP port
range, the SIP listen-port pool and the `mrtcsip.sip.registrations` gauge. Create exactly one per
process (in the host's `Start`) and `Close` it in `Stop`. `hostkit.NewCallStack(br, &cfg, deps)` builds the
`calls.Manager` on top of it; `deps` carries the `CallHost`, a `Dial` func (`Account.Dial`), a
`LoginNumber` func and a logger.

### 1.5 Account lifecycle

One `sipaccount.Account` per login that has SIP credentials (`rt.NewAccount`). Rules the jabber host
follows and other hosts should too:

- Tie the account to the **login**, not to a transport session: start it when the login is loaded (and the
  runtime is up, whichever comes last), stop it when the login is terminally disconnected. Reconnects of
  the Matrix-facing network connection must not touch SIP.
- Never start a second UA for one account (two Contacts, doubled ringing). In jabber, bridgev2 disconnects
  the old client before it loads the replacement, so a jabber re-login stops the account (ending active
  calls) and re-registers SIP from the stored credentials; there is no takeover.
- Replace credentials by stopping the old account **before** registering the new one
  (`sipaccount.Register` blocks until a result, with a timeout, and does not save anything). Save the
  credentials only after it reports Registered.
- `Account.Stop` unregisters only this bridge's Contact on a detached context. End active calls first
  (`calls.Manager.EndLogin`).
- `OnState` runs on the UA goroutine: never block in it. Hand notices off to a goroutine.
- SIP state is separate from the host's own connection state.

### 1.6 Encryption

Call rooms must be unencrypted: an encrypted room gives silent Element Call audio (ADR-002). The core
refuses to ring in an encrypted room and posts a notice instead. A host that cannot force rooms
unencrypted should warn at startup when its default is encrypted (`hostkit.RoomEncrypted` is the check).

### 1.7 Build tags

- Host builds use `nolibopusfile` (hraban/opus without opusfile); cgo and `pkg-config opus` are required.
- Hosts without libolm add `nocrypto`.
- Keep embedding code behind a host-owned build tag so a host that does not want SIP never links it.
  jmpchat-matrix-bridge uses `sipcalls`; the untagged build only contains SIP-free extension seams.
- Core packages must not import `pkg/telemetry` or the OTel SDK; `internal/importcheck` enforces this.
  `pkg/hostkit` and `pkg/telemetry` are host-class.

---

## Part 2: user guide, jmpchat-matrix-bridge embedded mode

In embedded mode the texts and the calls of a JMP/Cheogram number share **one Matrix room and one
ghost** (`+1NXXNXXXXXX@cheogram.com`). Incoming calls ring in the caller's Cheogram room (created on
demand). Outgoing calls start from an existing Cheogram 1:1 room (press call, or `call` with no number)
or from `call <number>`.

This is part of jmpchat-matrix-bridge's default build (build tag `sipcalls`). Calls stay off until
`network.calls.enabled: true`.

### Warning: one SIP account, one bridge

**Never register the same SIP account from two bridges at once.** JMP forks every call to all
registered Contacts, so a standalone `mrtc-sip-bridge` and the embedded mode on the same account would
both ring and fight over answering. Stop (or `sip-logout` in) the standalone first, and restore it only
after the embedded login is removed. The same goes for any other client you want to keep registered
(an ATA is fine: JMP forks to it and to the bridge on purpose).

### Config

Under the jabber `network:` section (everything else at the generated defaults):

```yaml
network:
    cheogram: true          # required when calls are enabled
    calls:
        enabled: true       # default false
        sip_port_min: 5070
        sip_port_max: 5079
        rtp_port_min: 40200
        rtp_port_max: 40300
        external_ip: <public IP of the host>
        stun_server: <host:port>
        livekit_service_url: <byte-identical to the well-known livekit service URL>
```

The remaining `calls` keys are the same names, defaults and validation as the standalone `network:`
section (see README "Configure"); `./jmpchat-matrix-bridge -e` prints them all. `calls.enabled: true` without
`network.cheogram: true` is a startup error. `encryption.default: true` logs a startup warning (see
Encrypted portals).

A ready-to-adapt service config and compose snippet live in `docs/PHASEJ-SPEC.md` §9.3 (local copies
in the gitignored `deploy-dev/jabbercalls/`).

### Commands

With the bridge's configured prefix (the dev service uses `!jc`):

| Command | What it does |
|---|---|
| `sip-login` | Asks for registrar, username, password etc. one message at a time (your password message is redacted), registers **before** saving. Takes no arguments. In jabber, run it in the management room (your DM with the bridge bot); in a chat room it refuses, because the answers would be sent to the contact. |
| `sip-logout` | Stops the SIP account and deletes the stored credentials. |
| `sip-status` | Calls enabled or not, Cheogram on/off, encryption warning, masked SIP user, registrar, number, registration state. Shows no secrets. |
| `call [number]` | Ring your devices from the number's ghost; answer and the bridge dials. In a Cheogram 1:1 room the number is optional. |
| `hangup` | End the active call. |
| `dtmf <digits>` | Send keypad tones during a call; your command message is redacted and digits are never logged. |
| `roster-only-calls on\|off\|status` | Ring for incoming calls only from numbers in your Jabber roster; other callers get a 480 (voicemail) and a notice in the management room. Gates SIP calls only: XEP-0353 call notices still appear. Off by default; the Dialpad widget has the same toggle. |
| `save-contact <name>` | In a one-to-one chat: save the chat's contact (Cheogram number or JID) to your roster under `<name>`, replacing an existing name. Works in every jabber build; the widget's Name / Save contact use the same semantics. |

Only Cheogram 1:1 rooms (`<E.164>@cheogram.com`) carry calls. Group MMS rooms, MUC rooms and non-Cheogram
DMs never do. Number policy (country code, allowed prefixes, short codes, self-dial) is the standalone's.
When the account first registers (and on every `sip-login`) the bridge raises the call power levels of
your Cheogram 1:1 rooms so the call button works.

### Ports

SIP signalling uses `sip_port_min..sip_port_max` (UDP and TCP) and RTP audio uses
`rtp_port_min..rtp_port_max` (UDP). Publish exactly what you configure. The dev service uses 5070-5079 and
40200-40300 so it can sit next to the standalone bridge's 5060-5069 and 40000-40100 (only one of them may
hold the registration).

### Element X outbound pickup bug

Pressing call in a Cheogram room from **Element X** may ring forever and hang up after 30 s even though
the bridge dialled and audio flows. This is an upstream Element Call bug: it waits for the first other
member of the room, which can be the bridge bot instead of the ghost, and has an open TODO to ignore
functional members:
[`CallNotificationLifecycle.ts` line 128](https://github.com/element-hq/element-call/blob/8be229a438dc363972bdae5f6198607998a851d8/src/state/CallViewModel/CallNotificationLifecycle.ts#L128).
This is documented only; there is no bridge-side workaround. Use `call <number>` (or `call` in the room)
from Element X, or press call in Sable.

### Encrypted portals

jabber may create encrypted portals (`encryption.default: true`) and the embedded host does not override
that, because texts may want it. Calls only ring in unencrypted rooms: in an encrypted room the call is
rejected to voicemail with a notice, and `sip-status` reports the setting. For a number you want to call,
use an unencrypted portal (the dev service runs with `encryption.default: false`).

### Telemetry behaviour

In the `sipcalls` build jabber initialises OpenTelemetry through this repo's `pkg/telemetry`, with
`service.name=jmpchat-matrix-bridge` unless `OTEL_SERVICE_NAME`/`DD_SERVICE` say otherwise. Differences from the
public jabber build:

- Setting `OTEL_EXPORTER_OTLP_ENDPOINT` now also enables **metrics** (`mrtcsip.*` plus Go runtime
  metrics). `OTEL_METRICS_EXPORTER=none` turns them off, `OTEL_TRACES_EXPORTER=none` turns traces off.
  Env resolution follows this repo's README "Tracing and metrics (OpenTelemetry)".
- Core spans (`sip.*`, `call.*`, `rtc.*`, `matrixrtc.*`, `media.*`) appear under the jabber service.
- New host spans: `bridge.sip_connect` and `bridge.sip_disconnect` (roots; the retry is a linked root),
  `bridge.calls.pl_sweep` (linked root), `bridge.calls.open_chat` (linked root, the widget's Message),
  `bridge.calls.inbound_filtered` (roster-only rejection), `cmd.roster_only_calls`, `cmd.save_contact`,
  `cmd.sip_login`, `cmd.sip_login.submit` (root consumer),
  `cmd.sip_logout`, `cmd.sip_status`, `cmd.call`, `cmd.hangup`, `cmd.dtmf`.
- Log lines under spans carry `trace_id`/`span_id`/`dd.trace_id`/`dd.span_id` exactly once.
- Matrix HTTP calls are traced (jabber's transport wrapper) but there are no Matrix HTTP metrics in
  embedded mode.
- Privacy rules are unchanged: no audio, DTMF digits, SDP, raw SIP, passwords or tokens; the SIP username
  only masked; metrics never carry identities.

### Widget (Dialpad)

A host adds the room widget by wiring `pkg/widget` (host-class: it imports bridgev2) next to the call
stack:

    srv, err := widget.New(cfg.Widget, widget.Deps{
        Bridge:             br,
        Calls:              func() hostkit.CallAPI { ... },  // nil interface while starting
        ResolveRoom:        ..., // (user, room) -> widget.RoomContext, or widget.Refuse(widget.MsgNotBridgeRoom)
        EligibleRooms:      ..., // rooms that should carry the widget (1:1 number portals + management room)
        DefaultCountryCode: cfg.DefaultCountryCode,
        Log:                log,
    })
    widget.Mount(br, srv)                // AS.Router.Handle(prefix, handler)
    srv.Start()                          // installer worker, startup sweep, periodic sweep, janitor
    commandDeps.Widget = srv.Installer() // enables the `widget` command and trigger 4
    ...
    srv.Stop()

Hooks the host owns:

- `srv.Installer().Enqueue(room, "Call <e164>")` when `ResolveCallRoom` creates a room (never blocks).
- `srv.Installer().RequestSweep(ctx, widget.TriggerLogin)` after each login's power-level sweep.
- When `widget.enabled` is false and `remove_when_disabled` is true, run `go widget.Remove(ctx, deps)`
  once at startup instead of creating the server.

ResolveRoom refuses rooms that are neither an owned number portal nor the user's management room. For a
valid room whose login is not ready (jabber: not an XMPP login, or no registered SIP account), it returns
a RoomContext with `Login` nil and `Refusal` set to the command's refusal text, and no error, so the
widget shows the hint. Resolution runs on every action. Config: `widget:` lives in the shared
`hostkit.Config` (standalone `network.widget`; jmpchat-matrix-bridge `network.calls.widget`). `hostkit.CopyKeys`
copies it and generates the secret.

Dialpad PWA (`widget.pwa.enabled`, PWA-SPEC §11): call `widget.OpenDeviceStore(ctx, br, cfg, log)` before
`widget.New` (an error fails start), pass `Deps.Devices` and `Deps.ResolveDevice` (same readiness rules as
the commands; return `widget.KindDevice`, `Login` nil plus `Refusal` when not ready, and
`widget.ErrDeviceLoginGone` when the login is missing or not the user's), and give the commands a
late-bound `hostkit.CommandDeps.Dialpad` that delegates to `srv.DialpadLinks()` once the server exists
(nil interface when the PWA is off). `Notify`, `Redact`, `DeviceOwner` and `CommandPrefix` have defaults.

Optional features (WIDGET-SPEC §7.5): set `Deps.Features` to a value implementing any of these
`pkg/hostkit` interfaces (type-asserted once in `widget.New`; nil = none, the UI hides the controls):

    type RosterOnlyPrefs interface {
        RosterOnly(ul *bridgev2.UserLogin) RosterOnlyState                                  // memory only, never blocks
        SetRosterOnly(ctx context.Context, ul *bridgev2.UserLogin, on bool) (RosterOnlyState, error)
    }
    type ChatOpener interface {
        OpenChat(ctx context.Context, ul *bridgev2.UserLogin, e164 string) (ChatRoom, error) // Pending past the deadline
    }
    type ContactSaver interface {
        SaveContact(ctx context.Context, ul *bridgev2.UserLogin, e164, name string) (SavedContact, error)
    }
    type ContactLookup interface {
        ContactName(ul *bridgev2.UserLogin, e164 string) (name string, saved bool)          // memory only, never blocks
    }
    type ContactLister interface {                                                          // the Contacts tab
        ListContacts(ul *bridgev2.UserLogin, query string, offset, limit int) (ContactPage, error) // memory only, never blocks
    }

Rules: `RosterOnly` and `ContactName` run on every state poll, so they read memory only. `ListContacts` runs
per `GET contacts` request (5/s per user) and also reads memory only (copy under your lock, then filter, sort and
slice outside it). It lists **named phone contacts only**, with every name passed through
`hostkit.DisplayContactName` before filtering and sorting, sorted by lowercase name then E.164, searched
by the rule in the `ContactLister` godoc (name substring, or at least 2 digits of the number), and returns
`ContactPage{Loaded: false}` until your contact list has loaded. Never log or trace names or queries. Every method
returns before its context ends. `OpenChat` runs no history backfill inline (start it afterwards in its
own goroutine) and returns `ChatRoom{Pending: true}` when creation outlives the deadline. Failures the
user should see are `hostkit.NewActionError(code, msg)` (`invalid_number`, `invalid_name`,
`unavailable`; `$cmdprefix` is expanded in the message); anything else is reported as a generic
internal error and logged scrubbed. Contact names must never be logged or traced.

jmpchat-matrix-bridge (local `sipcalls` build): `sipcalls_widget.go` implements this. A ready login is
`commandLoginWithAccount`, portals are `isCallPortal` (Cheogram 1:1, never group MMS or MUC), and titles
come from `cheogramE164`. `sipcalls_widgetfeatures.go` implements the optional features: roster-only
uses the login's cached call preferences and roster counters, Save contact is the `save-contact` roster
set (an unchanged name sends nothing), and Message reuses `ResolveCallRoom` (the room is the same one
`pm` creates, plus the widget) detached from the request, with the MAM backfill started afterwards.
