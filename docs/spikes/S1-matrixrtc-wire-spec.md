# Spike S1: MatrixRTC wire spec for ringing from an appservice ghost

Status: research done 2026-10-16. **Nothing here has been exercised against live clients yet.** The
S1 spike code has to confirm it. Items marked **UNVERIFIED** were not confirmed in source, or the
sources disagree.

Goal: a ghost user (appservice puppet), in an **unencrypted** room with a human owner, makes the
owner's Element X (Android/iOS) and Element Web ring. When the owner answers, the ghost is in the same
LiveKit room and publishes an audio track the owner hears, attributed to the ghost.

## 0. Sources checked (versions and commits)

All repos were shallow-cloned on 2026-10-16. Licences per `docs/CREDITS.md`. AGPL sources were read,
not copied. Formats below are described in our own words or taken from the MSCs.

| Source | Version / commit | Licence | What was read |
|---|---|---|---|
| matrix-org/matrix-js-sdk | `develop` @ `d0e8162` (package 44.0.0) | Apache-2.0 | `src/@types/event.ts`, `src/matrixrtc/{types,CallMembership,MembershipManager,MatrixRTCSession,LivekitTransport,utils}.ts`, `membershipData/{session,rtc,common}.ts`, `client.ts` RTC endpoints |
| element-hq/element-call | `main` @ `8be229a` (post-v0.28.0), and tag `v0.28.0` | AGPL-3.0 | `sdk/src/media-backend/livekit/auth/*`, `sdk/src/matrixrtc/*`, `src/e2ee/sharedKeyManagement.ts`, `docs/self_hosting.md`, `v0.28.0:src/livekit/openIDSFU.ts`, commit `f9b6015` |
| element-hq/lk-jwt-service | `main` @ `33e5465` (latest release v0.9.0, 2026-10-13). **Now written in Rust.** | AGPL-3.0 | `README.md`, `src/handler.rs`, `src/helper.rs`, `src/requests.rs` |
| element-hq/element-web | `develop` @ `fc4d45c` | AGPL-3.0 | `apps/web/src/Notifier.ts` (`handleRTCNotification`), `toasts/IncomingCallToast.tsx` |
| element-hq/element-x-android | `develop` @ `ea8d647d` (embeds Element Call `0.28.0`) | AGPL-3.0 | `CallNotificationEventResolver.kt`, `ActiveCallManager.kt`, `DefaultCallWidgetSettingsProvider.kt`, feature flags |
| element-hq/element-x-ios | `develop` @ `734c067` | AGPL-3.0 | `NSE/Sources/NotificationHandler.swift`, `ElementCallWidgetDriver.swift` |
| matrix-org/matrix-rust-sdk | `main` @ `8769f1a` (pins ruma `09550bab`) | Apache-2.0 | `matrix-sdk-base/src/room/room_info.rs`, `matrix-sdk/src/room/calls.rs`, ffi `event.rs`, CHANGELOG |
| ruma/ruma | `09550bab4` (rust-sdk pin, 2026-10-01) | MIT | `ruma-events/src/call/member/*`, `rtc/{notification,decline}.rs`, `enums.rs`, `ruma-client-api/src/rtc/transports.rs`, `discovery/discover_homeserver.rs` |
| element-hq/synapse | `develop` @ `26a92c7` (1.163.0) | AGPL-3.0 | `rust/src/push/base_rules.rs`, `synapse/config/{experimental,server,matrixrtc}.py`, `rest/client/{matrixrtc,openid,room}.py`, config docs |
| lhns/matrix-sip-bridge | `0303e60` (2026-10-09) | Apache-2.0 (Pierre Kisters) | `pkg/calls/{membership,notification,identity,rtc,livekit,portaljoin}.go`, `docs/adr/0006,0010–0013` |
| MSC4075 (PR #4075, open) | head `4bdec0f963` | spec | `proposals/4075-rtc-notification-event.md` (now includes decline, which supersedes MSC4310) |
| MSC4310 (PR #4310) | **closed 2026-09-04**, folded into MSC4075 | spec | |
| MSC4143 (PR #4143, open) | head `b7cfa8125d` | spec | |
| MSC4195 (PR #4195, open) | hughns head `9259070646` | spec | room name/identity derivation, test vectors, "implementation differences" appendix |

URLs: `https://github.com/<owner>/<repo>/blob/<commit>/<path>` for each row above. For example
`https://github.com/matrix-org/matrix-js-sdk/blob/d0e8162/src/matrixrtc/CallMembership.ts`,
`https://github.com/element-hq/lk-jwt-service/blob/33e5465/src/handler.rs`,
`https://github.com/ruma/ruma/blob/09550bab4be30e80ad0918a65b4b10fb1caed69e/crates/ruma-events/src/call/member/focus.rs`.

### Executive summary

- **Membership:** send the **legacy per-device state event `org.matrix.msc3401.call.member`** (the
  "SessionMembershipData" shape). That's what Element Call sends by default ("Compatibility" mode),
  and it's the **only** membership type Element X's Rust SDK understands. The sticky `org.matrix.msc4143.rtc.member`
  (MSC4143 + MSC4354, "Matrix 2.0" mode) is opt-in in Element Call. Don't send it.
- **Ring:** send `org.matrix.msc4075.rtc.notification` (unstable name, **not** `m.rtc.notification`).
  Include `notification_type: "ring"`, which is required by Element X even though MSC4075 has dropped it. Also include
  `sender_ts`, `lifetime`, `m.mentions.user_ids: [owner]`, and an `m.reference` to the ghost's
  membership event. lhns sends `m.rtc.notification`. Element X accepts that as an alias, but Element
  Web and Synapse's push rules only match the unstable name.
- **Media:** the ghost advertises a LiveKit transport with **`livekit_service_url` only** (plus
  `livekit_alias`, which Element X needs to parse the event). It gets its token from lk-jwt-service
  **`POST /sfu/get`** with an OpenID token minted for the ghost. The resulting LiveKit identity is
  `@ghost:server:DEVICEID`, and clients expect exactly that for a membership whose transport has only `livekit_service_url`.
  The LiveKit room name is `unpaddedBase64(sha256('["<room_id>","m.call#room"]'))` (lk-jwt-service >= 0.10;
  v0.9 hashed `m.call#ROOM`), the same room the owner's client lands in. Sticky memberships and MSC4195
  tokens: see §1.6.

---

## 1. Membership: `org.matrix.msc3401.call.member` (state event)

### 1.1 Which event type clients require (late 2026)

| Client | Reads legacy `org.matrix.msc3401.call.member` | Reads sticky `org.matrix.msc4143.rtc.member` | Sends by default |
|---|---|---|---|
| Element Call (web, and embedded in EX/EW) | yes | yes (merges both, prefers sticky on duplicate) | legacy state event (`MatrixRTCMode.Compatibility` is the default; `Matrix_2_0` = sticky is a dev setting or `matrix_rtc_mode` in config.json) |
| matrix-js-sdk (Element Web) | yes | yes | n/a (uses EC) |
| matrix-rust-sdk / ruma (Element X native ring logic, `hasRoomCall`) | **yes** (`CallMemberEventContent`, alias `m.call.member`) | **no** (type not in ruma's enums) | n/a |

**Decision: send only the legacy state event.** Sending both isn't needed. A sticky membership
would also need Synapse `msc4354_enabled` and a different identity scheme (hashed, see §5).

Element X Android has a native MatrixRTC stack behind feature flag `feature.native_call`, which defaults
to **off**. By default it embeds the Element Call webview `0.28.0`.

### 1.2 State key

`_` + `{user_id}_{device_id}_{application}{call_id}`. For the room call, `application = "m.call"` and
`call_id = ""`. So:

```
_@sip_15551234567:example.org_SIPAB12CD34_m.call
```

- The leading `_` is required unless the room version is an MSC3757/MSC3779 unstable one
  (`org.matrix.msc3757.*` / `org.matrix.msc3779.*`). js-sdk `MembershipManager.makeMembershipStateKey`
  adds it for every other room version. Normal room versions refuse state keys starting with `@`
  unless they equal the sender exactly. Always use the underscore form for normal room versions.
- ruma parses it as: strip `_`, take everything up to the first `_` *after* the `:` as the user ID,
  and the rest as the member id. Underscores in the ghost localpart are fine. Avoid `_` in the server
  name (it can't occur in valid server names anyway).
- `device_id` is an **invented, opaque string**. Nothing looks it up (no `/devices` entry, no keys).
  Make it stable per call and unique among live calls in the room. lhns uses `"SIP" + upper(callID[:8])`.
  Keep it `[A-Z0-9]` for safety.

### 1.3 Content (join)

Use the union of what Element Call writes (js-sdk `MembershipManager.makeMyMembership` for
`SessionMembershipData`) and what ruma requires to deserialize (`SessionMembershipData` /
`LivekitFocus`):

```json
{
  "application": "m.call",
  "call_id": "",
  "scope": "m.room",
  "device_id": "SIPAB12CD34",
  "membershipID": "@sip_15551234567:example.org:SIPAB12CD34",
  "created_ts": 1792160000000,
  "expires": 14400000,
  "m.call.intent": "audio",
  "focus_active": { "type": "livekit", "focus_selection": "multi_sfu" },
  "foci_preferred": [
    {
      "type": "livekit",
      "livekit_service_url": "https://matrix-rtc.example.org/livekit/jwt",
      "livekit_alias": "!portalroom:example.org"
    }
  ]
}
```

Field-by-field:

| Field | Required by | Notes |
|---|---|---|
| `application` | js-sdk (string), ruma (tag; must be `"m.call"`) | |
| `call_id` | js-sdk (string), ruma | `""` = the room call. js-sdk maps `""` to slot `m.call#ROOM`. |
| `scope` | ruma (**required**, `"m.room"`). Optional in js-sdk. | Element X `has_active_room_call` filters on `scope == m.room`. |
| `device_id` | both | Invented. Must match the state key and the `/sfu/get` `device_id`. |
| `focus_active` | js-sdk (`type` string), ruma (`type: livekit`, `focus_selection` any string) | Element Call now writes `multi_sfu`. ruma accepts any string. lhns writes `oldest_membership`. Both parse. Use `multi_sfu`: with multi-SFU, every member publishes to its own `foci_preferred[0]`, and others subscribe there. |
| `foci_preferred` | js-sdk (array of `{type}`), ruma (**each entry needs `livekit_alias` and `livekit_service_url`**) | Element Call ≥ v0.28.0 stopped sending `livekit_alias` (commit `f9b6015`, "nothing used the value"). ruma at the rust-sdk pin still needs it. **Always send it.** Its value is ignored by current clients, so use the Matrix room ID like older EC did. **Do not add `url`**: that switches subscribers to the MSC4195 hashed identity (§5). |
| `expires` | ruma (**required**, ms). Optional in js-sdk (default 4 h). | A **duration relative to `created_ts`** (or `origin_server_ts` if `created_ts` is absent), not a deadline. 4 h = `14400000` matches EC's `DEFAULT_EXPIRE_DURATION`. |
| `created_ts` | optional | Set it to now on join. Synapse drops a state write whose content equals current state, so a re-publish has to differ (lhns ADR-0012 bumps `created_ts`). To *extend* a long call, EC keeps `created_ts` and raises `expires` to n × 4 h. |
| `membershipID` | optional (js-sdk only) | EC writes `"${userId}:${deviceId}"`. Setting it to exactly that makes every js-sdk version agree on the LiveKit identity (§5). |
| `m.call.intent` | optional | `"audio"`. Read only for DM rooms: Element X picks `JOIN_EXISTING_DM_VOICE` only if the room is a DM *and* the intent consensus is audio (lhns ADR-0013). Otherwise the call opens with the camera on and on speaker. |

**Silent failure modes:**
- ruma's `CallMemberEventContent` is `#[serde(untagged)]` over Legacy / Session / **Empty**. Empty
  has only optional fields and ignores unknown keys. A membership that fails Session parsing
  (missing `scope`, `expires`, `livekit_alias`, …) **deserializes as Empty, which means "left"**. Element X
  then sees no call and won't ring (`hasRoomCall == false`).
- js-sdk's `checkSessionsMembershipData` drops the membership on missing `device_id`, `call_id`,
  `application` or `focus_active.type`.

### 1.4 Leave

Send the same state key with empty content:

```json
{}
```

(js-sdk `clientSendLeaveMembership` sends `{}`. ruma parses it as `Empty`. `leave_reason:
{"m.lost_connection"}`-style content is optional and only used for delayed leaves.) Expiry also
counts as leaving: ruma and js-sdk both treat `created_ts + expires < now` as gone.

### 1.5 Power levels

- Element only offers the call button (and can only answer) if the user may send the membership
  event. Bridge portals have `state_default: 50` with the owner at 0. **Set
  `events["org.matrix.msc3401.call.member"] = 0`, and the same for `"m.call.member"`**, when creating
  portal rooms (lhns ADR-0011; Element's own rooms do the same).
- The ghost sends the membership itself, which needs the same override or a ghost PL ≥ state_default.
- js-sdk rejects (sticky) memberships whose `member.user_id` ≠ sender. For legacy events the sender
  *is* the member (state key and sender). So send as the ghost via appservice masquerade
  (`?user_id=@ghost`), never as the bot.

### 1.6 Newer formats (sticky `rtc.member`, `rtc.slot`)

Superseded 2026-11-19. The earlier "for awareness, don't send" guidance no longer holds: the bridge reads
sticky `m.rtc.member` and `rtc.slot` events and, depending on `matrixrtc_format`, publishes the sticky
membership next to the legacy one. Wire formats (type names, slot id spellings, sticky keys, leave
content, delayed events, transports, tokens) are in `docs/MATRIXRTC2-SPEC.md` §1, the decision table is
§2 there, and the rationale is ADR-014 in `docs/DECISIONS.md`. The slot power level override
(`matrixrtc.CallPowerLevelTypes`) is unchanged.

---

## 2. Ring notification: `org.matrix.msc4075.rtc.notification` (message event)

### 2.1 Type names

| Name | Who uses it |
|---|---|
| `org.matrix.msc4075.rtc.notification` | **Send this.** js-sdk `EventType.RTCNotification` (Element Web/Call send and receive *only* this), Synapse's MSC4075 push rules match only this, ruma's primary name. |
| `m.rtc.notification` | ruma alias (Element X accepts it). Element Web doesn't. lhns sends this. |
| `m.rtc.invite` | current MSC4075 stable name. **Nobody implements it.** The unstable id stayed `rtc.notification`. |
| `org.matrix.msc4075.call.notify` | deprecated. js-sdk stopped sending it (CHANGELOG #5167). Don't use. |

### 2.2 Content

```json
{
  "notification_type": "ring",
  "sender_ts": 1792160000123,
  "lifetime": 45000,
  "m.mentions": { "user_ids": ["@owner:example.org"] },
  "m.relates_to": { "rel_type": "m.reference", "event_id": "$ghostMembershipEventId" },
  "m.call.intent": "audio",
  "slot_id": "m.call#ROOM"
}
```

| Field | Needed by | Notes |
|---|---|---|
| `notification_type` | **ruma: required** (`ring`/`notification`). Element X rings only on `ring`. Element Web plays the ring sound only on `ring`. | Removed from the MSC text, but the reference implementation still sends it. |
| `sender_ts` | ruma required. js-sdk/Element Web use it as the lifetime base. | Element Web falls back to `origin_server_ts` if they differ by ≥ 15 s, ruma at > 20 s. Set it right before sending, and keep host clocks NTP-synced. |
| `lifetime` | ruma required. Element Web uses it (default 90 s if absent). | ms. js-sdk caps it at 120 s (MSC: MUST cap at 2 min). **Make it equal to the bridge's ring timeout** (30–60 s), as lhns does. Element X Android/iOS stop ringing at `expiration_ts = base + lifetime` with **no cap**. |
| `m.mentions.user_ids` | Push: Synapse `.org.matrix.msc4075.rule.rtc.invite_for_me` (if `msc4075_enabled`) or else `.m.rule.is_user_mention`. Element Web only handles it if push actions say `notify`. | Use explicit owner IDs. EC itself sends `{user_ids: [], room: true}`, but `room: true` only notifies if the sender has `notifications.room` power (default 50), which ghosts usually don't have. |
| `m.relates_to` | **Element Web requires** an `m.reference` to a membership event (fetchable). It ignores the notification otherwise. | Point it at the event ID returned when sending the ghost's call.member. |
| `m.call.intent` | Element X: shows "Incoming audio call" vs video. Element Web: shows voice vs video toast. | Optional but recommended. |
| `slot_id` | Only js-sdk `parseCallNotificationContent` (unused for ringing). Harmless elsewhere. | Optional, forward-compat. |
| `msc4354_sticky_key` | Only when sent as a sticky event (must equal `slot_id`). | Omit. Send as a normal event via `PUT /rooms/{id}/send/...`. |

Order matters: **send the membership first, then the notification.** Element X waits up to 3 s
(Android) / 5 s (iOS) for `hasRoomCall` after the push arrives, and treats the event as a plain
notification if it still sees no call.

### 2.3 Element X ringing conditions (Android `CallNotificationEventResolver`, iOS `NotificationHandler`)

1. A push arrives for the event, so a pusher must exist and the server-side push rules must say
   `notify` (§2.5).
2. Content parses as `RtcNotification` (ruma) with `notification_type == ring`.
3. The room has an active room call: `RoomInfo.has_active_room_call`, i.e. at least one
   non-expired, *parseable* `call.member` with `application m.call` + `scope m.room`. That's the ghost's membership.
4. Then Android shows the full-screen ringing notification, and iOS reports a CallKit VoIP call.

**No DM requirement, no member-count requirement** for ringing. DM-ness only picks the
voice-vs-video intent when answering.

### 2.4 Element Web ringing conditions (`Notifier.handleRTCNotification`)

1. Event type is exactly `org.matrix.msc4075.rtc.notification`, and client-side push evaluation
   gives `notify` (same rules as §2.5, fetched from the server).
2. The owner isn't already a member of the room's `m.call` session.
3. `now - sendTs <= lifetime`.
4. `m.relates_to.rel_type == "m.reference"` and the referenced membership event exists (local or
   fetched).
5. Shows `IncomingCallToast` (ring sound if `notification_type == ring`), deduplicated per
   `call_id`+room.

### 2.5 Push rules / phone wake

- Synapse ships `.org.matrix.msc4075.rule.rtc.invite_for_me` (type = unstable notification and
  `m.mentions.user_ids` contains the user; actions notify + `sound: ring`) and
  `.org.matrix.msc4075.rule.rtc.invite_for_room` **only if `experimental_features.msc4075_enabled:
  true`** (`rust/src/push/base_rules.rs`).
- Without it, `.m.rule.is_user_mention` (override, any event type) still matches
  `m.mentions.user_ids` and gives notify + highlight + default sound. That's enough to deliver the push
  that Element X turns into a ring. **So no custom push rule is needed** as long as the owner is in
  `m.mentions.user_ids`. Enabling `msc4075_enabled` is still recommended.
- Gotchas: a room set to "mute" or "mentions only" in a way that overrides mentions suppresses it.
  In *encrypted* rooms the server can't see the type or mentions (irrelevant here, rooms are unencrypted).

---

## 3. Decline, cancel, and how ringing stops

### 3.1 Decline event (MSC4310, now inside MSC4075)

Type **`org.matrix.msc4310.rtc.decline`** (ruma alias `m.rtc.decline`, MSC stable name
`m.rtc.decline`). Message event, sent by the owner:

```json
{
  "m.relates_to": { "rel_type": "m.reference", "event_id": "$ringNotificationEventId" },
  "msc4354_sticky_key": "$ringNotificationEventId"
}
```

(`msc4354_sticky_key` is present only when the client sent it as a sticky event.) Element X builds it with
ruma `RtcDeclineEventContent::new(notification_event_id)`, which serializes to type
`org.matrix.msc4310.rtc.decline`. The bridge should listen for **both** names, and only honour declines
whose `event_id` is a notification it sent and whose sender is the owner (lhns ADR-0011).

### 3.2 Stopping the ring

| Trigger | Element X (Android `ActiveCallManager`, iOS similar) | Element Web (`IncomingCallToast`) |
|---|---|---|
| Owner answers on another device | stops when the owner appears in `activeRoomCallParticipants` (parsed by ruma, see the caveat below) | stops when the owner appears in the call's participants |
| Owner declines on another device | stops on a decline from own user referencing the notification | same |
| Ghost membership cleared (`{}`) or expired | stops (`hasRoomCall` false) and shows a missed call | stops when participants drop to 0 |
| Notification **redacted** | **no** (not observed in the code) | stops (also on redaction of the referenced membership) |
| Lifetime elapsed | stops at `expiration_ts` | stops |

**So on SIP CANCEL or ring timeout the bridge must do both:** clear the ghost membership (for Element X)
and redact the notification (for Element Web and for clients that haven't synced the state).

**Caveat (UNVERIFIED, could break multi-device stop on Element X):** Element Call ≥ 0.28.0 publishes
`foci_preferred` entries without `livekit_alias`, but ruma's `LivekitFocus` (rust-sdk pin
`09550bab`, and ruma `main` as of 2026-10-15) still requires it. If that holds, an owner who answers
in an EC 0.28 client publishes a membership that Element X parses as "Empty". Other Element X devices
then keep ringing until `lifetime` or a decline. Keep `lifetime` short, and check this in S1.
(Our *ghost's* membership includes `livekit_alias`, so ringing itself isn't affected.)

---

## 4. LiveKit transport discovery and auth

### 4.1 How clients discover the focus

1. `GET /_matrix/client/unstable/org.matrix.msc4143/rtc/transports`, which returns `{"rtc_transports":
   [{type, livekit_service_url?, url?}]}`. Synapse serves it when `experimental_features.msc4143_enabled`
   is set, from `matrix_rtc.transports`. js-sdk only calls it if `/versions` advertises
   `org.matrix.msc4143`. ruma has only the unstable path. (MSC4519 "transports registry" now
   defines this endpoint, per EC's docs.)
2. Fallback: `.well-known/matrix/client` key **`org.matrix.msc4143.rtc_foci`** (ruma alias
   `m.rtc_foci`), used by matrix-rust-sdk `discover_rtc_transports` when the endpoint is missing.
   Element Call (sdk) no longer reads well-known itself. It uses the endpoint, then a host-supplied fallback.
3. Missing both gives `MISSING_MATRIX_RTC_TRANSPORT` (element-call#3933).

**The bridge doesn't need discovery:** it publishes its own `foci_preferred` and calls lk-jwt-service
directly. Make the bridge's `livekit_service_url` configurable, optionally defaulting to the first
`livekit` transport from the endpoint.

### 4.2 Transport shapes and which token endpoint clients use

Per EC `chooseAuthFlow` (main) and `openIDSFU.ts` (v0.28.0):

| Transport has | Publisher (own media) | Subscriber (someone else's transport) |
|---|---|---|
| only `livekit_service_url` (legacy) | OpenID → **`POST {svc}/sfu/get`** | OpenID → `POST {svc}/get_token`, falls back to `/sfu/get` on v0.28 |
| only `url` (MSC4195) | `POST /_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token` (homeserver, lk-jwt as appservice) | same |
| both | OpenID `/sfu/get` | C-S API `get_token` |

**The ghost publishes on a `livekit_service_url`-only transport and uses `/sfu/get`.** That's the only
path where an appservice can get a token without the MSC4195 homeserver proxy, and it gives the
identity clients expect for that transport shape (§5).

### 4.3 Get an OpenID token as the ghost (appservice masquerade)

```
POST /_matrix/client/v3/user/@sip_15551234567:example.org/openid/request_token?user_id=@sip_15551234567:example.org
Authorization: Bearer <as_token>
Content-Type: application/json

{}
```

Response:

```json
{ "access_token": "SomeT0kenHere", "token_type": "Bearer",
  "matrix_server_name": "example.org", "expires_in": 3600 }
```

Synapse (`rest/client/openid.py`) only checks that the path user equals the authenticated requester.
Appservice `user_id=` masquerading makes the requester the ghost, so this works (the ghost must be
registered, which bridgev2 does). mautrix-go: `intent.RequestOpenIDToken` / raw
`MakeFullRequest` (**UNVERIFIED** exact method name in mautrix v0.32).

### 4.4 lk-jwt-service legacy endpoint `/sfu/get`

```
POST https://matrix-rtc.example.org/livekit/jwt/sfu/get
Content-Type: application/json

{
  "room": "!portalroom:example.org",
  "openid_token": {
    "access_token": "SomeT0kenHere",
    "token_type": "Bearer",
    "matrix_server_name": "example.org",
    "expires_in": 3600
  },
  "device_id": "SIPAB12CD34"
}
```

Response `200`:

```json
{ "url": "wss://matrix-rtc.example.org/livekit/sfu", "jwt": "eyJhbGciOi..." }
```

What the service does (`handler.rs process_legacy_sfu_request`, `helper.rs`):

1. Verifies the OpenID token via `GET https://<fed-base>/_matrix/federation/v1/openid/userinfo?access_token=…`.
   `<fed-base>` comes from `https://<matrix_server_name>/.well-known/matrix/server` (`m.server`, default
   port 8448) or `https://<server_name>:8448`. **No SRV lookup.** Requires `sub`'s server ==
   `matrix_server_name`.
2. **Full access** if `matrix_server_name` ∈ `LIVEKIT_FULL_ACCESS_HOMESERVERS` (or `*`). Full access
   means canPublish = true and the service creates the LiveKit room (empty_timeout 5 min, departure_timeout 20 s).
   Restricted means it can only join existing rooms, and `can_publish` is false (verified: the
   handler passes `is_full_access_user` as the token's `can_publish`). A restricted ghost would join
   but be unable to publish audio. The ghost lives on the owner's homeserver, so list that server name.
3. Room membership is checked **only** when lk-jwt runs as an appservice *and* the full-access list is
   exactly its own homeserver. Then the ghost must be joined to the room (it is).
4. Identity = **`"{matrix_id}:{device_id}"`** → `@sip_15551234567:example.org:SIPAB12CD34`.
5. LiveKit room = `unpaddedStdBase64(sha256(json(["<room>", "m.call#room"])))` (slot hard-coded for
   the legacy endpoint). **Correction 2026-11-19:** v0.9 hashed `m.call#ROOM`; lk-jwt-service >= 0.10
   (checked v0.10.0, `map_to_stable_m_call_slot_id`) maps `ROOM` to `room` first, so the room is the same
   on every endpoint, including `/sfu/get`.
6. JWT grants: roomJoin, canPublish (full access), canSubscribe, canUpdateOwnMetadata, room = alias;
   TTL 1 h; `roomCreate: false`.

Errors: `400 M_NOT_JSON`/`M_BAD_JSON`, `401 M_UNAUTHORIZED` (OpenID failure), `403 M_FORBIDDEN` (not
joined, appservice mode only), `502 M_UNKNOWN`. Only POST is accepted. CORS headers are set.

Other lk-jwt endpoints, for reference: `/get_token` (standalone, body `{room_id, slot_id, openid_token,
member: {id, claimed_user_id, claimed_device_id}}`, identity = `b64(sha256(json([user, device,
member_id])))`), `/delegate_delayed_leave`, `/sfu_webhook`, `/healthz`. In appservice mode it adds
`/_matrix/client/unstable/io.element.msc4195/rtc/livekit/{get_token,delegate_delayed_leave}`
(identity = `b64(sha256(json([user, member_id])))`).

### 4.5 Room-name helper (for diagnostics only; never mint tokens ourselves)

Go, matching lk-jwt `livekit_room_alias_for` and the MSC4195 test vector
(`["!roomid:example.com","slot1234"]` → `O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc`). Use compact
JSON with HTML escaping off, standard base64 alphabet, no padding (lhns `identity.go`, Apache-2.0,
same approach):

```go
// LiveKitRoomName(roomID, "m.call#room")  (lk-jwt >= 0.10; v0.9 and older hashed "m.call#ROOM")
func hashJSON(parts ...string) string {
    var buf bytes.Buffer
    enc := json.NewEncoder(&buf)
    enc.SetEscapeHTML(false)
    _ = enc.Encode(parts)
    sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
    return base64.RawStdEncoding.EncodeToString(sum[:])
}
```

Assert in S1 that the JWT's `video.room` equals `hashJSON(roomID, "m.call#room")` (lk-jwt >= 0.10; against
0.9 or older use `m.call#ROOM`). The implementation is `matrixrtc.LiveKitRoomAlias`, which maps either
spelling to `room`; the hash helper moved to `pkg/matrixrtc/identity.go`.

---

## 5. LiveKit participant identity and track attribution

How the owner's client maps a LiveKit participant to the ghost's membership (js-sdk
`CallMembership.parseFromEvent`; EC sdk `LivekitBackend.mediaFor`; EC v0.28 `MatrixLivekitMembers`):

- For a legacy membership whose `foci_preferred` LiveKit transport has **`livekit_service_url`**, the
  accepted backend identity is **`"${sender}:${device_id}"`**. (EC v0.28 compares to
  `rtcBackendIdentity`. Current js-sdk sets that to the same legacy identity.)
- If the transport has **`url`**, it also accepts `b64(sha256(json([sender, memberId])))` (MSC4195
  2-tuple), where memberId = `membershipID ?? "${sender}:${device_id}"`.
- The client opens one LiveKit connection per distinct transport key (`url`,
  `livekit_service_url`, and serverName if `url` is present; `livekit_alias` is ignored). It then looks on the
  ghost's transport connection for a participant whose identity is in the ghost's identity list.
  **Unmatched participants are neither rendered nor heard** (lhns ADR-0010, ADR-0012). The SFU reports
  them as healthy, so server-side checks can't catch this.

So, for our ghost:

| Thing | Value |
|---|---|
| state key | `_@ghost:server_DEV_m.call` |
| content `device_id` | `DEV` |
| content `membershipID` | `@ghost:server:DEV` |
| `/sfu/get` `device_id` | `DEV` |
| LiveKit identity (JWT `sub`, chosen by lk-jwt) | `@ghost:server:DEV` ✔ matches |

Note on lhns: its `identity_scheme: hashed` uses the **3-tuple** `[user, device, member]` hash, which is
lk-jwt's *standalone `/get_token`* identity and not what MSC4195 or js-sdk now accept (2-tuple). Don't
copy it. Its default `user_device` scheme equals the table above.

Publishing with `github.com/livekit/server-sdk-go/v2` (latest v2.19.0; method names from memory,
**verify in S1**): `lksdk.ConnectToRoomWithToken(url, jwt, &lksdk.RoomCallback{…})`, then
`lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000,
Channels: 2})` and `room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name:
"audio", Source: livekit.TrackSource_MICROPHONE})`. Use Source MICROPHONE (EC maps tracks by
source; treating `UNKNOWN` as audio is **UNVERIFIED**). Publish **no video track**.
Use `lksdk.WithAutoSubscribe(true)` (default) to receive the owner's audio.

---

## 6. Gotchas

1. **E2EE:** Element Call uses per-participant SFrame E2EE iff the room has `m.room.encryption`
   (standalone `sharedKeyManagement.ts`). Element X passes `PerParticipantKeys` vs `Unencrypted` based
   on room encryption (Android `DefaultCallWidgetSettingsProvider`, iOS `ElementCallWidgetDriver`).
   Plain audio into an encrypted-room call decodes to **silence**. Portal rooms must be unencrypted,
   and the bridge refuses or warns otherwise.
2. **`application`/`scope`:** must be `"m.call"` / `"m.room"`, `call_id: ""`. Anything else is a different
   slot (`m.call#<id>`) or a user-scoped call, which Element X `has_active_room_call` ignores.
3. **Power levels** for `org.matrix.msc3401.call.member` / `m.call.member` = 0 (§1.5). Without it
   there's no call button and no error.
4. **Owner must be joined** to the portal (double puppet, or accept the invite). Non-members don't see
   state or get pushes.
5. **DM marking** (`is_direct` on invite + `m.direct` via double puppet) only picks the voice-vs-video
   intent and the earpiece/speaker route in Element X (lhns ADR-0013). It isn't needed to ring.
6. **Membership expiry and refresh:** `expires` is a duration. 4 h with `created_ts = now` covers S1. For
   longer calls, re-send with the same `created_ts` and `expires = n × 4h` before expiry (EC's
   pattern). A crash leaves a phantom member until expiry, so clear stale ghost memberships at startup (lhns).
7. **Delayed events (MSC4140)** for leave-on-disconnect: on Synapse, enabled iff
   `max_event_delay_duration` is set (top-level config, **not** `experimental_features`), via
   `?org.matrix.msc4140.delay=<ms>` on the state PUT and
   `/_matrix/client/unstable/org.matrix.msc4140/delayed_events/{id}/restart`. Using these masqueraded as a
   ghost is **UNVERIFIED** (should work, auth is per-request). Not needed for S1 (we own the SIP leg and
   send explicit leaves). Element Call itself does need it for its own clean leaves, so enable it on the server.
8. **MSC4222** (`msc4222_enabled`, sync `state_after`) is needed by Element Call to track room state
   reliably. Server-side setting, not something the bridge sends.
9. **`sender_ts` skew:** Element Web uses `origin_server_ts` if they differ by ≥ 15 s. Keep the bridge
   clock synced.
10. **Don't use `m.rtc.notification`** (lhns) or `m.rtc.invite` (MSC text). Element Web and Synapse push
    rules only know `org.matrix.msc4075.rtc.notification`.
11. **Reverse proxy:** route the *whole* `/livekit/jwt/` prefix. EC's Caddy sample only forwards
    `/sfu/get` and `/healthz`, which breaks subscribers that try `/get_token` first.
12. **Room creation race:** with `room.auto_create: false` on LiveKit, the room exists only after a
    full-access token request. The ghost's `/sfu/get` creates it. If the ghost waits for the owner to
    answer, the owner's own token request creates it. Either way works.

---

## 7. S1 sequence (inbound ring → answer → audio)

1. Ensure the portal is unencrypted, PLs allow call.member at 0, and the owner and ghost are joined.
2. Ghost: `PUT /_matrix/client/v3/rooms/{room}/state/org.matrix.msc3401.call.member/_@ghost:srv_DEV_m.call?user_id=@ghost:srv`
   with the §1.3 content. Keep the returned `event_id` (M).
3. Ghost: `PUT /_matrix/client/v3/rooms/{room}/send/org.matrix.msc4075.rtc.notification/{txn}?user_id=@ghost:srv`
   with the §2.2 content, `m.relates_to.event_id = M`. Keep `event_id` (N).
4. (Optional, pre-warm) OpenID token → `/sfu/get` → connect to LiveKit and publish the tone. Doing this
   *before* the owner answers means the identity is present when the owner's client resolves
   participants (lhns ADR-0012 saw one-way audio when the SIP participant joined late on the outbound
   path).
5. Watch room state for a `call.member` from the owner with a non-empty, unexpired content → answered.
   Watch for `org.matrix.msc4310.rtc.decline` / `m.rtc.decline` referencing N → declined.
6. On answer: if not pre-warmed, do step 4 now. Publish audio. Optionally redact N (Element Web
   already stops when the owner joins).
7. On caller cancel or timeout: redact N, then set the ghost membership to `{}`, then disconnect LiveKit.
8. On hangup (owner membership → `{}`/expired, or SIP BYE): disconnect LiveKit, set the membership to `{}`.

---

## 8. Synapse/deployment checklist for the owner

Do this on the dev homeserver before running S1. **First confirm two Element accounts can call each
other** (rules out transport/proxy problems), then add the bridge.

### 8.1 Synapse (`homeserver.yaml`, checked against 1.163.0)

```yaml
experimental_features:
  msc4143_enabled: true    # serves /_matrix/client/unstable/org.matrix.msc4143/rtc/transports (+ /versions flag)
  msc4222_enabled: true    # sync state_after, needed by Element Call
  msc4075_enabled: true    # RTC ring push rules (.org.matrix.msc4075.rule.rtc.invite_for_me/_room). Recommended.
  msc3266_enabled: true    # room summary; only for standalone Element Call knocking, optional
  # msc4354_enabled: NOT needed (sticky events / Matrix 2.0 mode)

max_event_delay_duration: 24h   # enables MSC4140 delayed events (clients' clean leaves)

rc_message:
  per_second: 0.5
  burst_count: 30
rc_delayed_event_mgmt:
  per_second: 1
  burst_count: 20

matrix_rtc:
  transports:
    - type: livekit
      livekit_service_url: https://matrix-rtc.example.org/livekit/jwt
      # For S1 leave out `url:`. With `url`, clients switch to the MSC4195
      # homeserver token API, which needs lk-jwt-service as an appservice
      # (msc4502/msc4512) and gives a different identity scheme.
```

- **A `federation` or `openid` listener resource** reachable from lk-jwt-service at
  `https://<server_name>/.well-known/matrix/server` → `m.server`, or at `https://<server_name>:8448`.
  lk-jwt verifies every OpenID token (including the ghost's) through
  `/_matrix/federation/v1/openid/userinfo`. It doesn't use SRV.

### 8.2 `https://<server_name>/.well-known/matrix/client` (CORS `*`)

```json
{
  "m.homeserver": { "base_url": "https://matrix.example.org" },
  "org.matrix.msc4143.rtc_foci": [
    { "type": "livekit", "livekit_service_url": "https://matrix-rtc.example.org/livekit/jwt" }
  ]
}
```

(Fallback for clients or hosts that can't use the transports endpoint.)

### 8.3 LiveKit SFU

- `keys: { <KEY>: <SECRET> }`, `room.auto_create: false`, RTC ports open (default UDP range +
  TCP 7881; set `rtc.use_external_ip` / `node_ip` as needed), TURN optional.
- Reverse proxy `wss://matrix-rtc.example.org/livekit/sfu` → `:7880`, with WebSocket upgrade.
- Optional: `webhook.urls: [https://matrix-rtc.example.org/livekit/jwt/sfu_webhook]`, `webhook.api_key: <KEY>`
  (only for delegated delayed leaves).

### 8.4 lk-jwt-service (v0.9.0+, standalone mode is fine for S1)

```
LIVEKIT_URL=wss://matrix-rtc.example.org/livekit/sfu
LIVEKIT_KEY=<KEY>
LIVEKIT_SECRET=<SECRET>
LIVEKIT_FULL_ACCESS_HOMESERVERS=example.org     # your server_name; the ghost must be full-access to publish/create rooms
LIVEKIT_JWT_BIND=:8080
```

Reverse-proxy the **entire** `https://matrix-rtc.example.org/livekit/jwt/` prefix → `:8080/`
(`/sfu/get`, `/get_token`, `/healthz`, …). For a self-signed lab only:
`LIVEKIT_INSECURE_SKIP_VERIFY_TLS=YES_I_KNOW_WHAT_I_AM_DOING`.

Appservice mode (only if you later want `url` transports / MSC4195 C-S endpoints): Synapse
`msc4502_enabled` + `msc4512_enabled`, plus a separate appservice registration for lk-jwt (see its
README). Not needed for S1.

### 8.5 Bridge appservice registration

- Exclusive user namespace for ghosts (e.g. `@sip_.*:example.org`) and the bot. `rate_limited: false`.
- Nothing extra for OpenID: masquerading (`?user_id=`) is standard appservice behaviour.
- Portal rooms: **no `m.room.encryption`**; power levels `events: {"org.matrix.msc3401.call.member": 0,
  "m.call.member": 0}`; owner joined (double puppeting recommended); optionally `is_direct` +
  `m.direct` for the voice intent.
- Bridge config needs the public `livekit_service_url` (the same URL as `matrix_rtc.transports`).

### 8.6 Clients

- Element X Android/iOS: notifications enabled. Check that a plain @-mention from another account
  pushes while the app is backgrounded. If that doesn't push, the ring won't either.
- Element Web: Element Call is the default call backend in current develop builds. A self-hosted Element
  Web with a custom `element_call` config may need `element_call.url` (**UNVERIFIED** for your build).

---

## 9. Open questions to settle in the S1 spike

1. Does Element X stop ringing on device B when the owner answers on an EC-0.28 client (missing
   `livekit_alias` vs ruma)? See §3.2.
2. Does server-sdk-go's publish with `Source: MICROPHONE` render as the ghost's audio in Element X and
   Web (identity match per §5)? Check the JWT `sub` and `video.room` against §4.4/§4.5.
3. Is pre-joining LiveKit before the answer (step 4) needed to avoid one-way audio, or is joining on
   answer fine?
4. Exact mautrix-go v0.32 call for `openid/request_token` as a ghost intent.
