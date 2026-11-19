# Architecture decisions

> Note: the host bridge mautrix-jabber was renamed to jmpchat-matrix-bridge in 2026-10 (v0.6.0); this document keeps the old name.

Short ADR-style records. Each one says what was decided, why, and what would change the decision.
Add new entries at the bottom. Don't rewrite old ones; mark them superseded instead.

---

## ADR-001: Calls go in a separate SIP bridge, not in mautrix-jabber via Jingle

**Status:** accepted (2026-10-16)

**Context.** mautrix-jabber already handles chat with JMP/Cheogram numbers. Earlier planning assumed
calls would come later through Jingle (XEP-0166/0167) in that bridge. Today mautrix-jabber only reads
XEP-0353 Jingle Message Initiation and posts "Incoming / Missed call" notices
(`pkg/connector/xmpp_jingle.go`). It does no session negotiation and carries no media.

**Decision.** Calls are handled by this project, a standalone bridge that uses **SIP credentials**.
mautrix-jabber stays chat-only and keeps its JMI notices.

**Why.**
- In both designs the bridge has to be a real media endpoint (RTP in, WebRTC/LiveKit out).
  Jingle adds a lot on top of that: ICE, DTLS-SRTP and Jingle negotiation over XMPP, all inside a bridge
  that is already the biggest one in the stack. SIP signalling with plain RTP is much simpler,
  and that's exactly what JMP's SIP account (and ATAs) speak.
- SIP isn't tied to one provider. The same bridge works with JMP, voip.ms, a home Asterisk, and so on.
- It keeps failure domains apart. A crash in call/media code doesn't take down chat bridging.
- Multi-device ringing doesn't need Jingle's forking. MatrixRTC rings every client of the Matrix user
  (see ADR-002).

**Costs / consequences.**
- In standalone mode, a JMP number's texts and calls show up in **different Matrix rooms**. The
  optional embedded mode in mautrix-jabber puts them in one room (ADR-006).
- If JMP also rings XMPP/Jingle clients, mautrix-jabber may post an "Incoming call" notice for the same
  call this bridge is ringing. That's acceptable. It can be turned off later.

**Would revisit if:** a mature Go Jingle+media stack appears, or JMP drops SIP accounts.

---

## ADR-002: Matrix side uses MatrixRTC (Element Call / LiveKit), not legacy `m.call.*`

**Status:** accepted (2026-10-16)

**Context.** Matrix has two calling systems:
1. Legacy 1:1 VoIP (`m.call.invite/answer/candidates/select_answer/hangup`), peer-to-peer WebRTC.
   Element X does **not** support it, and it's on a deprecation path.
2. MatrixRTC (MSC4143) with the LiveKit backend (MSC4195), as used by Element Call (standalone and
   embedded in Element X / Element Web). Membership is announced with `org.matrix.msc3401.call.member`
   state events. Ringing is an MSC4075 RTC notification (`m.rtc.notification` /
   `org.matrix.msc4075.rtc.notification`, `notification_type: ring`). Declining is MSC4310.

**Decision.** Target MatrixRTC only. The bridge joins the LiveKit room as one participant (a ghost user
standing for the remote phone number) and publishes/subscribes audio.

**Why.** It's what current clients ring on. Fan-out comes for free: one ring notification in the portal
room rings every logged-in client of the user, and the first one to join answers. Clients stop ringing
when they see the user's own membership, or a decline.

**Consequences.**
- The homeserver deployment must have a MatrixRTC transport: a LiveKit SFU plus the MatrixRTC auth
  service (`lk-jwt-service`), advertised the way Element X expects.
- **Portal rooms are unencrypted** (accepted as a given for now). Element Call turns on per-participant SFrame E2EE in rooms with
  `m.room.encryption`. Plain audio published by the bridge then decodes to silence while everything
  *looks* connected (documented by lhns/matrix-sip-bridge). The bridge must refuse or warn on encrypted
  portals.
- bridgev2 doesn't route `call.member` / RTC events to network connectors, so we subscribe to those
  event types ourselves (see PLAN.md Spike S3).

**Legacy `m.call.*` support is explicitly out of scope** (user decision, 2026-10-16).

**Would revisit if:** Matrix standardises a different RTC backend.

---

## ADR-003: Self-contained Go media path, no Asterisk or livekit-sip

**Status:** accepted, pending Spike S2 (2026-10-16)

**Options considered.**
- **A. One Go binary:** `sipgo` for SIP (REGISTER, INVITE, CANCEL, BYE, digest auth), RTP and
  G.711↔Opus transcoding via `github.com/livekit/media-sdk` (g711, opus, rtp; Apache-2.0, Opus through
  cgo `hraban/opus`), and the LiveKit Go SDK (`server-sdk-go`) to join the room as a participant.
- **B. lhns/matrix-sip-bridge style:** Asterisk handles SIP/PSTN, and livekit-sip dials into an Asterisk
  ConfBridge. The bridge only does signalling.
- **C. Hybrid:** the bridge does REGISTER and livekit-sip handles media. livekit-sip doesn't support
  REGISTER (there's an open upstream PR, livekit/sip#774), and it expects to be the INVITE target, so
  wiring the two together is awkward.

**Decision.** Option A.

**Why.** The target user has *SIP credentials* (a registering UA account), not a trunk. A single binary
that registers like a phone is the simplest thing to deploy alongside a homeserver: no PBX, no Redis,
no livekit-sip. The transcoding code is the same code livekit-sip uses (media-sdk). Expected call
volume is tiny (one user, a handful of calls), so CPU cost doesn't matter.

**Consequences.** We own RTP, jitter and NAT handling, which is real work (see PLAN.md risks). The
build needs cgo plus libopus, which matches mautrix-jabber's existing Docker build that uses cgo and
libolm.

**Would revisit if:** media quality or NAT problems eat more than about a phase of effort. Then fall
back to B (with livekit-sip) or to C once livekit/sip ships REGISTER.

---

## ADR-004: Built on mautrix bridgev2

**Status:** accepted (2026-10-16)

**Decision.** Use `maunium.net/go/mautrix/bridgev2`, the same version line as mautrix-jabber (v0.32.x),
for appservice, config, DB, logins, portals (one per remote number) and ghosts. Use our own handlers for
MatrixRTC events.

**Why.** Login/portal/ghost/double-puppet plumbing is solved there, and it matches the sibling project,
so conventions (telemetry, Docker build, config generation) carry over. lhns/matrix-sip-bridge shows
bridgev2 plus a separate call subsystem works.

**Note.** Double puppeting is effectively required. Without it, the user has to accept portal invites by
hand before a call can ring.

---

## ADR-005: Calls only, no SIP MESSAGE / SMS

**Status:** accepted (2026-10-16)

Texting stays in mautrix-jabber (Cheogram). This bridge sends only a short timeline notice per call
("Missed call", "Outgoing call · 1m20s") so the portal shows history and unread state.

---

## ADR-006: Call core is a library with two hosts (standalone bridge, embedded in mautrix-jabber)

**Status:** accepted (2026-10-16)

**Context.** The user wants a JMP number's texts (mautrix-jabber, Cheogram) and calls to be able to
share **one Matrix room**, controlled by a config flag. The bridge should also work on its own for
non-JMP SIP accounts.

**Options considered.**
- **A. Two appservices sharing a room.** The SIP bridge finds the mautrix-jabber portal for the number
  (via its DB or an API) and puts its own ghost in it. Rejected: two ghosts stand for one person,
  neither bridge owns the room state, invites and power levels need coordinating across appservices,
  and the two bridges have to agree on portal lookup through a side channel.
- **B. Library-first core, two hosts (chosen).** All call logic (SIP UA, media, LiveKit participant,
  MatrixRTC signalling, call state machine) lives in importable packages under
  `github.com/williamkray/mrtc-sip-bridge/pkg/...`. It talks to "the bridge" only through a small
  `CallHost` interface. Two implementations:
  1. **Standalone host** (`cmd/mrtc-sip-bridge`, `pkg/connector`): its own bridgev2 connector, portals
     keyed by E.164 number, its own ghosts.
  2. **Embedded host in mautrix-jabber**, enabled by a config flag (for example `network.calls.enabled`).
     It maps the SIP caller's number to the existing Cheogram portal and ghost
     (`+1555…@cheogram.com`), so the call rings in the same room as the texts, from the same ghost.
     One appservice, no cross-bridge coordination.
- **C. Merge everything into mautrix-jabber.** Rejected: non-JMP SIP users would have to run an XMPP
  bridge.

**Interface sketch** (to be firmed up in Phase 1):

```go
type CallHost interface {
    // Room and the ghost that stands for the remote party; create the portal if needed.
    ResolveCallRoom(ctx context.Context, login LoginRef, e164 string) (CallRoom, error)
    // Text/notice in the room ("Missed call", "Call · 1m20s").
    SendCallNotice(ctx context.Context, room CallRoom, text string) error
    OwnerMXID(login LoginRef) id.UserID
}
type CallRoom struct {
    RoomID id.RoomID
    Ghost  GhostIntent // can send state/events, get an OpenID token, as the ghost
}
// Hosts feed MatrixRTC events (call.member, decline) into core.HandleMatrixRTCEvent.
```

**Constraints this puts on the code from day one.**
- `pkg/` core must not import `pkg/connector` (the standalone host), and should depend on mautrix only
  for basic types (`id`, `event`) plus a minimal intent interface. That keeps mautrix version skew
  between the two hosts manageable. Both hosts still need compatible mautrix versions in practice.
- SIP credentials are per login. In embedded mode they live in mautrix-jabber's login metadata, set by
  a bot command (for example `!jabber sip-login`). Never register the same SIP account from both hosts.
- The embedded host turns off mautrix-jabber's XEP-0353 JMI notices for numbers it rings itself, to
  avoid duplicate "Incoming call" notices.
- Dependency licenses must stay compatible with mautrix-jabber importing the core (see docs/CREDITS.md).

**Would revisit if:** tying the two release cycles together turns out painful. Fallback is option A.

---

## ADR-007: Bridge-issued SIP credentials for ancillary SIP phones (optional, later)

**Status:** proposed, not a blocker (2026-10-16)

The bridge could run a small built-in registrar: it generates SIP credentials, a hardware phone or ATA
registers to the bridge, and that device becomes **one more ring destination** next to the Matrix
clients (first answer wins). Outbound calls from the device are relayed through the bridge's upstream
registration (B2BUA). As a side effect, the bridge stays the only contact registered with the provider.
Advanced work. Planned after the core works, and never on the critical path.

---

## ADR-008: Dialing new numbers from Matrix (dialer UX)

**Status:** proposed, future work (2026-10-16)

**Problem.** Calling a number that already has a portal room just means pressing the call button in
that room. But most Matrix clients (Element X especially) have no dialpad, so there's no natural way to
call a number you've never called before.

**Options, roughly in order of effort.**

0. **Baseline, works in every client (Phase 3):** dialing a new number means creating its portal.
   - Bot command `call <number>` / `pm <number>` in the management room.
   - Start a DM with the ghost MXID (for example `@sip_15551234567:server`), which bridgev2 can turn
     into a portal.
   - The bridge then rings the user's own devices ("tap to connect"), or the user presses call in the
     new room.
   Clunky but universal.
0b. **Third-party protocol lookup (`m.protocol.pstn` / `im.vector.protocol.pstn`):** the appservice
   advertises a PSTN protocol and answers `/thirdparty/user/<protocol>?m.id.phone=…` with the ghost.
   Element Web shows a dialpad when the homeserver supports PSTN lookup. **UNVERIFIED:** Element Web's
   dialpad may start a *legacy* `m.call` after resolving, which this bridge doesn't support (ADR-002).
   Even so, the DM-creation half could work. Cheap to try.
1. **Matrix widget** (a dialpad web page that calls a bridge HTTP endpoint or sends a bot command).
   Only clients with widget support (Element Web/Desktop, not Element X). Low effort.
2. **Helper app:** a separate, minimal Matrix client that logs into the user's account, shows a
   phone-like UI (dialpad, recents from the portal rooms, contacts), and handles only call events for
   bridge portals. Media through the LiveKit SDK, the same as Element Call. Needs care so it doesn't
   double-ring alongside Element X.
3. **Dialer in existing or forked clients** (Element, Sable, …): a native dialpad plus "call number"
   configuration that maps to the bridge's ghosts. The best UX inside those clients, but forks are a
   maintenance burden. Upstreaming depends on other projects.
4. **Android native dialer integration**, the ideal on Android: register a Telecom `PhoneAccount`
   through a `ConnectionService`, the way the Cheogram Android app registers as a calling account. The
   system dialer and contacts can then place calls "via Matrix/JMP". Inbound bridge calls could ring
   through the native incoming-call UI. This needs a native app, so in practice it's option 2 (or 3)
   with Telecom integration.

**Leaning.** Ship 0 with Phase 3. Try 0b cheaply. Treat **2+4 (an Android helper app with
ConnectionService)** as the long-term native answer, and 1 as an optional extra for web users. Revisit
once inbound and outbound calling are solid.

---

## ADR-009: Adopt diago behind `pkg/sipua`; build without crypto

**Status:** accepted (2026-10-16)

**Context.** Spike S2 passed with diago (v0.41.0): it registered to JMP, held the registration, and
handled digest auth.

**Decision.** Production uses diago v0.41.0 for SIP signalling only, and only behind the `sipua.UA`
interface, so it can be swapped for raw sipgo + media-sdk later. Three workarounds sit on top of diago:

1. AOR `To:` fix on REGISTER.
2. Per-attempt `Via` reset, because diago's `register()` does not remove `Via` and a second `Register`
   would reuse the old branch.
3. Unregister of our own Contact only, by editing the transaction's `Origin` request (`Expires: 0`) and
   sending it with `Qualify`, because diago's `Unregister` sends `Contact: *` and would drop other
   devices on the account.

Registrar IP pinning is added on top (resolve once, switch IPs only after two consecutive failures).
The periodic refresh is a full `Register` (not `Qualify`), so the Contact stays in step with the
response's Via `rport`/`received` after a NAT rebinding.

**Known limitation.** The refresh interval is fixed (default 25 s) because diago does not expose the
granted `Expires`.

**Build.** The bridge builds with `-tags nocrypto` (no libolm), because portals must be unencrypted
(ADR-002) so end-to-bridge encryption has no use.

**Revisit** if diago's API churn or media threading becomes a problem (fallback per S2 notes, section 2).

---

## ADR-010: The bridge bot leaves 1:1 number portals

**Status:** rejected (2026-10-17) by the user before implementation. The root cause is a known, open
Element Call TODO (`// TODO: Respect io.element.functional_members`,
https://github.com/element-hq/element-call/blob/8be229a438dc363972bdae5f6198607998a851d8/src/state/CallViewModel/CallNotificationLifecycle.ts#L128),
and bridgev2 already marks the bot as a functional member. We don't design bridge behaviour around a
client bug that upstream already plans to fix. The limitation is documented in the README instead,
with `!sip call` and Sable as workarounds. The analysis below is kept for reference.
Implementation: `docs/PHASE3-SPEC.md` §17.

**Problem (seen live, Phase 3).** The owner presses call in a portal (members: owner, ghost, `@sipbot`)
in Element X. The bridge publishes the ghost's `call.member` about 80 ms after the ring, dials, the
PSTN side answers, and two-way audio and DTMF work. Element X still shows "ringing" and leaves the call
after exactly 30 s (the notification `lifetime`).

**Cause (checked in source).**
- Element X opens Element Call with intent `start_call_dm` / `start_call_dm_voice` when the room is a DM
  (iOS `ElementX/Sources/Services/Room/Room.swift:13-18`, Android `DefaultCallWidgetProvider.kt:52`).
  Both apps build the Rust SDK with `DmRoomDefinition::TwoMembers` (Android
  `RustMatrixClientFactory.kt:197`, iOS `ClientFactory.swift:127`). That definition is "one `m.direct`
  target, and at most 2 active members once `io.element.functional_members` are subtracted"
  (matrix-rust-sdk `crates/matrix-sdk/src/room/mod.rs:4792-4815`). bridgev2 lists the bot as a functional
  member (`bridgev2/portal.go:5503-5507`), so the portal counts as a DM.
- For those intents Element Call sets `waitForCallPickup` (element-call 8be229a,
  `src/UrlConfiguration.ts:198-206`). In `src/state/CallViewModel/CallNotificationLifecycle.ts:116-160`,
  the ring "recipient" is `roomMemberUserIds$.value.find(id => id !== localUser)` (line 129; the code has
  `TODO: Respect io.element.functional_members`). The call counts as answered only when the recipient's
  user ID shows up in the MatrixRTC memberships (line 143). Otherwise `timeout` fires, then `autoLeave$`.
- `roomMemberUserIds` comes from `room.getMembersWithMembership(Join).concat(…Invite)`
  (`CallViewModel.ts:1600-1604`). matrix-js-sdk returns those in `Object.values(currentState.members)`
  order (`src/models/room.ts:1952-1956`, `src/models/room-state.ts:282-284`). That's insertion order:
  the order in which member events were first set (`room-state.ts:623-628`). In widget mode the initial
  state comes from the host's `readStateEvents` (`src/embedded.ts:370-380`). Element X answers from
  `room.get_state_events` (`crates/matrix-sdk/src/widget/matrix.rs:224-226`), which runs
  `SELECT … FROM state_event WHERE room_id=? AND event_type=?` with no `ORDER BY`
  (`crates/matrix-sdk-sqlite/src/state_store.rs:1006-1021`). The keys are encoded, so with Element X's
  encrypted store they're hashed state keys (PK `(room_id, event_type, state_key)`,
  `migrations/state_store/001_init.sql:25-33`). **So the picked recipient is arbitrary but stable for each
  device and room.** The bot comes before the ghost about half the time, and then that room fails on that
  device every time (as seen). With an unencrypted store, the plain-key order would be
  `@sip_1…` < `@sipbot` and it would work by luck.
- Sable doesn't wait for pickup. In a direct room it rings every joined member except itself, the bot
  included (`sable-next crates/sable-core/src/calls/mod.rs:528-557`), and it isn't affected. Inbound
  calls aren't affected anywhere: answering uses `join_existing_dm`, which has no `waitForCallPickup`.

**Options.**
- **(A) The bot is never in DM portals.** bridgev2 v0.32.0 always creates portal rooms as the bot
  (`createMatrixRoomInLoop`, `portal.go:5582` `portal.Bridge.Bot.CreateRoom`; the bot gets PL 9001,
  `:5464-5475`). There's no option to create them as the ghost. Doing it would mean replacing
  `CreateMatrixRoom` and the ghost-DM-invite path (`matrixinvite.go:200-221` invites the bot, joins it and
  gives it power). That's a fork of framework internals. Rejected.
- **(B) The bot leaves after the room is set up.** First it gives the ghost enough power to manage the
  room. Clients then see `leave` for the bot, which Element Call's Join+Invite list leaves out, so the
  recipient is the ghost. To clients the result is the same as (A), and only bridgev2's public API is
  needed. **Chosen.**
- **(C) The bot publishes its own `call.member`** so that "recipient joined" fires. That's a fake
  participant: Element Call shows an extra tile, `autoLeaveWhenOthersLeft` breaks, and the bot needs a
  LiveKit identity. Rejected.
- **(D) Fix Element Call upstream** (respect `io.element.functional_members` when picking the recipient,
  as the TODO says). That's the right long-term fix, and bridgev2 already publishes the state. But it
  depends on Element X shipping a new embedded Element Call, which is outside our control. We'll file it
  upstream; it isn't a fix we can ship now.

**Decision.** In every 1:1 number portal (Receiver set, E.164 portal ID, room type DM), the bridge bot:
1. makes sure the ghost is joined,
2. raises the ghost to **PL 50** if it's lower, and applies the `call.member` PL-0 overrides (one
   `m.room.power_levels` event, sent as the bot),
3. leaves the room.

This happens synchronously in `ResolveCallRoom` (before any ring), from a debounced `m.room.member`
watcher (10 s of quiet, covers `start-chat`/`pm`, provisioning and ghost-DM-invite rooms), and in a
one-off sweep at startup (migrates existing portals). Bot command replies in such rooms are sent by the
ghost, and the `dtmf` command redaction comes from the ghost (PL 50 ≥ `redact` 50). There's a config
switch, `network.bot_leaves_portals` (default `true`). Setting it to `false` turns this off, and the
startup sweep then has the ghost invite the bot back in (rollback).

**Why PL 50, not 100.** 50 is enough for everything the ghost does: redact others' events
(`dtmf` commands), revoke invites, kick, and **invite the bot back**. Once back, the bot has PL 9001
(the stale `users` entry stays when it leaves), or creator power in room v12 (Synapse's default
`DEFAULT_ROOM_VERSION = "12"`). It also keeps the ghost from showing as "Admin". Synapse's `invite`,
`kick` and `redact` defaults are 50 (`synapse/handlers/room.py:1706-1710`). The owner's own PL stays as
it is (0 in bot-created portals).

**Consequences / costs.** All of these are paths where bridgev2 acts as the bot. Without the bot in the
room they fail, with a log line only:
- bridgev2's built-in commands (`help`, unknown command, …) sent *inside* a portal get no reply. Our
  `call`/`hangup`/`dtmf` replies are sent as the ghost, and the management room is unchanged.
- Message-status events, error notices and delivery receipts (`bridgev2/matrix/connector.go:553-581`)
  aren't sent in portals. All three are off or unused here (`HandleMatrixMessage` sets `SendNotice(false)`).
- `m.bridge` / room-features resends (`Bridge.ResendBridgeInfo`, `bridge.go:259-290`), PL syncs as
  the bot, `Bot.DeleteRoom` on portal delete or logout cleanup (`matrix/intent.go:683` needs
  `joined_members`), and following a room upgrade tombstone all fail. The room keeps working for calls.
- Every number portal gets a "sipbot left" timeline event, plus one PL change on existing rooms.
- Embedded mode (ADR-006, mautrix-jabber) isn't covered. The text bridge needs its bot in the room, so
  that host either accepts the Element X limitation or waits for (D).

**Would revisit if:** Element Call respects functional members (then set `bot_leaves_portals: false`
and the bot can stay), or bridgev2 gets a supported "ghost-owned DM" mode.

---

## ADR-011: Phase J embedding: build-tagged integration on a private branch, shared host kit

**Status:** accepted (2026-10-17).

**Context:** ADR-006 option B; the user requires that no SIP content reaches the public mautrix-jabber
repo.

**Decision:** mautrix-jabber gains SIP calls only on the never-pushed local branch `local/sip-calls`, in
`//go:build sipcalls` files behind SIP-free seams, built with a local `replace` and a Docker named build
context; images are not pushed publicly. Host logic shared by both hosts moves to `pkg/sipaccount`
(core-class) and `pkg/hostkit` (host-class). Answers to the PLAN "before J3" items: group MMS and MUC
portals never get calls (`LookupCallRoom` accepts only `<E.164>@cheogram.com` 1:1 portals; inbound always
maps to the 1:1 JID); jabber portals are not forced unencrypted (startup warning, core refusal notice,
docs). SIP lifecycle follows the jabber login, not the XMPP session. JMI notices are suppressed for
Cheogram 1:1 numbers while the login has a SIP account. Telemetry: jabber's sipcalls build initialises
through this repo's `pkg/telemetry` with `ServiceName: mautrix-jabber`; `obs.AnnotateLogger` unifies log
annotation.

**Would revisit if:** mautrix-jabber's public repo ever accepts an optional SIP module, or the
replace-based build becomes painful (then: private module proxy).

Details: `docs/PHASEJ-SPEC.md`.

---

## ADR-012: Dialpad widget: OpenID + room-bound key, served on the AS listener

**Status:** accepted (2026-10-17). Implements ADR-008 option 1 for both hosts. Details:
`docs/WIDGET-SPEC.md`.

**Context:** A "Dialpad" room widget (`im.vector.modular.widgets`) in number portals and the management
room lets Sable, sable-next and gomuks users place calls, send DTMF and hang up without bot commands. The
widget API places PSTN calls, so it must act only for the owner of the room's login. Client research
(WIDGET-SPEC §1, source-checked at pinned commits) found that **sable-next, old Sable and gomuks hand a
Widget API OpenID token to any widget without asking the user** (`askOpenID` answers `Allowed` with no
prompt; matrix-widget-api does not gate `get_openid` behind a capability). Element Web prompts. Element
X does not render room widgets at all. An OpenID token therefore proves *who* the user is, but not that
the request comes from *our* widget: any widget the user opens, in any room, could obtain a token and
call our API.

**Decision:**

- **Authentication = OpenID + a room-bound key.** The page sends the Widget API OpenID token plus a
  per-room key `k = base64url(HMAC-SHA256(secret, "mrtcsip-dialpad-v1\0" + room_id))[:22]` that only
  our widget URL carries. `k` and the room id are in the URL **fragment** (never sent to a server or
  written to access logs); the state event that holds them is readable only by the room's members. The
  bridge checks `k` (constant time) first, then verifies the token with
  `/_matrix/federation/v1/openid/userinfo` on its own homeserver address (federation, with the mautrix
  federation client, only for other servers and only with `allow_remote_users`), then requires an
  existing bridge user with command permission and an eligible room (same rules as the bot commands).
- **Sessions** are short-lived (at most 1 h, at most the OpenID lifetime), in memory only (sha256 of the
  token), bound to one user and one room; the room always comes from the session, and room eligibility
  and the login are re-resolved on every request. No cookies (no CSRF surface), same-origin checks on
  POST, no CORS, strict bounded JSON, per-IP and per-user rate limits with a failure lockout.
- **Hosting on the existing appservice listener** under the path of `widget.public_url` (mounted on
  `AS.Router`, not `GetRouter()`, which needs `public_address`). One process, one port; the reverse proxy
  exposes only the prefix, never `/_matrix/app`. Polling, not SSE, so no long-lived handlers share the AS
  transaction server.
- **The bridge bot installs the widget** (state key `<appservice id>_dialpad`, so two bridges never
  collide) from a bounded background queue: startup sweep, 10-minute periodic sweep of unseen rooms,
  newly created portals and call commands. Content is compared before sending; no DB state.
- The page requests **no capabilities**, so sable-next opens it without a permission dialog.

**Consequences:** Anyone who can read the room's state (its members) can extract `k`, so `k` alone is
not an authorization; it only binds a valid OpenID identity to our widget and one room. A user's own
other widgets in the same room could read our URL only with a cross-room/state capability, which clients
prompt for. Element X users keep `call`/`dtmf`/`hangup`. Rotating `widget.secret` invalidates all widget
URLs; the next sweep re-sends every room's state event.

Residual key exposure (security review, accepted): anyone who can read the room's state holds the key.
That includes users who are invited or later joined, other homeservers' members through federation, and
past members whose clients kept the state. Mitigations: the installer (`Installer.Ensure`, sweeps) never
writes the widget into a room that has a joined or invited member besides the bot, this bridge's ghosts
and the owner (`widget.PrivateRoom`, bot state store only, no network I/O; refusal text
`widget.MsgNotPrivateRoom`, which hosts also return from `ResolveRoom`); the command `widget` is limited
to number portals and the user's own management room. A room that becomes shared later keeps its already
installed event: the key still needs a valid OpenID identity of the room's owner, so the exposure is to
that binding only, and the only way to revoke keys is rotating `widget.secret`. The management-room key
is the most powerful one (it is the owner's whole account), so keep that room private. Proxy trust is
explicit (`widget.trusted_proxies`; empty = private/loopback peers), IPv6 clients are rate-limited by
/64, and remote OpenID verification (`allow_remote_users`) resolves servers through an IP-filtering
dialer (no loopback, private, link-local or 100.64.0.0/10 targets, also for `.well-known`), without
redirects, and at most 256 distinct remote server names per hour.

**Would revisit if:** clients start gating OpenID behind a prompt or capability (then the key could be
dropped), Element X gains room widgets, or a standalone PWA with its own login replaces the widget.

---

## ADR-013: Dialpad PWA: one-time enrollment link, bridge-issued device tokens

**Status:** accepted (PWA-SPEC §14). Supersedes ADR-012's "would revisit if a standalone PWA ... replaces
the widget" note only partly: the widget stays; the PWA is an extra for clients without widgets
(Element X).

**Context:** Outside a Widget API host there is no OpenID.

**Decision:** `dialpad-link` (management room only, private room only) issues a 15-minute single-use code
in a fragment link; the PWA redeems it after user confirmation for a 256-bit device token (stored hashed
in own `dbutil` child tables). Every exchange for the existing 1 h in-memory session (management-room
scope, one login) rotates the token; the predecessor is superseded once the client confirms the successor
and stays valid for a 120 s grace window; a superseded token used after grace revokes the whole family
(stolen-copy detection). Expiry is by inactivity (90 days), with an optional absolute lifetime that is off
by default, so a device in use never has to be reinstalled. Revocation deletes the family and the live
sessions.

**Consequences:** A stolen copy works only until the real app's next rotation trips detection, or
indefinitely if the real app is never used again (until inactivity expiry or revoke); restoring an old
browser backup signs the device out; the origin of `public_url` holds a long-lived secret in
`localStorage`, so a dedicated hostname is recommended; the PWA does not carry audio (callback ring).

**Would revisit if:** Element X gains room widgets, a Matrix login inside the PWA becomes practical (then
the device token could be replaced by the user's own Matrix session plus OpenID), or Web Push for call
status is wanted.

---

## ADR-014: Dual-format MatrixRTC: read all, write in the owner's dialect

**Status:** accepted 2026-11-19 (MATRIXRTC2-SPEC §7), implemented, pending live verification (§9 there).
Amends the S1 "don't send sticky" guidance (S1 §1.6, ADR-002/ADR-003 still hold for legacy).

**Context:** MatrixRTC is moving from per-device `call.member` state events to MSC4143 slots and sticky
`m.rtc.member` events (MSC4354). The ecosystems disagree: matrix-js-sdk and Element Call use the unstable
names and slot `m.call#ROOM`; matrix-rust-rtc, lk-jwt-service >= 0.10 and the client that produced the
2026-11-19 logs (sable-next) use `m.rtc.member`, `m.rtc.slot` and `m.call#room`. A client only sees a
membership in its own dialect. LiveKit identities also differ by endpoint (`@ghost:DEV` for `/sfu/get`,
`hash([user, member.id])` for the MSC4195 homeserver `get_token`).

**Decision:**
- Read everything: both type names, both slot spellings, legacy and sticky (`matrixrtc.Membership`).
- Write in the owner's dialect. Owner-initiated calls mirror the owner's sticky membership (sticky only, same
  type name, slot spelling and transport url). Bridge-initiated calls (incoming SIP, `call`, widget) publish
  **dual** (legacy + sticky) in the remembered dialect, else the stable names, and open **both** slot
  spellings under **both** slot event types. Slots are never closed; a slot that declares encryption
  refuses owner-initiated calls (and explicit `sticky`/`dual`); bridge-initiated calls under `auto` drop
  the sticky part and ring on the legacy path.
- Until a client has shown the new format (no dialect known for the login) `auto` does not change anything
  an old client sees: the legacy membership content is unchanged, the ring keeps the legacy shape (slot
  `m.call#ROOM`, `m.relates_to`, not sticky), the token comes from `sfu_get` and the sticky membership
  names a `livekit_service_url` transport (identity `<ghost>:<DEV>`, like the legacy one). Explicit
  `dual`/`sticky` config and a known dialect get the union ring and MSC4195.
- `livekit_auth: auto` uses the MSC4195 homeserver `get_token` first and falls back to `/sfu/get` (the
  sticky join is re-sent with a `livekit_service_url` transport, same `member.id`).
- Sticky memberships carry an MSC4140 delayed leave (20 s, restarted every 7 s), a 1 h sticky duration and a
  40 min refresh.
- Without the sticky or delayed-event capability, or on `ErrUnsupported`, the bridge keeps the legacy path
  (sticky paused for the login for 10 min). `matrixrtc_format: legacy` restores the old behaviour.
- The dialect is kept in memory only. Delayed-leave cadence is fixed (20 s / 7 s), not configurable
  (lk-jwt `delegate_delayed_leave` needs SFU webhooks).
- With a known dialect (or explicit `sticky`/`dual`) the ring is the union of the MSC4075 shapes (sticky,
  `slot_id`, `notification_type` kept) and is withdrawn when it ends.
- Failures never cost the call more than the new format: an owner-mirror plan whose slot or sticky part fails
  carries on (legacy membership after a sticky failure); a failed MSC4195 attempt (token error or a stall
  before the connect, `get_token` bounded to 4 s) falls back to `sfu_get` without using up a join attempt
  and the login stays on `sfu_get` for 10 min.
- `core.GhostRTCIntent` is optional; hosts without it stay legacy. No new dependencies.

**Consequences:** More events per call (up to 4 slot state events, delayed leave, join, refresh). A ghost
with no dialect yet is visible to new-format clients only as the extra sticky membership; the first owner
sticky membership seen teaches the dialect (memory only, lost on restart). Needs Synapse sticky, delayed
and transports flags for the new format, plus MSC4195/appservice proxy for hashed identities; all of it
degrades to the old path. Fallbacks (`Unstable` names, `m.call#ROOM`, the `?org.matrix.msc4140.delay`
form) are to be deleted when the ecosystems converge. Behaviour is verified through telemetry after
deploy, not by manual checks first.

**Would revisit if:** a client cannot ring or join with the union ring or the dual membership, AS
masquerade for `get_token` or delayed events does not work on the deployment, or MSC4075 merges with a
different notification shape.
