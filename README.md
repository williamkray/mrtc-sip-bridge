# mrtc-sip-bridge

A Matrix ↔ SIP calling bridge. It registers to a SIP provider with ordinary SIP account credentials
(like a desk phone or ATA would), and turns calls into MatrixRTC (Element Call / LiveKit) calls:

- Incoming calls ring **all** of your Matrix clients. Answer on whichever one you like.
- Outgoing calls are placed from a Matrix room per phone number.
- Each call leaves a "missed / answered / outgoing" notice in the room.

First target is a [JMP.chat](https://jmp.chat) number's SIP account. Texting for JMP numbers is handled
separately by [jmpchat-matrix-bridge](https://github.com/williamkray/jmpchat-matrix-bridge) (formerly mautrix-jabber) via Cheogram. In
[embedded mode](#embedded-mode-jmpchat-matrix-bridge) that bridge runs this call core itself, so a number's
texts and calls share one room.

**Status:** inbound calls, **outbound calls** and DTMF are implemented and validated against a live JMP
account and a Synapse/LiveKit deployment. The bridge registers with SIP
credentials, keeps the account registered, creates per-number rooms with `!sip start-chat <number>`,
bridges **incoming calls** (they ring all your Matrix clients) and **outgoing calls** (press call in a
number's room, or `!sip call <number>`), and sends keypad tones with `!sip dtmf`. See
[docs/PLAN.md](docs/PLAN.md), [docs/PHASE2-SPEC.md](docs/PHASE2-SPEC.md),
[docs/PHASE3-SPEC.md](docs/PHASE3-SPEC.md), [docs/DECISIONS.md](docs/DECISIONS.md) and
[docs/RESEARCH.md](docs/RESEARCH.md). Credits and licensing: [docs/CREDITS.md](docs/CREDITS.md).

## Requirements

- A Matrix homeserver with a working MatrixRTC setup (LiveKit SFU + lk-jwt-service), already able to
  do Element Call between two normal users. The bridge finds lk-jwt-service through
  `https://<homeserver.domain>/.well-known/matrix/client` (`org.matrix.msc4143.rtc_foci`), or through
  `network.livekit_service_url`. The lk-jwt-service URL is never taken from the newer
  `/_matrix/client/v1/rtc/transports` endpoint (Synapse 1.165+ `matrix_rtc.transports`, transport types
  `m.livekit`/`livekit`): if your homeserver advertises LiveKit only there and no longer in `.well-known`,
  set `network.livekit_service_url`. (The bridge does read `/rtc/transports` for the SFU address of
  sticky memberships, see "MatrixRTC 2.0" below.) lk-jwt-service must accept the bridge ghosts: its
  `LIVEKIT_FULL_ACCESS_HOMESERVERS` has to include your homeserver (or `*`).
- Double puppeting configured for the bridge (read receipts as you stop the ring on push-driven clients).
- A SIP account (registrar, username, password) and a reachable UDP port range for RTP.
- Rooms must be unencrypted (encrypted rooms give silent Element Call audio). The bridge is built without
  encryption support and forces `encryption.default: false` at startup, with a warning if your config
  sets it.

## Build

```bash
./build.sh                      # Docker build; extracts ./mrtc-sip-bridge and tags the image mrtc-sip-bridge
```

Or locally, with Go 1.26+, cgo and the sqlite headers:

```bash
go build -tags "nocrypto nolibopusfile" -o mrtc-sip-bridge ./cmd/mrtc-sip-bridge
```

Local builds also need libopus headers (`pkg-config opus`). Always build, vet and test with
`-tags "nocrypto nolibopusfile"`: `nocrypto` drops libolm, `nolibopusfile` builds the Opus binding without
opusfile. The Docker image installs libopus.

## Configure

```bash
./mrtc-sip-bridge -e        # write an example config.yaml
# edit config.yaml: homeserver.address/domain, database, appservice, bridge.permissions, network.*
./mrtc-sip-bridge -g        # generate registration.yaml, and add it to your homeserver
./mrtc-sip-bridge           # run
```

With Docker, `docker run --rm mrtc-sip-bridge > config.yaml` prints the example config (the container
does this whenever it is started without arguments and `/data/config.yaml` does not exist). With
`/data` mounted and containing `config.yaml`, a no-argument start runs the bridge with
`-c /data/config.yaml -r /data/registration.yaml`; any arguments you pass (for example `-g`) replace
that default. The default appservice port is **29319** and the
command prefix is `!sip`. Ghost users are `@sip_<digits>:server`.

The `network:` section:

| Key | Default | Meaning |
|---|---|---|
| `sip_bind_ip` | `""` | Local IPv4 for SIP sockets. Empty or `0.0.0.0` = first non-loopback IPv4. |
| `sip_port_min` / `sip_port_max` | 5060 / 5069 | SIP port range, one port per active login (UDP and TCP). |
| `external_ip` | `""` | Public IPv4 for the SIP Contact and SDP. Empty = discover via STUN, else the bind IP. |
| `stun_server` | `stun.l.google.com:19302` | STUN `host:port` used when `external_ip` is empty. Empty disables STUN. |
| `rtp_port_min` / `rtp_port_max` | 40000 / 40100 | RTP (call audio) UDP range. Must be reachable from your provider. Two ports per active call. |
| `register_expiry_seconds` | 60 | REGISTER `Expires` to request. |
| `register_refresh_seconds` | 25 | How often to re-register. At least 5 and less than the expiry. |
| `ring_timeout_seconds` | 30 | Ring time before a call counts as missed (5-300). Element Web stops ringing after 120 s regardless. |
| `rtp_timeout_seconds` | 60 | Hang up an answered call when no audio (RTP) arrives from the provider for this long (10-3600). |
| `livekit_service_url` | `""` | lk-jwt-service URL, e.g. `https://matrix-rtc.example.org/livekit/jwt`. Empty = discover from the homeserver's `.well-known` (`org.matrix.msc4143.rtc_foci`). |
| `matrixrtc_format` | `auto` | Membership format of the bridge's phone-number user: `auto` (follow your client, see "MatrixRTC 2.0"), `legacy` (only the `call.member` state event, the behaviour before 2.0), `sticky` (only the sticky `m.rtc.member`), `dual` (both). |
| `livekit_auth` | `auto` | How the bridge gets its LiveKit token: `auto` (homeserver `get_token` per MSC4195, falling back to lk-jwt-service `/sfu/get`), `sfu_get` (always `/sfu/get`, the behaviour before 2.0), `msc4195` (only `get_token`). |
| `default_country_code` | `"1"` | Country code assumed for numbers without a leading `+`. |
| `outbound_trigger` | `ring` | How the bridge notices that you started a call in a number's room: `ring` (your client's call notification plus your fresh call membership), `membership` (fresh membership alone, for clients that send no notification), `off` (only `!sip call`). |
| `dial_timeout_seconds` | 60 | Cancel an outgoing call that is not answered within this time (10-300). |
| `dial_allowed_prefixes` | `["+1"]` | Only numbers starting with one of these E.164 prefixes can be called. `[]` = any number. Emergency numbers and short codes can never be called. |
| `dial_number_format` | `digits` | Request-URI user for dialled numbers: `digits` (`15551234567`, JMP/Bandwidth) or `e164` (`+15551234567`, unverified). |
| `allow_self_dial` | `false` | Allow calling the account's own number (testing only: the call loops back through the provider). |
| `dtmf_redact_command` | `true` | Redact `dtmf` command messages after sending the tones (they may contain PINs). |

`stun_server: ""` plus an explicit `external_ip` keeps the bridge from contacting Google.

### MatrixRTC 2.0 (sticky memberships)

Newer clients publish their call membership as a sticky `m.rtc.member` event in a room call slot
(MSC4143, MSC4354) instead of a per-device `call.member` state event. The bridge reads both formats, in
both type-name and slot-id spellings. What it publishes for its phone-number user is `matrixrtc_format`:

- A call you start from a client that uses sticky memberships is answered with a sticky membership in the
  same dialect (type name, slot id, SFU address). A client that still uses `call.member` gets `call.member`.
- Calls the bridge starts (incoming SIP calls, `call`, the Dialpad widget) publish both formats, so every
  client sees the call. The bridge opens the room's `m.call` slot if it is not open and never closes it. A
  slot that declares encryption makes the bridge refuse the call, like an encrypted room.
- The sticky membership carries a delayed leave (MSC4140, 20 s, restarted every 7 s), so a crashed bridge
  does not leave a ghost in the call for up to an hour. Long calls refresh the membership every 40 min.
- With no sticky support on the homeserver (or `matrixrtc_format: legacy`) nothing changes.

Synapse prerequisites for the sticky format (`homeserver.yaml`, names as of Synapse 1.165):

```yaml
experimental_features:
  msc4354_enabled: true   # sticky events
  msc4140_enabled: true   # delayed events
  msc4143_enabled: true   # /rtc/transports
max_event_delay_duration: 30s   # at least 30 s
```

For the homeserver token (`livekit_auth: auto` or `msc4195`) lk-jwt-service 0.10 or newer has to run in
appservice-proxy mode with Synapse forwarding `get_token` to it (`msc4502_enabled` and `msc4512_enabled`; see
the lk-jwt-service README). The request's `url` must equal the service's `LIVEKIT_URL` byte for byte. If the
token request fails, the bridge uses `/sfu/get` for that call and re-publishes its membership accordingly.
Tracing shows which path each call took (`matrixrtc.plan.choose`, `rtc.token.fallback`).

## Dialpad widget

Most Matrix clients have no dialpad. With `network.widget.enabled: true` the bridge adds a **Dialpad**
widget to each number room and to your management room. Open it from the room's widget list: a keypad to
type or paste a number (normalised like `call`, same number policy), a call button, in-call DTMF keys and a
hang-up button. It dials through the same call core as `!sip call`, so your devices ring from the number's
ghost and you answer to start the call.

- **Contacts, Message and roster-only (embedded in jmpchat-matrix-bridge only):** when the host supports them
  (jmpchat-matrix-bridge v0.3.0 and later), the Dialpad also has an optional **Name** field, a **Message** button
  (opens the number's text chat, with an "Open chat" link), **Save contact** (saves the number to your
  Jabber roster; in a number room it shows "Saved as ..." and can rename it) and an **Only ring for
  contacts** checkbox (inbound calls from numbers outside your roster go to voicemail). A name typed
  with Call saves the contact before the call. These controls appear only when the host offers them: the
  standalone bridge shows none of them. The widget never logs or traces contact names (the host's own
  roster logging may). See
  [docs/WIDGET-SPEC.md](docs/WIDGET-SPEC.md) §7.5 for the host interfaces.
- **Contacts tab (embedded in jmpchat-matrix-bridge only):** when the host lists contacts, the Dialpad and the
  PWA get a **Contacts** tab next to **Dial**: search your saved phone contacts by name or digits, then
  tap **Call** or **Message** on a row. Only named phone contacts appear (numbers saved without a name
  and plain Jabber contacts do not). Names are shown to you only and are kept in memory, never stored in
  the browser or logged.
- **Supported clients:** Sable and gomuks (both show room widgets and answer the widget's OpenID
  request). **Element X does not show widgets**: use `call`, `dtmf` and `hangup` there.
- **Who can use it:** only the owner of the login the room belongs to. The widget URL carries a per-room
  key derived from `widget.secret`; on open the widget asks your client for an OpenID token and the bridge
  checks it with your homeserver (users of other homeservers are refused unless `allow_remote_users`).
  Every action re-checks that the room is still one of yours.
- **Installation:** the bridge bot sends the widget state event (it needs power level 50 in the room; if it lacks it,
  `!sip widget` tells you). A sweep at startup, one every 10 minutes and room creation
  by an outgoing call install it in every eligible room, and `!sip widget` in a room installs it right
  away. It is idempotent: rooms that already have the current widget are left alone.
- **Disabling:** `enabled: false` mounts nothing. With `remove_when_disabled: true`, the next start removes
  existing Dialpad widgets from rooms.

`network.widget` keys:

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Serve and install the widget. |
| `public_url` | `""` | Public HTTPS URL of the widget, ending in `/` (for example `https://bridge.example.org/sip-dialpad/`). `http` is accepted only for localhost. The path must not be `/` or start with `/_matrix/`. |
| `secret` | `generate` | Secret for the per-room keys (at least 32 characters). `generate` is replaced with a random value on start. Changing it changes every widget URL; the next sweep updates the rooms. |
| `frame_ancestors` | `["*"]` | Origins allowed to frame the widget (CSP `frame-ancestors`): `*`, `'self'` or scheme/host sources. Narrow it to your client's origin(s) if you can. |
| `allow_remote_users` | `false` | Let users of other homeservers use the widget (their OpenID token is checked over federation). |
| `trusted_proxies` | `[]` | CIDRs or bare IPs of reverse proxies whose last `X-Forwarded-For` hop is trusted as the client address (used for rate limiting). Empty: the socket peer address is used. |
| `remove_when_disabled` | `false` | With `enabled: false`, remove existing Dialpad widgets on start. |

**Private rooms only.** The widget URL carries the room key, so anyone who can read the room's state can
use it. The bridge therefore installs the widget, and serves it, only in rooms whose joined or invited
members are just you, the bridge bot and this bridge's ghosts; a room with anyone else is refused. The
check reads the bridge's view of room membership and cannot see people who join later until the next
sweep. Residual risk: a member added afterwards can use the widget until the key rotates (see
[ADR-012](docs/DECISIONS.md)).

**Exposure.** The widget is served by the bridge's existing appservice listener (default port 29319) under
the path of `public_url`; there is no extra port. Forward only that path prefix from your reverse proxy and
never `/_matrix/app`. The proxy must not strip the prefix. With Traefik (labels on the bridge container):

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.sip-dialpad.rule=Host(`bridge.example.org`) && PathPrefix(`/sip-dialpad/`)
  - traefik.http.routers.sip-dialpad.entrypoints=websecure
  - traefik.http.routers.sip-dialpad.tls.certresolver=letsencrypt
  - traefik.http.services.sip-dialpad.loadbalancer.server.port=29319
```

The `widget` bot command (`!sip widget`, in a number room or the management room) adds the widget to that
room and reports if the bot lacks the power level to do so.

## Dialpad app (PWA)

Clients without room widgets (Element X) have no dialpad. The **Dialpad app** is an installable web app
(PWA) served by the bridge: a dialer and remote control. You type or pick a number, press Call, and the
call **rings your Matrix clients** like any other bridged call; the app shows the call state and sends
keypad digits and hang-up. It carries no audio. It needs `widget.enabled: true` and its own switch,
`widget.pwa.enabled: true` (the bridge refuses to start with `pwa` but no `widget`).

```yaml
network:
  widget:
    enabled: true
    public_url: https://jabber.bridge.example.com/dialpad/
    pwa:
      enabled: true
      # inactivity_days: 90        # a device unused this long is signed out
      # max_lifetime_days: 0       # optional absolute lifetime (0 = none)
      # rotation_grace_seconds: 120
      # enroll_minutes: 15         # lifetime of a setup link
      # max_links_per_user: 5
      # notify_on_enroll: true
```

**Setting up a phone.** In your management room (your DM with the bridge bot; it is refused elsewhere and
in rooms with other members) run `!sip dialpad-link [name]` (for example `dialpad-link Pixel`). The bot
posts a one-time link valid for `enroll_minutes`. Open it on the phone (or open the app URL and paste the
link), check that the confirmation page shows your Matrix ID, and press Set up. The link message is
redacted right away, and a link can be used once only. Other commands, all management-room only:

- `dialpad-links` lists your links (name, state `waiting for setup`/`active`/`expired`, client, last use).
- `dialpad-revoke <id|all>` signs a device out immediately (ids are shown by `dialpad-links`).

**Installing.** Chrome and Firefox on Android: open the link, finish setup, then use the browser menu
(Install app / Add to home screen) and start the app from the home screen. iOS Safari: finish setup in
Safari, then Share, Add to Home Screen. On iOS the home-screen app has its own storage, so open the link
(or paste it) again from the installed app if it shows the setup screen.

**Renewal and expiry.** The device holds a long-lived token that the app swaps for a fresh one every time
it opens a session (about hourly while in use), so a device in use never needs reinstalling. A device that
is not used for `inactivity_days` (default 90) expires and must be set up again. **Reuse protection:**
if an already replaced token is presented after the grace window, the bridge assumes a copy was stolen,
signs the whole device out and tells you in the management room. Restoring an old browser backup therefore
also signs the device out. Rotation is lossless: a lost response or two open tabs never log you out.

**Production exposure.** The app keeps its token in `localStorage` of the origin that serves it, so give
it a **dedicated hostname with a path prefix**, such as `https://jabber.bridge.example.com/dialpad/`, and
route only that prefix to the bridge (never `/_matrix`, and do not share the origin with other web
apps). With Traefik, as for the widget:

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.sip-dialpad.rule=Host(`jabber.bridge.example.com`) && PathPrefix(`/dialpad/`)
  - traefik.http.routers.sip-dialpad.entrypoints=websecure
  - traefik.http.routers.sip-dialpad.tls.certresolver=letsencrypt
  - traefik.http.services.sip-dialpad.loadbalancer.server.port=29319
```

The prefix must not be stripped. The app is served under `<public_url>app/`. A start-up warning reminding you of this is logged
whenever the app is enabled under a path prefix. Design and threat model: `docs/PWA-SPEC.md`, ADR-013.

## Networking

Each login uses its own SIP port from `sip_port_min..sip_port_max`, and each call uses two ports from the RTP range (`rtp_port_min..rtp_port_max`; the default range allows about 50 simultaneous calls).
The simplest Docker setup is `network_mode: host`. Otherwise publish `5060-5069/udp`, `5060-5069/tcp`
and `40000-40100/udp` with **identical host and container ports**, and set `network.external_ip` to your
public address. Forward the same ports on your router if the bridge is behind NAT. The RTP range must be
reachable from your SIP provider; if it is not, calls connect but are silent and are hung up after
`rtp_timeout_seconds`. The bridge also needs outbound HTTPS and WebRTC access to your LiveKit server.

Deployment notes:

- Publish the SIP ports `5060-5069/udp` (and `/tcp`) and the RTP range with identical host and container
  ports. `rtp_port_min`/`rtp_port_max` must match exactly what you publish. diago uses even RTP ports with
  RTCP on port+1, and `rtp_port_max` is exclusive, so `40000-40100` gives 50 RTP/RTCP pairs.
- Set `network.external_ip` (or let STUN discover it) so the SIP Contact and SDP carry your public address.
- `network.livekit_service_url` must be byte-identical to the value in the homeserver's well-known
  (`org.matrix.msc4143.rtc_foci`), with no trailing slash.
- Set `stop_grace_period` to at least 20s so active calls are hung up and SIP unregisters before the
  container is killed.
- lk-jwt-service must trust the bridge ghosts: set `LIVEKIT_FULL_ACCESS_HOMESERVERS` to include your
  homeserver (or `*`).

## Login

Start a chat with the bridge bot and send `!sip login`. Answer each prompt as its own message (the
password may contain spaces): SIP username, password, registrar (`host:port`, port defaults to 5060),
transport (`udp` default; `tcp` is accepted but untested), and the account's phone number. Use the
registrar values from your provider or your ATA config; for JMP the SIP username is not your phone
number. The bridge registers during login, so a wrong password is reported immediately.

**Warning:** registering competes with any existing ATA or desk phone on the same account. JMP does not
officially support two registrations, so calls may stop reaching the other device.

Then `!sip start-chat <number>` creates an unencrypted room with that number's ghost.

## Calls

- An incoming call to your number rings in the room for the caller's number (created on demand, always
  unencrypted), and the ringing reaches all your Matrix clients (Element Web, Element X, Element Call).
  Answer on any one: the bridge answers the SIP call and the other clients stop ringing.
- **Decline** in Matrix sends `603 Decline` to the caller. **No answer** within `ring_timeout_seconds`
  (or the caller hanging up) leaves a "Missed call" notice and lets the provider's voicemail take over.
- **Busy:** only one call at a time per account; a second call gets `486 Busy Here` and a "line busy" notice.
- **Encrypted rooms are refused:** if the number's room has been encrypted, the call is rejected to
  voicemail with an explanatory notice (an encrypted call would be silent).
- **Withheld or anonymous callers** are rejected (to voicemail); there is no shared "Unknown caller" room.
- Calls need PCMU (G.711 u-law) from the provider; calls whose offer lacks it are rejected with `488` and a notice.
- After a call, the room gets a "Call · 1m 20s" notice (or "Declined call", "Missed call").
- Known limits: Element Web stops ringing after 120 s even with a longer `ring_timeout_seconds`;
  Sable on Android only stops ringing on answer when the bridge can send read receipts as you, which needs
  double puppeting; no call transfer, hold or video. Sable web (and sometimes Sable Android) keeps
  ringing after you answer or decline on another device, until the ring lifetime (`ring_timeout_seconds`)
  runs out. That's client behaviour; the bridge already sends every signal it can (your membership,
  read receipt, membership clear and ring redaction).

### Outgoing calls

Dial the number from the SIP username's account (JMP shows your real number as caller ID). Two ways:

- **Press call in the number's room** (Element X, Sable, Element Web/Call). Create the room first with
  `!sip start-chat <number>`, or reuse one from an earlier call. The ghost's tile appears within a
  second or two and you hear ringback (the carrier's early media if it sends any, otherwise a generated
  ringback tone). The bridge recognises the call because your client announces it (a call notification plus
  your freshly published call membership), see `outbound_trigger`. A call you join rather than start never
  dials, and neither does a stale call left over from before a bridge restart.
- **`!sip call <number>`** in the management room, or `!sip call` inside a number's room. Your devices ring
  from the number's ghost exactly like an incoming call; answer and the bridge dials. Declining or letting
  it ring out places no call. Use this if your client does not send a call notification.
- **`!sip hangup`** ends the active call from either direction (hanging up in your client works too).
- **`!sip dtmf <digits>`** (0-9 * # A-D, up to 32, spaces ignored) sends keypad tones while a call is
  connected, in or out, for IVRs and PINs. Send it in the call's room or the management room. The bridge
  redacts your command message afterwards (`dtmf_redact_command`, the bot needs permission to redact) and
  never logs or traces the digits. The phone provider must support RFC 4733 `telephone-event` (JMP does).
- **Number policy:** numbers go through the same normalisation as `start-chat` (`default_country_code`).
  Short codes and emergency numbers (911 and similar) are always refused: use a real phone. The number must
  start with one of `dial_allowed_prefixes` (default `+1` only), and your own number is refused unless
  `allow_self_dial` is set. Only one call at a time per account across both directions.
- **Notices** in the room: `Outgoing call · 1m 20s` when answered, `Cancelled call` if you hung up before
  the answer, `Call failed: 486 Busy Here` (or `603 Decline`, `no answer`, `the provider did not respond`,
  ...) with a busy or reorder tone, and `Call not placed: ...` when a rule or a busy line stopped it. The
  ghost also declines your call notification so your client can close its call screen.
- Known limits: only PCMU, no in-band DTMF fallback, no PSTN dialpad or third-party lookup, no hold,
  transfer or video. Whether your client's call screen closes by itself when the call fails differs by
  client (Element X may stay in an empty call; leave it manually). Sable and some clients that stay in a
  call after hang-up cannot start a second call until they leave; clients that send no call notification
  need `!sip call` or `outbound_trigger: membership` (which may misfire if a second device of yours joins
  an empty call). A ghost membership left over from a bridge crash makes your client join an existing
  call and send no notification, so use `!sip call` then. Calls to your own number, or to a number that
  rings back to your account, are refused or loop: see `allow_self_dial`.
- **Element X: pressing call in a number's room may ring forever, then hang up after 30 s**, even
  though the bridge has dialled, the far end answered and audio is flowing. This is an Element Call bug
  (Element X embeds Element Call for its call screen). When you start a call in a DM it waits for the
  *first other room member* to join, and that can be the bridge bot (`@sipbot`) instead of the number's
  ghost. Element Call already has an open TODO to ignore functional members such as bridge bots when
  picking that recipient:
  [`CallNotificationLifecycle.ts` line 128](https://github.com/element-hq/element-call/blob/8be229a438dc363972bdae5f6198607998a851d8/src/state/CallViewModel/CallNotificationLifecycle.ts#L128)
  (`// TODO: Respect io.element.functional_members`). bridgev2 already lists the bot in
  `io.element.functional_members`, so no bridge change is planned. Until it's fixed upstream, use
  `!sip call <number>` from Element X (you answer the bridge's ring, which Element X handles as joining an
  existing call), or place the call from Sable, which doesn't wait for a specific recipient.

## Embedded mode (jmpchat-matrix-bridge)

The call core also runs inside [jmpchat-matrix-bridge](https://github.com/williamkray/jmpchat-matrix-bridge) (build tag
`sipcalls`, included in its default images; off unless `network.calls.enabled: true`), so a Cheogram
number's texts and calls share one Matrix room and one ghost.
Calls are configured under `network.calls` and the SIP account is set per login with `sip-login`,
`sip-logout` and `sip-status`; `call`, `hangup` and `dtmf` work as here. Full host contract and user
guide: [docs/EMBEDDING.md](docs/EMBEDDING.md) (design: [docs/PHASEJ-SPEC.md](docs/PHASEJ-SPEC.md)).

- **Never register one SIP account from two bridges at once** (this standalone bridge and the embedded
  mode): JMP forks calls to every Contact. Stop or log out one before using the other.
- Only Cheogram 1:1 rooms carry calls (no group MMS, no MUC). Encrypted portals are refused for calls.
- **Element X:** pressing call in a number's room may ring forever then hang up after 30 s (upstream
  Element Call bug, see
  [`CallNotificationLifecycle.ts` line 128](https://github.com/element-hq/element-call/blob/8be229a438dc363972bdae5f6198607998a851d8/src/state/CallViewModel/CallNotificationLifecycle.ts#L128)).
  Use `call <number>` there, or Sable. Documented only, no workaround.
- In that build, setting `OTEL_EXPORTER_OTLP_ENDPOINT` also enables metrics.

## Bridge states

- `CONNECTED`: the REGISTER succeeded, and the bridge re-registers every `register_refresh_seconds`.
- `BAD_CREDENTIALS`: the registrar rejected the credentials (401/403/404/407). The bridge keeps retrying
  every 5 minutes, but you should fix the password and log in again.
- `TRANSIENT_DISCONNECT`: registration failed for another reason (timeout, network, server error). The
  bridge retries with backoff.

## Tracing and metrics (OpenTelemetry)

Tracing and metrics are optional and **off by default**. They are enabled purely by environment
variables; there is no config-file setting. When no endpoint is set, nothing is installed (no exporter,
goroutines or sockets), so there is no overhead. Export problems are logged as rate-limited warnings and
never stop or block the bridge. The design is in [docs/TELEMETRY-SPEC.md](docs/TELEMETRY-SPEC.md).

**Enabling.** Traces and metrics are decided independently. Each turns on when its endpoint is set:
`OTEL_EXPORTER_OTLP_ENDPOINT` enables **both**, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or
`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` enables only that signal. Always include the `http://` or
`https://` scheme.

| Variable | Notes |
|----------|-------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Base endpoint for both signals. For `http/protobuf` the exporters append `/v1/traces` and `/v1/metrics`. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Per-signal endpoint, used as-is. Enables only that signal when the base is unset. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` / `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` / `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | `grpc` or `http/protobuf`. **Set this explicitly.** The default is `http/protobuf` per the OTel spec and the bridge does not guess from the port. Any other value (e.g. `http/json`) logs a warning and disables that signal. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `_TIMEOUT`, `_COMPRESSION`, `_CERTIFICATE`, `_INSECURE`, ... | Handled by the OTLP exporters themselves (also in `TRACES_` / `METRICS_` variants). |
| `OTEL_SDK_DISABLED=true` | Disable everything without removing the rest of the config. An unparseable value logs a warning and is treated as false. |
| `OTEL_TRACES_EXPORTER` | Only `otlp` (or unset) is supported; `none` disables traces; other values warn and disable. Setting `otlp` alone does not enable tracing. |
| `OTEL_METRICS_EXPORTER` | Same rules for metrics (`prometheus` and `console` are not supported: they warn and disable metrics). |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Standard sampling, e.g. `parentbased_traceidratio` / `0.25`. |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT` | Metric export period and timeout in ms (defaults 60000 / 30000). |
| `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` | `cumulative` (default), `delta` or `lowmemory`, case-insensitive. Other values warn and use cumulative. **Use `delta` with Datadog.** |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Standard resource settings; these override the `DD_*` fallbacks below. |
| `OTEL_BSP_SCHEDULE_DELAY`, other `OTEL_BSP_*` | Batch span export tuning, e.g. `1000` (ms) for faster delivery while testing. |

`OTEL_LOGS_EXPORTER` is not supported: logs stay on stdout as JSON, correlated by ids (see below).

Resource defaults: `service.name` is `mrtc-sip-bridge` and `service.version` is the bridge version.
`DD_SERVICE`, `DD_VERSION` and `DD_ENV` (sets both `deployment.environment.name` and
`deployment.environment`) are used as fallbacks, and the `OTEL_*` variables above take precedence over
them. `git.commit.sha` and `git.repository_url` come from the build metadata;
`DD_GIT_COMMIT_SHA` / `DD_GIT_REPOSITORY_URL` override them.

**Match the protocol to the port.** Port 4317 is gRPC and port 4318 is HTTP. If the configured protocol
and port disagree (for example `http/protobuf` against `:4317`), the bridge logs a startup warning and you
will see export errors in the log. On success it logs one `OpenTelemetry tracing enabled` and/or
`OpenTelemetry metrics enabled` line with the protocol and `scheme://host:port` (never the path or headers).

### Generic OpenTelemetry collector

```bash
# gRPC (collector port 4317)
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
OTEL_EXPORTER_OTLP_PROTOCOL=grpc

# or HTTP (collector port 4318)
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf

OTEL_SERVICE_NAME=mrtc-sip-bridge
OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=prod

# optional: traces only, or metrics only
# OTEL_METRICS_EXPORTER=none
# OTEL_TRACES_EXPORTER=none
```

The collector needs an `otlp` receiver (grpc and/or http) and, for metrics, a pipeline with an exporter
for your metrics backend (for example `prometheus` or `prometheusremotewrite`). Cumulative temporality,
the default, suits Prometheus.

### Datadog Agent (OTLP ingest)

Enable OTLP ingest on the Agent (APM must be enabled, which is the default; set `DD_APM_ENABLED=true` to
be explicit). Metrics arrive on the same OTLP ports as traces:

```bash
DD_API_KEY=<your key>
DD_SITE=datadoghq.com
DD_APM_ENABLED=true
DD_OTLP_CONFIG_RECEIVER_PROTOCOLS_GRPC_ENDPOINT=0.0.0.0:4317
DD_OTLP_CONFIG_RECEIVER_PROTOCOLS_HTTP_ENDPOINT=0.0.0.0:4318
```

Then point the bridge at it (`DD_AGENT_HOST` alone does nothing) and ask for **delta** temporality, which
is what Datadog recommends for OTLP metrics:

```bash
# gRPC
OTEL_EXPORTER_OTLP_ENDPOINT=http://datadog-agent:4317
OTEL_EXPORTER_OTLP_PROTOCOL=grpc
# or HTTP
# OTEL_EXPORTER_OTLP_ENDPOINT=http://datadog-agent:4318
# OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf

OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta
DD_SERVICE=mrtc-sip-bridge   # optional
DD_ENV=prod                  # optional
DD_VERSION=0.1.0             # optional
```

### What is traced

Phase 1, 2 and 3 coverage:

- **Bridge start:** a zero-length `bridge.start` span at startup (handy as a smoke test), with the
  bridge version and a few config summary booleans.
- **Login, connect and disconnect:** `bridge.login.submit` (containing `sip.ua.start` and the first
  `sip.register`), `bridge.connect`, `bridge.disconnect` / `bridge.logout` (with `sip.unregister`).
- **SIP registration:** every initial or full REGISTER and every failed refresh gets a `sip.register`
  span (kind, attempt, registrar address, status code, outcome, next
  retry delay, masked user). **Successful refreshes (every 25 s by default) create no span**; they are
  visible only as metrics.
- **MatrixRTC event ingest:** a `matrixrtc.ingest` span for each call.member / rtc.member / rtc.slot /
  notification / decline event that is routed to a login. Dropped events (ghost echoes, the bot, unknown rooms) create no span but
  are counted in `mrtcsip.matrixrtc.events{routed=false}`.
- **MatrixRTC 2.0:** under the call's setup and teardown spans, child spans `matrixrtc.plan.choose` (which
  format and token endpoint, and why), `matrixrtc.slot.open`, `matrixrtc.member.publish`,
  `matrixrtc.member.leave` and `matrixrtc.ring.withdraw`. The delayed-leave restarts (every 7 s per call)
  create no spans, only the metric below. Attributes are closed enums (`matrixrtc.plan.format`,
  `matrixrtc.plan.reason`, `matrixrtc.slot.action`, `rtc.token.endpoint`, ...), see `docs/TELEMETRY-SPEC.md`.
- **Identity resolution:** a `bridge.resolve_identifier` span for phone-number lookups such as
  start-chat, with the local and remote E.164 numbers.
- **Bot commands:** `cmd.<name>` spans (the command name only, never its arguments).
- **Outbound Matrix HTTP:** calls made while handling traced work appear as child spans named like
  `METHOD /_matrix/client/v3/rooms/*/state/*/*`, with ids replaced by `*`. Requests with no traced
  context get no span.

- **Inbound calls (Phase 2):** one `call.inbound` trace per incoming call, rooted at the INVITE and
  ending at ACK (or after the failure branch for unanswered calls). Its children are `call.resolve_room`,
  `matrixrtc.membership.publish`, `matrixrtc.ring.send`, `rtc.join` (with `rtc.openid_token`,
  `rtc.jwt_fetch` for `POST /sfu/get`, `rtc.livekit.connect`, `rtc.track.publish`), `call.ring`,
  `call.answer`, `media.start`, and for unanswered calls `matrixrtc.membership.clear`,
  `matrixrtc.ring.redact`, `call.notice.send`. Events on `call.inbound` mark the SIP responses sent
  (`sip.response.sent`) and `sip.cancel.received`.
- **After answer:** `call.teardown` (linked root; children `sip.bye`, `rtc.leave`, `media.stop`,
  `matrixrtc.membership.clear`, `matrixrtc.ring.redact`, `call.notice.send`), plus short linked roots
  `call.owner.left`, `call.media.degraded` (loss over 5 % or no SIP RTP for 5 s) and
  `call.membership.refresh`. `call.session` is a zero-length summary span written at the end of every
  call (outcome, duration, codec, RTP counters and jitter, end reason, who hung up) that links to the
  setup and teardown traces.
- `matrixrtc.ingest` spans gain `matrixrtc.routed_to` (`call` or `none`) and `call.id`.
- **Outbound calls (Phase 3):** one `call.outbound` span per outgoing call, a linked root (linked to the
  `cmd.call` span or to the `matrixrtc.ingest` span of the owner's trigger event) that ends at ACK or after
  the failure branch. Attributes: `call.id`, `call.direction=outbound`, `call.trigger` (`command`,
  `owner_membership`), `call.remote.e164`, `call.local.e164`, `matrix.room_id`, `matrix.ghost_id`,
  `matrix.owner_id`, `sip.call_id`, the final `sip.response.status_code`, `call.outcome`, `call.end_reason`;
  event `sip.ack.sent`. Children: `sip.invite` (INVITE to final response, events
  `sip.response.received` with the status code and `sip.auth.challenge`), `call.ringback`
  (first 18x to answer or failure, `sip.early_media`), `media.start`, `matrixrtc.decline.send`
  (the ghost's decline of your notification), `call.resolve_room`, `matrixrtc.membership.publish` /
  `.clear`, `rtc.join`, `call.notice.send`, and on the command path `matrixrtc.ring.send`, `call.ring`
  and `matrixrtc.ring.redact` as for inbound. `call.session` gets `call.direction=outbound`. Remote SIP
  reason phrases are scrubbed on `sip.invite` and never reach notices.
- **Commands:** `cmd.call`, `cmd.dtmf` and `cmd.hangup` are root spans with `cmd.name` only; `cmd.dtmf`
  adds `dtmf.digit_count` (the number of digits, never the digits).
- **Dialpad widget:** `widget.session` (the OpenID session exchange; child `widget.openid.verify`, with
  the userinfo HTTP call under it), `widget.call`, `widget.dtmf`, `widget.hangup` and (when the host offers
  them) `widget.prefs`, `widget.chat` and `widget.contact` are root spans for the
  matching API requests (`outcome`, `matrix.room_id`, `matrix.sender`; `widget.call` adds the dialled
  `call.remote.e164`, `widget.call.form` and, when a name was sent, `widget.contact.saved`, and
  `call.outbound` links to it; `widget.chat` and `widget.contact` add `call.remote.e164` and result flags,
  never the contact name; `widget.dtmf` records only
  `dtmf.digit_count`). `bridge.widget.sweep` covers an installer sweep (`trigger`, `rooms.checked`,
  `rooms.installed`, `rooms.updated`, `rooms.failed`); a periodic pass only gets a span when it found rooms
  it had not seen. State polling, number normalisation and static files get no spans, only metrics. Widget
  keys, sessions, OpenID tokens and digits are never recorded.
- **Dialpad app (PWA):** `widget.device.enroll`, `widget.device.session`, `widget.device.confirm` and
  `widget.device.forget` are root spans for the device API (`widget.device.id`, `matrix.sender` once known,
  `outcome`; the session span adds `widget.device.token_state`). The commands `cmd.dialpad_link`,
  `cmd.dialpad_links` and `cmd.dialpad_revoke` record `cmd.name` and an event with the link id and outcome,
  never the name or code. `bridge.widget.device_janitor` exists only for a run that purged or redacted
  something. `widget.call`, `widget.dtmf` and `widget.hangup` gain `widget.client` (`widget`, `pwa`) and,
  for the app, `widget.device.id`. State polling from the app gets no span. Codes, tokens, device names and
  the User-Agent are never recorded.

Background work starts its own trace, linked to the triggering trace where there is one. No span stays
open for the length of a call. Every call span carries `call.id`.

### Metrics

Metrics use the `mrtcsip.` prefix. Attributes are low-cardinality enums only: never phone numbers, MXIDs,
room ids, call ids, SIP usernames or IP addresses. Go runtime metrics are also exported when metrics are on.

| Metric | Type | Attributes | Meaning |
|--------|------|------------|---------|
| `mrtcsip.sip.register.attempts` | counter | `sip.register.kind` (full, refresh, unregister), `outcome` (ok, auth, transient) | Every REGISTER transaction result. |
| `mrtcsip.sip.register.duration` | histogram, `s` | same | REGISTER round trip, digest round included. |
| `mrtcsip.sip.registrations` | gauge | `state` (registering, registered, auth_failed, transient_error) | Active registrations per state. |
| `mrtcsip.matrixrtc.events` | counter | `matrixrtc.kind`, `routed` | Every MatrixRTC event the ingest hook sees. |
| `mrtcsip.bridge.logins` | counter | `outcome` (ok, auth_failed, transient_error, timeout, error) | Login submit results. |
| `http.client.request.duration` | histogram, `s` | method, status code, `server.address`, `url.template`, `error.type` (no `server.port`; the port is on spans only) | Outbound Matrix HTTP calls and lk-jwt-service. |
| `mrtcsip.call.started` | counter | `call.direction` | Calls begun. |
| `mrtcsip.call.active` | up-down counter | `call.direction` | Calls in progress. |
| `mrtcsip.call.ended` | counter | `call.direction`, `call.outcome`, `sip.response.status_class` | Calls finished. |
| `mrtcsip.call.setup.duration` | histogram, `s` | `call.direction`, `call.outcome` | INVITE received until the 200 OK (answered) or the final failure. |
| `mrtcsip.call.ring.duration` | histogram, `s` | `call.direction`, `call.outcome` | Ring sent until it ended. |
| `mrtcsip.call.duration` | histogram, `s` | `call.direction`, `media.codec` | Answered call length. |
| `mrtcsip.rtc.join.duration` | histogram, `s` | `outcome` | One LiveKit join attempt. |
| `mrtcsip.matrixrtc.delayed_leave.restarts` | counter | `outcome` (ok, gone, error) | Restarts of the delayed leave of a sticky membership. |
| `mrtcsip.media.rtp.packets` | counter | `media.direction` (`rx`, `tx`) | SIP-side RTP packets. |
| `mrtcsip.media.rtp.lost`, `mrtcsip.media.rtp.late` | counter | none | Lost and late-dropped inbound RTP packets. |
| `mrtcsip.media.rtp.jitter` | histogram, `s` | none | Inbound RTP jitter, sampled every 5 s. |
| `mrtcsip.media.concealed` | counter | `media.leg` (`sip_to_rtc`, `rtc_to_sip`) | Audio frames concealed with silence. |

Call metrics are recorded once per call or every 5 s, never per packet. The call metrics above also
cover outbound calls with `call.direction=outbound` (`mrtcsip.call.setup.duration` runs from the dial
start to the 200 OK, or from the trigger to the end if unanswered; `mrtcsip.call.ring.duration` runs from
the first 18x to the answer or failure). `sip.response.status_class` is the class of the final status
received (`2xx` for answered, `none` if no INVITE was sent).

| Metric | Type | Attributes | Meaning |
|--------|------|------------|---------|
| `mrtcsip.dtmf.sent` | counter, `{digit}` | `dtmf.method` (`rfc4733`) | DTMF digits sent to the SIP side (counts only, never the digits). |
| `mrtcsip.widget.requests` | counter, `{request}` | `widget.action` (session, state, normalize, call, dtmf, hangup, prefs, chat, contact; app: enroll, device_session, device_confirm, forget), `outcome` (ok, unauthorized, forbidden, rate_limited, invalid, busy, not_ready, error; app: expired, used, reuse), `widget.client` (widget, pwa) | Dialpad widget and app API requests. |
| `mrtcsip.widget.sessions` | gauge, `{session}` | none | Active widget sessions. |
| `mrtcsip.widget.installs` | counter, `{room}` | `outcome` (installed, updated, unchanged, removed, failed) | Widget installer result per room. |
| `mrtcsip.widget.device_links` | gauge, `{link}` | `state` (pending, active) | Dialpad app links, from the store's cached counts (refreshed by the janitor). |
| `mrtcsip.widget.device_events` | counter, `{event}` | `event` (issued, enrolled, rotated, grace_used, reuse_revoked, revoked, expired, orphan_purged, notice_redacted) | Dialpad app device lifecycle. |

### Privacy

Trace and log attributes include **phone numbers (E.164)**, Matrix user, room and event ids, SIP Call-IDs,
the registrar and Contact `IP:port`, timings, status codes and scrubbed error messages. The SIP username
is recorded only masked (`12***78`); it is not part of the bridge login ID, which is
`<E.164 number>@<registrar host>` (bridgev2 logs that ID as-is). **Never recorded:** audio, SDP bodies, raw SIP messages, DTMF digits,
SIP passwords or digest values, Matrix OpenID tokens, LiveKit JWTs, AS/HS tokens, Matrix event content,
OTLP header values, and bot command arguments. URLs and token-shaped strings in error messages are
scrubbed. **Metrics never carry identities.** Anyone with access to your trace backend can still see call
metadata (who called whom, when, for how long), so restrict access accordingly.

### Log correlation

Log lines emitted inside a sampled, traced operation carry four fields, each once:

- `trace_id` (32 lowercase hex characters) and `span_id` (16 lowercase hex characters): the OpenTelemetry
  convention.
- `dd.trace_id` and `dd.span_id`: decimal strings. The trace id is the low 64 bits. This is the Datadog
  convention.

These are the same field names as in jmpchat-matrix-bridge, so one pipeline serves both. The fields appear only
on lines logged inside traced work. Other lines, and all lines when tracing is off, have no trace fields.
Call lines also carry `call_id`, to find a whole call across its several traces.

Correlation needs JSON logs on stdout. In containers, and for Datadog/OTel log correlation, set the stdout
writer to `format: json` (the generated example config uses pretty output), so `trace_id`, `span_id`,
`dd.trace_id` and `dd.span_id` are JSON attributes:

```yaml
logging:
  writers:
    - type: stdout
      format: json
```

Datadog recognises both pairs by default. For the Logs tab service filter to match the traces, set the
container label `com.datadoghq.tags.service` to the same value as `DD_SERVICE` or `OTEL_SERVICE_NAME`.
If the Logs panel shows the fields but no trace link, set the Trace Id attributes to only `trace_id` and
the Span Id attributes to only `span_id` (Logs > Pipelines > Preprocessing for JSON logs).

## License

MIT, see [LICENSE](LICENSE). Third-party components and their licenses (including MPL-2.0 dependencies,
used unmodified) are listed in [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md) and
[docs/CREDITS.md](docs/CREDITS.md).
