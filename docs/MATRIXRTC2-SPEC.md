# MatrixRTC 2.0 spec: MSC4143 slots and MSC4354 sticky `m.rtc.member`, alongside legacy call.member

Status: implemented 2026-11-19 (pending live verification, §9). Items marked **UNVERIFIED** must pass WP0 before WP5 is turned on by default.
Supersedes the "don't send" guidance of `docs/spikes/S1-matrixrtc-wire-spec.md` §1.6. Adds ADR-014 (see WP6).
Rules from CLAUDE.md apply throughout:
- Event type names, field names and endpoint paths live only in `pkg/matrixrtc/events.go`, each citing the MSC and the client versions checked.
- Never log audio, DTMF digits, credentials, JWTs or OpenID tokens.
- LiveKit tokens come only through the MatrixRTC auth service as the ghost: either OpenID plus lk-jwt-service, or the homeserver C-S `get_token` that Synapse proxies to lk-jwt-service. Never minted from the LiveKit secret.
- Legacy `m.call.*` stays out of scope.

## 0. Sources checked (2026-11-19)

| Source | Version / commit | Licence | Read |
|---|---|---|---|
| MSC4143 MatrixRTC | merged, `proposals/4143-matrix-rtc.md` on main (PR head b7cfa8125d) | spec | slots, `m.rtc.member`, leave, lifecycle, transports discovery |
| MSC4354 Sticky events | merged 2026-11-18 (head 2df06b0e80) | spec | `sticky.duration_ms`, unstable prefixes, sticky_key map, AS = normal PDU delivery |
| MSC4140 Delayed events | merged 2026-11-13 (head 02a1abfbdf) | spec | |
| MSC4195 LiveKit transport | merged (hughns head 9259070646) | spec | `m.livekit` transport `{type,url,server_name?}`, identity `b64(sha256([sender, member.id]))`, endpoints, unstable prefixes |
| MSC4196 m.call | merged | spec | slot `m.call#room`, `application.intent` = `m.audio`/`m.video` |
| MSC4075 rtc.notification | open, head 4bdec0f963 (unchanged since S1) | spec | |
| matrix-js-sdk | develop 484d22d (2026-11-19); Element Call pins 6c8b1b4 | Apache-2.0 | `membershipData/rtc.ts`, `CallMembership.ts`, `MembershipManager.ts` (StickyEventMembershipManager), `MatrixRTCSession.ensureRtcSlotOpen`, `types.ts` parseCallNotificationContent, `CallIntent.ts`, `LivekitTransport.ts` |
| element-call | main 86ad9bc; release v0.28.2 | AGPL-3.0 (read only) | `src/widget.ts` (requests both `org.matrix.msc4143.rtc.member` and `m.rtc.member`), `sdk/src/RTCSlot.ts` (default `m.call#ROOM`), `sdk/src/config.ts` (Compatibility vs Matrix_2_0) |
| element-hq/matrix-rust-rtc | main 52af3d1 (2026-11-13) | AGPL-3.0 (read only) | `core/src/wire.rs`, `slot.rs`, `join.rs`, `compat/mod.rs`, `compat/element_call.rs`, `host/event.rs`, `matrix-rtc-call/src/notification.rs`, `transport/src/livekit/identity.rs` (Element X native call stack) |
| matrix-rust-sdk | main b21ed96 | Apache-2.0 | sticky map (`sticky/extract.rs`), `room/call.rs` (active-call indicator is still **legacy state only**), RTC transports cache |
| ruma | main 44fec4f | MIT | `ruma-client-api/src/rtc/{transports,livekit/get_token}.rs` |
| lk-jwt-service | main bc31e4c, release v0.10.0 (2026-11-12) | AGPL-3.0 (read only) | `src/handler.rs` (`process_legacy_sfu_request`, `process_sfu_request`, `process_get_token_cs_request`), `src/helper.rs`, `src/requests.rs`, README (MSC4502/MSC4512 appservice proxy) |
| Synapse | develop 1b0fd0a, release v1.165.0 | AGPL-3.0 (read only) | `rest/client/appservice_proxy.py`, `rest/client/room.py` (sticky + delayed send), `handlers/delayed_events.py`, `rest/client/delayed_events.py`, `rust/src/handlers/versions.rs` |
| mautrix-go | v0.33.0 (our pin) | MPL-2.0 | `event.Event.Sticky` (`msc4354_sticky`), `Unsigned.StickyDurationTTL` (`msc4354_sticky_duration_ttl_ms`), `ReqSendEvent.UnstableDelay/UnstableStickyDuration`, `RTCTransports`, `RTCLivekitGetToken`, `UpdateDelayedEvent` |

URL pattern: `https://github.com/<owner>/<repo>/blob/<commit>/<path>`.

## 1. Wire formats (verified)

### 1.1 `m.rtc.member` (sticky message event)
- Type names:
  - `org.matrix.msc4143.rtc.member` is what js-sdk 484d22d sends; Element Call's widget can send both names.
  - `m.rtc.member` is stable, MSC4143. matrix-rust-rtc speaks it internally, and the 2026-11-19 production client sent it.
  - Readers must accept both. rust-rtc accepts both on input.
- Sent with `PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}?org.matrix.msc4354.sticky_duration_ms=3600000`.
  - The stable query parameter would be `sticky_duration_ms`; Synapse only parses the unstable one.
  - The PDU gains a top-level `msc4354_sticky: {duration_ms}`; mautrix parses it as `evt.Sticky`.
  - Clients see `unsigned.msc4354_sticky_duration_ttl_ms`.
  - The maximum effective duration is 1 h (MSC4354; `event.MaxStickyDuration`).
- Join content (MSC4143 plus what js-sdk validates in `checkRtcMembershipData`):
```json
{"slot_id":"m.call#room",
 "member":{"user_id":"@ghost:example.org","device_id":"SIPAB12CD34","id":"<uuid>","membership":"join"},
 "application":{"type":"m.call","intent":"m.audio","m.call.intent":"audio"},
 "transports":{"published":[{"type":"livekit","url":"wss://sfu.example.org"},{"type":"m.livekit","url":"wss://sfu.example.org"}],
               "can_subscribe":["livekit","m.livekit"]},
 "sticky_key":"<uuid>","msc4354_sticky_key":"<uuid>"}
```
  - js-sdk requires `member.user_id == sender`, string `device_id`, `transports.published[].type` and `can_subscribe` strings, and one sticky key (both must be equal if both are present). `slot_id` must start with `application.type + "#"`.
  - The MSC requires `sticky_key == member.id`. rust-rtc honours `sticky_key` when they differ.
  - Element Call's 2025 dialect used a flat `rtc_transports` array. rust-rtc lifts it into `transports.published`; we do the same when reading.
- Leave content (MSC4143):
  - `{"slot_id":..., "member":{"id":"<uuid>","membership":"leave"}, "leave_reason":{"code":"leave"}, "sticky_key":"<uuid>", "msc4354_sticky_key":"<uuid>"}`.
  - Older leaves are `{slot_id, msc4354_sticky_key}` or `{msc4354_sticky_key}` alone (a "tombstone"). Treat them all as leave.
  - Leave codes: `leave`, `delayed_leave`, `slot_closed`.
- A member is joined only if all of these hold:
  - `membership` is absent or `join`;
  - the slot exists and is open with the same `application.type`;
  - the sender is joined to the room;
  - the event is still sticky.
  - A redacted event loses stickiness and counts as left.
- Lifecycle (MSC4143, js-sdk):
  - The client schedules a delayed leave with the MSC4140 delay (js-sdk default 8 s, restarted every 5 s; the production client used 20000). It then sends the sticky join and refreshes it before 1 h.
  - Production order: slot GET/PUT, delayed leave PUT, join PUT, transports GET, `get_token`, then the sticky notification.

### 1.2 `m.rtc.slot` (state event, state key = slot id)
- Type names: `org.matrix.msc4143.rtc.slot` (js-sdk) and `m.rtc.slot` (MSC, rust-rtc, production). Both already have PL 0 (`CallPowerLevelTypes`).
- Content: `{"status":"open"|"closed","application":{"type":"m.call"},"encryption":{"type":"m.per_member"}?}`.
  - In an unencrypted room the slot MUST NOT carry `encryption` (rust-rtc `slot.rs`).
  - js-sdk `ensureRtcSlotOpen` uses `org.matrix.msc4143.per_member`.
  - Slots never close by themselves; the MSC recommends creating them at room creation.
- Room slot id:
  - The MSC4143/4196 stable value is **`m.call#room`**: rust-rtc, lk-jwt `M_CALL_ROOM_SLOT_ID`, and production.
  - js-sdk 484d22d and Element Call still use **`m.call#ROOM`** (`MatrixRTCSessionManager` default `{application:"m.call", id:"ROOM"}`). Legacy memberships map `call_id ""` to `ROOM`.
  - rust-rtc rejects `m.call#ROOM` in MSC mode and lifts it to `room` only in its room-state compatibility mode.
  - lk-jwt v0.10.0 maps `ROOM` to `room` before hashing, so the LiveKit room is the same on every token endpoint, **including legacy `/sfu/get`**. `rtc.roomSlotID = "m.call#ROOM"` in `pkg/rtc/discover.go` is therefore out of date (it is diagnostics only).

### 1.3 Notification / invite (`org.matrix.msc4075.rtc.notification`, message event, now sticky)
- The production client sent it sticky: `?org.matrix.msc4354.sticky_duration_ms=300000`.
- rust-rtc (MSC format, its "m.rtc.invite" on the wire as `org.matrix.msc4075.rtc.notification`) sends exactly `{slot_id, sender_ts, lifetime, "m.mentions":{user_ids,room}, msc4354_sticky_key: slot_id}`:
  - **no `notification_type` and no `m.relates_to`**;
  - lifetime defaults to 90 s, maximum 120 s;
  - sticky duration is 2 × lifetime, at least 5 min (MSC4354 SHOULD);
  - withdrawal is `{msc4354_sticky_key: slot_id}`.
- js-sdk `parseCallNotificationContent` (484d22d) accepts a notification only if all of these hold:
  - it is not a withdrawal;
  - `slot_id` is given, or derived from an `m.reference` to a legacy membership;
  - `msc4354_sticky_key` equals `slot_id` when present;
  - when sticky, it is the current keyed sticky event for (sender, type, slot);
  - **the slot exists and is open**;
  - `sender_ts` and `lifetime` are present (lifetime capped at 2 min), and `m.mentions` targets the user;
  - the user is not already joined, and no later leave or decline exists.
- Element X Android still requires `notification_type:"ring"` (S1 §2.3, unchanged).
- So the ghost's ring is the **union** of both shapes (WP1 `RingContent`).

### 1.4 Transports discovery
- `GET /_matrix/client/v1/rtc/transports` returns `{"transports":[...]}` (MSC4143, ruma v1; Synapse flag `org.matrix.msc4143.stable`).
- `GET /_matrix/client/unstable/org.matrix.msc4143/rtc/transports` returns `{"rtc_transports":[...]}` (flag `org.matrix.msc4143`).
- Entries: `{type:"m.livekit"|"livekit", url?, livekit_service_url?, server_name?}`.
- Fallback: `.well-known` `org.matrix.msc4143.rtc_foci` (existing `ParseRTCFoci`).
- mautrix `Client.RTCTransports` reads `rtc_transports` for both paths, which is wrong for stable. **Don't use it.**
- Both endpoints are authenticated. Call them as the ghost (or the bot).

### 1.5 LiveKit tokens (lk-jwt-service v0.10.0)
| Endpoint | Auth | LiveKit identity | Room alias |
|---|---|---|---|
| `POST {svc}/sfu/get` `{room, openid_token, device_id}` (current code) | OpenID as ghost | `@ghost:srv:DEV` | `b64(sha256(["!room","m.call#room"]))` (v0.10; v0.9 used `#ROOM`) |
| `POST {svc}/get_token` `{room_id, slot_id, openid_token, member:{id, claimed_user_id, claimed_device_id}}` | OpenID | `b64(sha256([user, device, member.id]))` (3-tuple; legacy) | same, slot mapped to stable |
| homeserver `POST /_matrix/client/v1/rtc/livekit/get_token` or `/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token` `{url, room_id, slot_id, member_id, server_name?}` → `{jwt}` | the user's access token. Synapse MSC4512 proxies the request to lk-jwt-service with `X-Matrix-User-Identifier: <requester>` (`appservice_proxy.py` uses `get_user_by_req`, which honours AS `?user_id=` masquerade). **UNVERIFIED end to end for a ghost.** | `b64(sha256([user, member_id]))` (MSC4195 §identity). Clients compute the same (js-sdk `computeRtcIdentityRaw`) | `livekit_room_alias_for(room_id, map_to_stable(slot_id))` |

- On the C-S endpoint `url` must equal lk-jwt's `LIVEKIT_URL` byte for byte (`require_matching_lk_url`), and the user must be joined (MSC4502 `is_joined`).
- The response carries only `jwt`; the LiveKit URL is the request `url`.
- Production log: `generated SFU access token (app-service) ... matrix_rtc_slot=m.call#room lk_id=<hash>`.
- The JWT carries no metadata or attributes (`get_join_token`). Owner identity must be derived from the owner's membership.
- Identities that clients accept for a membership (js-sdk `CallMembership.parseFromEvent`; rust-rtc "identity follows the published transport"):
  - a `url` transport → `hash([sender, memberId])`, where memberId is `member.id` (sticky) or `membershipID ?? "<sender>:<device>"` (legacy);
  - a `livekit_service_url` transport → `"<sender>:<device>"`.

### 1.6 Delayed events (MSC4140, Synapse 1b0fd0a)
- Preferred: `PUT /_matrix/client/unstable/org.matrix.msc4140/rooms/{room}/delayed_event/{type}/{txn}` with body `{"delay_ms":20000,"content":{...},"org.matrix.msc4354.sticky_duration_ms":3600000}` → `{"delay_id"}`.
- Deprecated, with Synapse's own note "remove by 2026-12-31": `?org.matrix.msc4140.delay=` on `/send` (mautrix `ReqSendEvent.UnstableDelay`).
- Management: `POST /_matrix/client/unstable/org.matrix.msc4140/delayed_events/{id}/restart|cancel|send` with `{}`. The legacy form is `POST .../delayed_events/{id}` with `{"action":...}` (mautrix `UpdateDelayedEvent`).
- The owner is the requester. `requester.device_id` may be null for AS masquerade; no AS restriction was found in `handlers/delayed_events.py` (**UNVERIFIED** live).
- Limits come from `max_event_delay_duration` and `max_delayed_events_per_user`.

### 1.7 Feature flags (`GET /_matrix/client/versions` `unstable_features`, Synapse rust `versions.rs`)
`org.matrix.msc4354` (sticky), `org.matrix.msc4140` (delayed), `org.matrix.msc4143` / `org.matrix.msc4143.stable` (transports), `org.matrix.msc4195.stable`.

### 1.8 Appservice delivery
- MSC4354 gives no special appservice rule. Sticky events are ordinary PDUs and reach the AS transaction like any timeline event in rooms where the AS has users.
- bridgev2 dispatch matches type **and** class, so register both member names as `MessageEventType` and both slot names as `StateEventType` (core `RegisterMatrixRTCHandlers` loops over `IngestEventTypes`).
- Whether Synapse includes `msc4354_sticky` / `unsigned.msc4354_sticky_duration_ttl_ms` in AS pushes is **UNVERIFIED**. Fall back to a 1 h sticky duration from `origin_server_ts`.

## 2. Format selection (decision table)

Definitions:
- **Formats:** L = legacy `org.matrix.msc3401.call.member` state (as today); S = sticky rtc.member; D = dual (L+S).
- **Dialect** = (type name, slot-id spelling, transport kind) as last seen from the owner's own sticky membership for this login, kept in memory.
- **Caps** = cached `/versions` result.
- **Auth:** M = MSC4195 C-S `get_token` (hashed identity), O = OpenID `/sfu/get` (`@ghost:DEV`).

| Trigger | Owner evidence | Server caps | Ghost membership | Ring | Token/identity | Slot |
|---|---|---|---|---|---|---|
| Owner-initiated (ring mode) | owner membership is S | sticky ok | **S only, mirroring the owner** (same type name, slot spelling and `url` from the owner's `transports.published`). Never fails the call: a slot problem is logged and skipped, a sticky failure drops to L | none (ghost is callee) | M with the owner's url (never `/rtc/transports`). On M failure (token error or a stall before the connect), O with a `livekit_service_url` transport (S content rebuilt) | ensure open (owner normally opened it) |
| Owner-initiated | owner membership is L | any | L (unchanged) | none | O (unchanged) | untouched |
| Bridge-initiated (inbound SIP, `!sip call`, widget) | dialect known | sticky ok | **D** with S in the remembered dialect | union ring, sticky, slot_id = dialect spelling | M if `/rtc/transports` gives a `url`, M did not fail for this login in the last 10 min and M works, else O | open both spellings under both slot types (4 state events; a mirror plan: the owner's type and spelling only) |
| Bridge-initiated, `auto` | no dialect yet | sticky ok | D with S in stable names (`m.rtc.member`, `m.call#room`); the S transport is `livekit_service_url`; L content unchanged | **legacy ring**: slot `m.call#ROOM`, `m.relates_to`, not sticky, nothing to withdraw | O (`/sfu/get`, no `/rtc/transports` lookup) | open both spellings under both slot types |
| Any | (any) | no sticky flag, or a sticky send returns 400/M_UNRECOGNIZED | L only (as today) | legacy ring, not sticky | O | untouched |
| Config `matrixrtc_format: legacy` | ignored | ignored | L | legacy | O | untouched |
| Config `sticky` | ignored | sticky ok | S (dialect, else stable) | union | M, else O | open |
| Existing slot declares `encryption` | | | **refuse**: same path as an encrypted room (`noticeEncrypted`), Warn log | | | never rewritten |

In D with auth M, the L content gets `membershipID = member.id` and `foci_preferred[0].url = <url>`, so js-sdk accepts the ghost's single hashed identity for both memberships. In D with auth O, the S transport is `{type:"livekit", livekit_service_url}`, so every client expects `@ghost:DEV`.

## 3. Work packages

### WP0: live verification (manual, in parallel; gates WP5's default)
On the deployment (Synapse msc4354/4140/4143/4502/4512, lk-jwt in appservice mode):
1. As a ghost via AS masquerade: `POST /_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token?user_id=@ghost` → 200 with a JWT whose `sub` = `hash([ghost, member_id])`. Repeat on the v1 path.
2. Delayed sticky send as a ghost (new body endpoint and old query parameter), then restart and send.
3. Capture the AS transaction JSON of an owner `m.rtc.member`: are `msc4354_sticky` and unsigned TTL present?
4. Record the exact slot content the production client wrote to `m.call#room`. Encryption present?
5. With a hand-crafted union ring (WP1 `RingContent` with sticky), confirm Element X Android/iOS and Element Web still ring.
6. Note client name and version of the "Element" used (likely Element X native via matrix-rust-rtc).
Record the results in this doc §9.

### WP1: `pkg/matrixrtc` contracts (first; blocks all others)
Files: `pkg/matrixrtc/events.go`, `parse.go`, `membership.go`, new `sticky.go`, new `identity.go`, tests.

**`events.go`.** Add the following. The header comment cites §0 versions and the date.
- Types:
```go
RTCMember      = event.Type{Type: "org.matrix.msc4143.rtc.member", Class: event.MessageEventType} // js-sdk name
RTCMemberAlias = event.Type{Type: "m.rtc.member", Class: event.MessageEventType}                   // MSC4143 stable
```
- `IngestEventTypes()` appends `RTCMember, RTCMemberAlias, RTCSlot, RTCSlotAlias`. Order stays fixed; update tests.
- Slot ids. Keep the old constant's value under a new name; do not silently change `RingContent`:
```go
SlotIDRoom = "m.call#room"; SlotIDRoomLegacy = "m.call#ROOM"
```
- Field and value constants:
  - `FieldMember="member"`, `FieldUserID="user_id"`, `FieldID="id"`, `FieldMembership="membership"`, `FieldTransports="transports"`, `FieldPublished="published"`, `FieldCanSubscribe="can_subscribe"`;
  - `FieldStickyKey="sticky_key"`, `FieldStickyKeyUnstable="msc4354_sticky_key"`;
  - `FieldLeaveReason="leave_reason"`, `FieldCode="code"`, `FieldStatus="status"`, `FieldEncryption="encryption"`, `FieldIntent="intent"`, `FieldURL="url"`, `FieldServerName="server_name"`;
  - `FieldRTCTransportsFlat="rtc_transports"`, `FieldTransportsResp="transports"`;
  - values `MembershipJoin="join"`, `MembershipLeave="leave"`, `SlotStatusOpen="open"`, `SlotStatusClosed="closed"`, `LeaveCodeLeave="leave"`, `FocusTypeLiveKitStable="m.livekit"`, `IntentAudioStable="m.audio"`.
- Endpoints and parameters:
  - `PathRTCTransports="/_matrix/client/v1/rtc/transports"`, `PathRTCTransportsUnstable="/_matrix/client/unstable/org.matrix.msc4143/rtc/transports"`;
  - `PathLiveKitGetToken="/_matrix/client/v1/rtc/livekit/get_token"`, `PathLiveKitGetTokenUnstable="/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"`;
  - `QueryStickyDuration="org.matrix.msc4354.sticky_duration_ms"`, `BodyStickyDuration` (same string), `QueryDelay="org.matrix.msc4140.delay"`;
  - `PathDelayedEventPrefix="/_matrix/client/unstable/org.matrix.msc4140"` (the hostkit builder appends `rooms/{room}/delayed_event/{type}/{txn}` and `delayed_events/{id}/{action}`).
- Feature flags: `FeatureSticky="org.matrix.msc4354"`, `FeatureDelayed="org.matrix.msc4140"`, `FeatureRTC="org.matrix.msc4143"`, `FeatureRTCStable="org.matrix.msc4143.stable"`, `FeatureLiveKitStable="org.matrix.msc4195.stable"`.
- Durations: `MemberStickyDuration=time.Hour`, `MemberStickyRefresh=40*time.Minute`, `DelayedLeaveDelay=20*time.Second`, `DelayedLeaveRestart=7*time.Second`, `NotificationStickyMin=5*time.Minute`.
- Helpers:
  - `IsRoomSlot(s string) bool` (either spelling);
  - `IsMemberType(t event.Type) bool`;
  - `IsSlotType`.

**`parse.go`.**
- Add `KindRTCMember` and `KindRTCSlot` (String: `rtc_member`, `rtc_slot`; low-cardinality metric values).
- `KindOf` maps member types to `KindRTCMember` and slot types to `KindRTCSlot`.
- `Event` gains:
```go
StickyDuration  time.Duration // evt.Sticky.GetDuration() (clamped 1 h); 0 = not sticky or unknown
StickyRemaining time.Duration // evt.Unsigned.StickyDurationTTL; 0 = absent
Redacted        bool          // evt.Unsigned.RedactedBecause != nil
```
- `FromMatrixEvent` fills these fields.

**`membership.go`.** Redefine the unified type. This is a breaking change for internal callers, which are updated in WP4.
```go
type Format int // FormatLegacy=1, FormatSticky=2
type Transport struct{ Type, URL, ServiceURL, Alias, ServerName string }
type Membership struct {
    Format     Format
    Key        string     // device key: legacy = state key; sticky = "sticky\x00" + sticky key
    UserID     id.UserID
    DeviceID   string     // "" on sticky leave
    MemberID   string     // sticky member.id; legacy membershipID ?? "<user>:<device>"
    SlotID     string     // as received; legacy -> SlotIDRoomLegacy
    TypeName   string     // evt.Type as received (dialect)
    Live       bool
    Created    time.Time
    Expires    time.Time  // legacy created+expires; sticky Timestamp+min(StickyDuration||1h,1h); zero if not live
    Transports []Transport
}
func ParseMembership(evt Event, now time.Time) (Membership, bool) // dispatch on Kind
func (m Membership) BackendIdentities() []string                    // §1.5 rules
func (m Membership) PreferredLiveKitURL() string                     // first Transport with URL ("m.livekit" before "livekit")
```
- Sticky parse rules:
  - The sticky key is `sticky_key ?? msc4354_sticky_key`. If it is missing → ok=false. If both are present and differ → ok=false.
  - Tombstone (only sticky keys), `membership=="leave"`, or `Redacted` → Live=false.
  - Live also requires:
    - `member.user_id == Sender` (absent = sender);
    - `application.type == "m.call"`;
    - `IsRoomSlot(slot_id)`;
    - membership absent or `join`;
    - now < Expires;
    - `Sender` is the parsed user.
  - Lift a flat `rtc_transports` into `published`.
  - Never fail on unknown fields.
- Legacy parse: keep the current rules. Fill Key=StateKey, MemberID, Transports from `foci_preferred` (type, `livekit_service_url`, `url`, `livekit_alias`), and Expires.

**`sticky.go`.** Builders:
```go
type StickyJoinParams struct{ UserID id.UserID; DeviceID, MemberID, SlotID string; Transports []Transport }
func StickyJoinContent(p StickyJoinParams) map[string]any   // §1.1 exactly; both sticky keys; intent m.audio + m.call.intent audio
func StickyLeaveContent(slotID, memberID string) map[string]any
func SlotOpenContent() map[string]any                        // {"status":"open","application":{"type":"m.call"}} (no encryption)
type Slot struct{ ID string; Open bool; Encrypted bool }
func ParseSlot(evt Event) (Slot, bool)                       // KindRTCSlot; open = status open && application.type m.call
func ParseTransports(body []byte) ([]Transport, bool)        // accepts "transports" and "rtc_transports"
func NewMemberID() string                                     // crypto/rand UUIDv4 text, no new dependency
```
- Extend `JoinParams` with `MemberID string` (empty → `"<user>:<device>"`, unchanged) and `LiveKitURL string` (non-empty → `foci_preferred[0].url`).
- Extend `RingParams` with:
  - `SlotID string` (empty → `SlotIDRoomLegacy`; today's wire value is kept);
  - `Sticky bool` (adds `msc4354_sticky_key: SlotID`);
  - `MembershipEventID` may be empty (then `m.relates_to` is omitted).
- Add `RingWithdrawContent(slotID string) map[string]any` and `NotificationStickyDuration(lifetime time.Duration) time.Duration` = max(2×lifetime, 5 min).
- `ParseNotification` adds `SlotID` and `Withdrawn bool` (content holds only sticky keys).

**`identity.go`.** Move `hashJSON` here from `pkg/rtc` (pure std lib):
- `LiveKitIdentity(user id.UserID, memberID string) string`
- `LegacyLiveKitIdentity(user id.UserID, deviceID string) string`
- `LiveKitRoomAlias(room id.RoomID, slotID string) string` (maps `ROOM` → `room` like lk-jwt `map_to_stable_m_call_slot_id`)

Tests:
- golden contents;
- the MSC4195 vector (`["!roomid:example.com","slot1234"]` → `O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc`);
- an identity vector computed with Python `json.dumps(separators=(',',':'))`;
- parse tables: stable and unstable names, both slot spellings, tombstone, leave with reason, mismatched sticky keys, user_id ≠ sender, expired by duration, flat `rtc_transports`, missing optional fields;
- `KindOf` class mismatch → unknown;
- the `IngestEventTypes` order.

### WP2: core and hostkit ghost RTC capability (after WP1)
Files: `pkg/core/callhost.go`, `pkg/core/ingest.go` (+tests), `pkg/hostkit/ghostintent.go` (+new `ghostintent_rtc_test.go`), `pkg/hostkit/fakes_test.go`.

New optional interface in core. Type-asserted by callers; hosts without it fall back to legacy.
```go
type DelayedEventID string
type GhostRTCIntent interface {
    // SendSticky sends a message event with sticky duration d (QueryStickyDuration). Returns the event id.
    SendSticky(ctx context.Context, roomID id.RoomID, t event.Type, content map[string]any, d time.Duration) (id.EventID, error)
    // ScheduleDelayed schedules content after delay; sticky>0 makes it a sticky delayed event. New body
    // endpoint first, falls back to the ?org.matrix.msc4140.delay query form on 404/M_UNRECOGNIZED.
    ScheduleDelayed(ctx context.Context, roomID id.RoomID, t event.Type, content map[string]any, delay, sticky time.Duration) (DelayedEventID, error)
    // UpdateDelayed: action "restart" | "cancel" | "send" (path form, then the legacy {action} body form).
    UpdateDelayed(ctx context.Context, did DelayedEventID, action string) error
    // StateEvent returns current state content, (nil, nil) on 404.
    StateEvent(ctx context.Context, roomID id.RoomID, t event.Type, stateKey string) (map[string]any, error)
    // RTCTransports: stable path then unstable path; raw body (parsed by matrixrtc.ParseTransports).
    RTCTransports(ctx context.Context) ([]byte, error)
    // LiveKitToken: homeserver C-S get_token (stable then unstable path) as the ghost. Returns the JWT only.
    LiveKitToken(ctx context.Context, req LiveKitTokenRequest) (string, error)
    // Capabilities: cached /versions unstable_features (10 min; errors -> empty).
    Capabilities(ctx context.Context) map[string]bool
}
type LiveKitTokenRequest struct{ URL string; RoomID id.RoomID; SlotID, MemberID string }
var ErrUnsupported = errors.New("core: homeserver does not support this MatrixRTC feature") // 404/M_UNRECOGNIZED/400 unknown param
```

hostkit `ghostIntent` implements it with `g.raw` (`*appservice.IntentAPI`, whose embedded client adds `user_id` masquerade):
- Build URLs with `raw.BuildURLWithQuery` / `MakeRequest`, using path constants from `matrixrtc`.
- Do **not** use mautrix `RTCTransports`/`RTCLivekitGetToken`: wrong key for stable, and `SpecVersions` dependence.
- `SendSticky` may use `raw.SendMessageEvent(..., mautrix.ReqSendEvent{UnstableStickyDuration: d})`.
- Map HTTP 404, `M_UNRECOGNIZED`, and a 400 naming the sticky or delay parameter to `ErrUnsupported`.
- Errors are scrubbed. Never log JWTs or URLs with query strings.
- `Capabilities` lives on a shared per-bridge cache (one `/versions` per 10 min), not per ghost.

`core/ingest.go`:
- New kinds flow through unchanged.
- The `matrixrtc.ingest` span adds `matrixrtc.membership.format` (legacy|sticky) for member kinds and `matrixrtc.slot_id`. The `device_id` attribute comes from the parsed membership.

Tests:
- an httptest homeserver for every path and fallback;
- `user_id` masquerade present;
- the token never appears in errors;
- the `ErrUnsupported` mapping;
- `RegisterMatrixRTCHandlers` registers 10 types with the right classes.

### WP3: `pkg/rtc` transport discovery, MSC4195 auth, owner matching (after WP1; uses the WP2 interface)
Files: `pkg/rtc/rtc.go`, `token.go`, `livekit.go`, `discover.go`, new `transports.go`, tests (`token_test.go`, `discover_test.go`, `identity_test.go`, `session_test.go`).

`JoinParams` additions:
```go
Auth       AuthMode   // AuthSFUGet (today) | AuthMSC4195
SlotID     string     // MSC4195; canonical spelling sent as received
MemberID   string     // MSC4195 member_id = ghost's member.id
LiveKitURL string     // MSC4195 transport url (also the connect URL)
Owners     []string   // initial accepted owner identities (exact)
```
- `Ghost` stays `core.GhostIntent`. AuthMSC4195 type-asserts `core.GhostRTCIntent`; if that fails → `JoinError{Stage:"token_error"}`.
- Flow for AuthMSC4195:
  - skip the OpenID step (span `rtc.openid_token` is not created);
  - `rtc.jwt_fetch` gets attribute `rtc.token.endpoint=get_token_cs`; the AuthSFUGet path sets `sfu_get`;
  - connect to `LiveKitURL`.
- The identity check compares with `matrixrtc.LiveKitIdentity(ghost, MemberID)` for MSC4195, else legacy (today's Warn).

`Session` gains:
```go
AddOwnerIdentities(ids ...string) // thread-safe; rescans remote participants, attaches already-subscribed audio, emits EventOwnerJoined
```
- `isOwner` becomes a matcher: exact set ∪ legacy prefix `"<owner>:"`.
- Subscribed audio tracks of not-yet-matched participants are held in a small map (no audio is read or forwarded until matched) and promoted on `AddOwnerIdentities`.
- No I/O in LiveKit callbacks.

`transports.go`: `TransportResolver` caches the result of `GhostRTCIntent.RTCTransports` like `ServiceURLResolver`:
- success cached for 1 h, failure for 60 s;
- a single in-flight fetch;
- `Get(ctx, g core.GhostRTCIntent) ([]matrixrtc.Transport, error)`.
- `ServiceURLResolver` stays for `livekit_service_url`. Selection helper: `SelectLiveKit(ts []Transport) (url, serviceURL string)`.
- `LiveKitRoomName` uses `matrixrtc.LiveKitRoomAlias(room, SlotIDRoom)` (stable), so it matches lk-jwt ≥ 0.10. Note in the doc comment: ≤ 0.9 used `#ROOM`.
- `PathTemplate` adds `"/rtc/livekit/get_token"` and `"/rtc/transports"`.

Tests:
- `get_token` happy path and errors (JWT never in the error string);
- owner matched by hash added after join, with the held track promoted;
- legacy prefix still works;
- non-owner tracks never read;
- room alias vector for both spellings.

### WP4: `pkg/calls` ingest side: detection, owner devices, dialect memory (after WP1)
Files: `pkg/calls/manager.go`, `detect.go`, `owner.go`, `outbound.go` (`onOutMembership`), `state.go`, `fakes_test.go`, `detect_test.go`, `call_test.go`, `outbound_test.go`, new `dialect.go`.

- `HandleRTCEvent`:
  - `KindRTCMember` → `evOwnerMembership` (same as call member).
  - `KindRTCSlot` → dropped (never routed or triggered; the ghost reads slots itself in `ensureSlots`).
  - `KindRTCNotification` with `Withdrawn` → drop any `pendingNotif` for that room, no trigger.
- Every place that keys owner devices by `ev.rtc.StateKey` switches to `mem.Key`: `detect.go` `updateOwnerLiveLocked`, `c.devices` in `owner.go`/`outbound.go`/`triggerLocked`.
- `memberSeen` gains `mem matrixrtc.Membership`, so the identities, url and dialect of live owner devices are known at trigger time.
- Dialect memory:
  - `type Dialect struct{ TypeName string; SlotID string; URL string; At time.Time }`;
  - `Manager.dialect map[core.LoginRef]Dialect`, set from every live owner **sticky** membership;
  - `func (m *Manager) dialectFor(login) (Dialect, bool)`.
- Ring-mode trigger: unchanged pairing (notification ± fresh membership within the windows). A sticky membership's `Created` = event ts satisfies freshness. A new-format notification has no `m.relates_to` or `notification_type`; `ParseNotification` already tolerates that.
- The triggered call records `c.ownerFormat` and `c.ownerMem` (the freshest live owner membership in the room) for WP5.
- On every owner live membership routed to a call, push `mem.BackendIdentities()` to `c.session.AddOwnerIdentities` (actor goroutine, non-blocking).
- Tests (fakes extended with sticky events):
  - production replay (slot event, owner `m.rtc.member` stable, sticky notification without `notification_type`) → outbound trigger;
  - reverse order (notification first);
  - tombstone leave removes the device;
  - leave with `leave_reason`;
  - mixed legacy and sticky devices of the owner;
  - withdrawal cancels pending;
  - the ghost's own echo is ignored (resolver drops ghost senders);
  - stale sticky (expired) is not live.

### WP5: `pkg/calls` ghost publishing and lifecycle (after WP2, WP3, WP4)
Files: `pkg/calls/setup.go`, `outbound.go`, `teardown.go`, `call.go`, `manager.go` (Config), new `rtcmember.go`, new `format.go`, tests.

`Config` additions:
```go
MembershipFormat string // "auto" (default) | "legacy" | "sticky" | "dual"
LiveKitAuth      string // "auto" (default) | "sfu_get" | "msc4195"
Transports       func(ctx context.Context, g core.GhostRTCIntent) ([]matrixrtc.Transport, error) // rtc.TransportResolver.Get
```

`format.go`, `func (c *call) choosePlan(ctx) plan`, implements the §2 table:
```go
type plan struct{ legacy, sticky bool; typ event.Type; slot string; auth rtc.AuthMode; url, serviceURL string; ringSticky bool }
```
- Inputs: trigger, `c.ownerFormat`/`c.ownerMem`, `dialectFor`, `GhostRTCIntent` presence, `Capabilities()[FeatureSticky]`, `Capabilities()[FeatureDelayed]`, config, and the slot state.
- An unknown slot is read via `StateEvent` for both spellings. An encrypted slot → refuse.
- Pure apart from those inputs; table-driven unit test per row.

`rtcmember.go`, the ghost sticky membership manager. It is owned by the call actor; I/O runs in one helper goroutine per call:
- `c.memberID = matrixrtc.NewMemberID()` per join (never reused after leave).
- `ensureSlots(ctx)`:
  - For each slot spelling the plan needs (bridge-initiated: both; owner-initiated: the owner's), `StateEvent`.
  - If missing or closed → `SendState(SlotOpenContent())`.
  - Child span `matrixrtc.slot.open` (attrs `matrixrtc.slot_id`, `matrixrtc.slot.action` = noop|opened|refused_encrypted).
  - Never close a slot.
- `publishSticky(ctx)`:
  - If delayed events are supported: `ScheduleDelayed(StickyLeaveContent, DelayedLeaveDelay, MemberStickyDuration)` → `c.delayID`.
  - Then `SendSticky(StickyJoinContent, MemberStickyDuration)` → `c.stickyEventID`.
  - Child span `matrixrtc.member.publish` (attrs `matrixrtc.membership.format=sticky`, `matrixrtc.slot_id`, `matrixrtc.member.event_type`, `matrixrtc.delayed_leave` bool).
  - If `ErrUnsupported` on the sticky send: mark the login capability "no sticky" for 10 min and continue with legacy if the plan also has legacy; else fail like the legacy publish.
- Keeper goroutine (`obs.CarrySpan`, no live span):
  - ticker `DelayedLeaveRestart` → `UpdateDelayed(restart)`;
  - outcome `ok|gone|error` counted on `mrtcsip.matrixrtc.delayed_leave.restarts` (metric only, never spans, like REGISTER refresh);
  - on `gone` → reschedule once;
  - stops on `ctx` cancel at teardown;
  - results are posted to the actor non-blocking only on state change (gone → rescheduled / failed).
- Sticky refresh: actor timer `MemberStickyRefresh` → re-send the join (same member.id) via `SendSticky`. Linked root span `call.membership.refresh` with attr `matrixrtc.membership.format=sticky`; retry after `refreshRetryDelay`.
- Join wiring (`startJoin`):
  - plan.auth M → `rtc.JoinParams{Auth: AuthMSC4195, SlotID: plan.slot (as published), MemberID, LiveKitURL: plan.url, Owners: identities from c.ownerMem}`.
  - On `JoinError` stage `token_error` with M and `LiveKitAuth=auto`: switch to O once. Rebuild the sticky content with a `livekit_service_url` transport (re-send the sticky join, same member.id) and retry within the existing `maxJoinAttempts`.
- Legacy content in D+M: `JoinParams.MemberID = c.memberID`, `LiveKitURL = plan.url`.
- Ring (`sendRing`): `RingContent{SlotID: plan.slot, Sticky: plan.ringSticky, MembershipEventID: legacy event id or ""}`.
  - Sticky → `SendSticky(..., NotificationStickyDuration(RingTimeout))`; else `SendMessage`.
  - Ring end (answer, timeout, cancel): keep `redactRing`. If sticky, also send `RingWithdrawContent` sticky (child span `matrixrtc.ring.withdraw`).
- Decline of the owner's notification (`sendDecline`): if the owner's notification was sticky (`Event.StickyDuration>0`), send the decline sticky with the same duration (best effort).
- Teardown, after `rtc.leave` and before/with `clearMembership`:
  - Sticky leave: if `delayID` → `UpdateDelayed(send)`; on error or none → `SendSticky(StickyLeaveContent)`. Child span `matrixrtc.member.leave` (attr `matrixrtc.member.leave_via` = delayed_send|explicit).
  - Then the legacy clear if published.
  - Both are best effort and bounded by `teardownStepTimeout`.
  - The keeper is stopped first.
- Crash safety:
  - The sticky membership self-expires through the delayed leave (20 s) at most 1 h.
  - Nothing to clean on startup beyond today's behaviour.

Edge cases:
- Owner answers on a legacy device while the ghost is D: fine (unified parse).
- Owner MSC client never sees the slot (owner-initiated: it opened it).
- `/rtc/transports` returns only `livekit_service_url` → plan O.
- The owner's url differs from the lk-jwt configured url → `get_token` returns 400 `M_INVALID_PARAM` → fallback O.
- The server lacks MSC4512 → 404 → `ErrUnsupported` → O.
- Rate limits (429) on restart → count as `error` and keep ticking.
- A call longer than 1 h → the sticky refresh keeps it alive.

Tests:
- fakes: `GhostRTCIntent` recording calls with injectable errors, and a session `AddOwnerIdentities` recorder;
- each §2 row end to end;
- fallback M→O;
- keeper restart cadence with the fake clock;
- teardown order and contents;
- ring withdraw and redact;
- encrypted slot refusal;
- no DTMF or secrets in logs (existing privacy tests extended).

### WP6: config, telemetry, docs (parallel; closes last)
- `pkg/hostkit/config.go`: `MatrixRTCFormat string \`yaml:"matrixrtc_format"\``, `LiveKitAuth string \`yaml:"livekit_auth"\``, with validation, defaults "auto", `example-config.yaml` comments, and `upgrade.go` copy.
- `stack.go` wires `Config.MembershipFormat/LiveKitAuth/Transports` and the shared capability cache.
- `docs/TELEMETRY-SPEC.md`:
  - spans `matrixrtc.slot.open`, `matrixrtc.member.publish`, `matrixrtc.member.leave`, `matrixrtc.ring.withdraw` (all child-only, CLIENT);
  - attributes `matrixrtc.membership.format`, `matrixrtc.slot_id`, `matrixrtc.member.event_type`, `matrixrtc.delayed_leave`, `matrixrtc.member.leave_via`, `rtc.token.endpoint`, `matrixrtc.slot.action` (closed enums; slot id is a fixed small set);
  - metric `mrtcsip.matrixrtc.delayed_leave.restarts` {outcome};
  - `matrixrtc.kind` values `rtc_member`, `rtc_slot`;
  - `rtc.join.duration` outcome unchanged;
  - `member.id` is never on metrics; logs and spans may carry it at debug only (it is a random pseudonym, not a credential).
- `pkg/obs/attrs.go`: add the keys (export list is governed by the spec; update §3.3 and §5.2). README "Tracing and metrics" in the same change.
- `docs/spikes/S1-matrixrtc-wire-spec.md` §1.6: replace with a pointer to this doc §1, and update §4.4 (lk-jwt v0.10 slot `m.call#room`) and §4.5.
- `docs/EMBEDDING.md` §1.1/1.2:
  - ingest list now includes rtc.member and rtc.slot (automatic via `IngestEventTypes`);
  - Synapse prerequisites: `experimental_features: msc4354_enabled, msc4143_enabled, msc4140_enabled` (delayed), `msc4502_enabled`/`msc4512_enabled` with lk-jwt in AS mode for MSC4195 auth; `max_event_delay_duration` ≥ 30 s;
  - `GhostRTCIntent` is optional; `hostkit` provides it.
- `docs/DECISIONS.md` ADR-014 "Dual-format MatrixRTC: read all, write in the owner's dialect" (amends the S1 "don't send sticky" guidance).
- `docs/PLAN.md` current state and checklist.
- `docs/RESEARCH.md`: sources rows.
- No new dependencies (UUID via crypto/rand).

### WP7: jmpchat-matrix-bridge host (after a mrtc-sip-bridge tag)
- Bump `github.com/williamkray/mrtc-sip-bridge` (currently v0.3.3) in `go.mod`.
- Nothing else is required in code:
  - `pkg/connector/sipcalls.go:145` already loops `IngestEventTypes`;
  - ghosts come from `hostkit.NewGhostIntent` (gets `GhostRTCIntent`);
  - power levels come from `hostkit.CallPowerLevelOverrides` (slot types already PL 0; `m.rtc.member` is a message event under `events_default`; verify portal `events_default` is 0 in `sipcalls_host_test.go`);
  - config is inlined `hostkit.Config`, and the example comes from `hostkit.ExampleYAML`.
- Check `CopyKeys(h, "calls")` picks up the two new keys (`sipcalls_config.go:59`) and add a test.
- Update the jmpchat README and example config for `matrixrtc_format` / `livekit_auth` and the Synapse prerequisites.
- Optional: `!sip status` shows capabilities (sticky/delayed/msc4195) from the cache.

## 4. Dependencies
WP1 → (WP2 ∥ WP3 ∥ WP4) → WP5 → WP6 close → WP7. WP3 needs the WP2 interface signatures only (merge WP2's `callhost.go` first). WP0 runs in parallel and gates `auto` defaults: if WP0.1 fails, the `LiveKitAuth` default becomes `sfu_get`.

## 5. Keeping up with the spec
- All names, paths, flags and durations live in `events.go` with source and version. Readers accept unstable and stable names and both slot spellings.
- Writers choose via the dialect, so when ecosystems flip to stable names the bridge follows the owner automatically.
- When js-sdk switches to `m.rtc.member`/`m.call#room` and Synapse drops the query-parameter delayed send, delete the fallbacks (search `Unstable`, `SlotIDRoomLegacy`, `QueryDelay`).
- Re-check list: js-sdk `EventType.RTCMembership`, rust-rtc `wire.rs` `wire_event_type`, lk-jwt `helper.rs` slot mapping, Synapse `room.py` FIXME(MSC4140), and MSC4075 merge state (`notification_type`).

## 6. Risks
- Two client ecosystems disagree on type name and slot spelling (js-sdk unstable/`ROOM` vs rust-rtc stable/`room`). A ghost membership is only seen by clients of the chosen dialect. Dual legacy plus remembered dialect covers Element X (legacy indicator and ring), Element Web/EC compatibility (legacy), and the owner's native client (sticky).
- AS masquerade through the Synapse MSC4512 proxy (WP0.1). Fallback O exists.
- Delayed events for AS users without a device (WP0.2). Without them the sticky membership lingers up to 1 h after a crash.
- Sticky fields missing from AS pushes (WP0.3): the 1 h default may keep a dead owner device "live" in the detector. LiveKit presence and grace still end calls.
- A slot with encryption written by some client in an unencrypted portal → refused (silent audio otherwise).
- Synapse sticky per-sender caps (MSC4354 suggests 60) with many concurrent ghost calls: negligible (1 call per login).
- A union ring might trip strict validators: js-sdk ignores `m.relates_to` when `slot_id` is present; rust-rtc ignores extra fields (WP0.5).

## 7. Open questions for the user
- D1 default auth: msc4195-first (spec direction, matches your client) vs `sfu_get`-first (proven).
- D2 dual for bridge-initiated calls.
- D3 open both slot spellings, never close.
- D4 dialect memory in memory only (lost on restart) vs persisted per login.
- D5 restart cadence (20 s / 7 s) vs lk-jwt `delegate_delayed_leave` (needs SFU webhooks).
- D6 which client and version produced the 2026-11-19 logs.

### Decisions (2026-11-19)
- D1: `livekit_auth: auto` = MSC4195 homeserver `get_token` first, automatic fallback to `/sfu/get`.
- D2/D3: bridge-initiated calls publish dual (legacy + sticky) and open both slot spellings; slots are never closed.
- D4/D5: planner defaults (dialect memory in memory only; delayed leave 20 s, restarted every 7 s).
- D6: the 2026-11-19 client was sable-next (git.sable.moe) v26.11.4 (approx.), which sent stable names
  (`m.rtc.member`, `m.rtc.slot`, slot `m.call#room`). Not Element X.
- WP0: verified through telemetry after deploy (spans on every new step, automatic fallbacks), not by manual checks first.

## 8. Not in scope
Legacy `m.call.*`; video; E2EE slots; multi-slot rooms; group rooms.

## Implementation notes

Deviations from the plan above (code wins):
- Re-review fixes (2026-11-19): an encrypted slot refuses the call only for owner-initiated (mirror)
  plans and explicit `sticky`/`dual` config; a bridge-initiated call under `auto` drops the sticky part
  (`matrixrtc.plan.downgrade=slot_error`) and rings on the legacy path as before. `ensureSlots` has one
  overall deadline (`matrixOpTimeout`, 5 s) for all reads and writes, so slots never delay the ring by
  more than that. Under explicit `sticky`, an owner-initiated (mirror) call still falls back to legacy
  when the sticky publish fails.
- Review fixes (2026-11-19), all in `pkg/calls`, `pkg/rtc`, `pkg/hostkit` unless noted:
  - Bridge-initiated, `auto`, no dialect: sfu_get, legacy ring shape (`plan.ringSlot` = `m.call#ROOM`, not
    sticky), legacy membership content unchanged, sticky membership with a `livekit_service_url`
    transport. An explicit `livekit_auth: msc4195` is still honoured there. `ensureSlots` opens every slot
    spelling under both slot event types (pairs read first, nothing opened if any is encrypted); a mirror
    plan keeps its single (type, id) and also accepts the slot under the other type.
  - A mirror plan never fails the call for slot or sticky problems (slot error: continue; sticky failure:
    `dropSticky`, `plan.legacy = true`, legacy publish). Only an explicit `sticky`-only config still fails.
  - Token fallback: a `token_error`, or a `timeout` that happened before the LiveKit connect
    (`rtc.JoinError.BeforeConnect`), switches to sfu_get. It is free (`call.joinFallbacks`, `canRetryJoin`,
    `joinResults` capacity `maxJoinAttempts+2`) and remembered per login for 10 min
    (`Manager.tokenOff`, plan reason `msc4195_failed_recently`, no `/rtc/transports` lookup meanwhile).
    `LiveKitToken` is bounded to 4 s in `rtc.Join` (`tokenTimeout`).
  - hostkit: `paramRejected` needs errcode M_UNRECOGNIZED/M_UNKNOWN/M_INVALID_PARAM, a message naming the
    sticky/delay parameter and no limit wording; limits (M_LIMIT_EXCEEDED, M_FORBIDDEN, M_TOO_LARGE,
    "exceeds the allowed maximum") are plain errors with no query-form fallback and no unsupported mark.
    `missing()` counts a 404 only with M_UNRECOGNIZED or no errcode.
  - Gone detection is `errors.Is(err, core.ErrNotFound)` (hostkit `hsError.Is`: errcode `M_NOT_FOUND`);
    `calls.Config.IsDelayedGone` was removed.
  - Every live owner membership of the room contributes its `BackendIdentities()` at the trigger
    (`triggerLocked`, deduplicated, `maxOwnerIdentities`).
  - Sticky join sends are serialised by `delayKeeper.sendMu` (`call.sendStickyJoin`, `delayKeeper.resendJoin`):
    the keeper reads its content under the lock right before sending, the refresh and the fallback store the
    new content under it before sending, so the newest join always lands last.
  - The manager no longer caches `rtc.slot` events (nothing read them); ingest still drops them. The
    `matrixrtc.slot_id` attribute is `m.call#room`, `m.call#ROOM` or `other`. `OnParticipantConnected`
    announces an owner only once (`markAnnounced`; a departure clears it, so a reconnect is announced again).
- After a `gone` restart the keeper reschedules the delayed leave and also re-sends the sticky join (same
  `member.id`), because the leave may already be on the timeline. The plan only said "reschedule once".
  If rescheduling fails the keeper stops and logs a Warn.
- The keeper starts no span; outcomes are the metric only, state changes are Warn logs carrying the trace
  fields of the stored setup span context (`obs.LoggerSC`). The sticky refresh runs on the call actor as the
  linked root `call.membership.refresh`.
- The token fallback (MSC4195 to `/sfu/get`) re-publishes the sticky join (and the legacy content in a dual
  plan) and retries the join; it is not counted against `maxJoinAttempts` (2).
- New plan attributes beyond WP6: `matrixrtc.plan.format`, `.reason`, `.downgrade`,
  `matrixrtc.sticky.supported`, `matrixrtc.delayed.supported`, `matrixrtc.transports.status`,
  `rtc.token.fallback`, plus the `matrixrtc.plan.choose` INTERNAL span, and `matrixrtc.slot.action`
  also has `failed`. See `docs/TELEMETRY-SPEC.md` §5.2.
- A dual plan whose sticky part fails (slot error, sticky refused, sticky error) degrades to legacy only
  and records `matrixrtc.plan.downgrade`; a sticky-only config plan fails the call. A refused sticky send
  pauses sticky for the login for 10 min (`stickyUnsupportedTTL`). The capability cache keeps errors for 30 s.

## 9. WP0 results
(to fill in)
