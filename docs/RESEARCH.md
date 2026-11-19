# Research notes

> Note: the host bridge mautrix-jabber was renamed to jmpchat-matrix-bridge in 2026-10 (v0.6.0); this document keeps the old name.

Facts gathered while planning, with sources. Anything marked **unverified** has to be confirmed
(usually in a spike) before code depends on it.

## JMP.chat SIP accounts

Source: François Marier, "Setting up a JMP SIP account on Asterisk" (2021), plus battlepenguin.com (2021).
Both are old, so re-check everything against a live account.

- Credentials: send `reset sip account` to the `cheogram.com` contact over XMPP. The reply contains a
  numeric username (10 digits) and an auto-generated password (three lowercase words with spaces).
- Registrar/proxy (2021): `jmp.cbcbc7.auth.bandwidth.com`, **UDP port 5008**. Bandwidth is the carrier.
  **Unverified today.** The user's working ATA config is the source of truth.
- Codecs offered: PCMU (ulaw) and G.729. **We only implement PCMU/PCMA**, so G.729 must not be the
  only codec offered.
- DTMF: RFC 4733 telephone-event.
- Outbound caller ID: Bandwidth seems to require the SIP username in the From/caller-ID, and JMP swaps
  in the real number.
- Media source IPs seen in 2021: RTP from ports 5004–5005 to 10001–20000. Signalling/media hosts
  67.231.2.13 and 216.82.238.135. Firewall rules must allow these, or symmetric RTP / NAT pinholes.
- NAT: the Asterisk setup used `rtp_symmetric`, `rewrite_contact` and an external address.
- SIP minutes: battlepenguin (2021) reports a **120 min/month SIP limit**. **Unverified today.**
- **Multiple registrations:** two clients on the same SIP account both rang (first answer won), but JMP
  told the author this is **not officially supported** and may not always work. Avoid depending on it:
  the bridge should be the account's only registered contact. The user's existing ATA doesn't need
  to keep working alongside the bridge. Bridge-issued credentials for extra SIP devices are an optional
  later phase (ADR-007).

## Prior art

- **lhns/matrix-sip-bridge** (Go, bridgev2, Apache-2.0, early stage, about 100 commits).
  github.com/lhns/matrix-sip-bridge
  - Asterisk handles SIP/PSTN. livekit-sip dials into an Asterisk ConfBridge for media. The bridge only
    signals, and its SIP leg gets 200 OK and is then dropped by the dialplan.
  - Matrix side: the ghost publishes MSC3401 `call.member`, rings with MSC4075, handles declines via
    MSC4310, and posts per-call notices ("Missed call", "Outgoing call — 1m 20s").
  - Lessons worth copying:
    - bridgev2 doesn't deliver `call.member` to connectors, so the bridge has its own handling.
    - **Encrypted portal rooms give silent calls** (SFrame). Keep portals unencrypted.
    - Double puppeting is needed, or invites must be accepted by hand.
    - Far-end hangup is detected by polling LiveKit participants. We can do better because we own the
      SIP leg, so BYE comes to us.
    - Optional REGISTER: digest auth, re-register at half the expiry.
  - Worth reading its `pkg/calls` before writing ours: membership JSON, foci fields, ring payload.
    It's Apache-2.0. Attribution rules for borrowed code are in CREDITS.md.
- **MTRNord/sip-bridge-mx**: WIP, SIP-over-WebSocket/WebRTC ↔ MatrixRTC. Different target (WebRTC PBXes).
- **gymnae/mumabridge**: Mumble ↔ Element Call via appservice ghosts. Same "ghost joins LiveKit" pattern.

## MatrixRTC / Element Call

- MatrixRTC is MSC4143. The LiveKit backend is MSC4195. Element Call is the reference client
  (github.com/element-hq/element-call).
- Each deployment needs a LiveKit SFU plus the MatrixRTC auth service (`lk-jwt-service`). The auth
  service swaps a Matrix OpenID token for a LiveKit URL + JWT, and decides the LiveKit room name from
  the Matrix room. **The bridge has to land in the same LiveKit room the clients use**, so go through
  lk-jwt-service with an OpenID token obtained for the ghost (appservices can masquerade with
  `?user_id=`). Don't mint tokens with the LiveKit API secret, because our room name could drift from
  the clients'.
- The first participant's chosen focus/backend applies to the call.
- Known pitfall: Element X `MISSING_MATRIX_RTC_TRANSPORT` on self-hosted setups when transport discovery
  isn't configured (element-call issue #3933). Make sure the deployment works client-to-client first.
- Ring: MSC4075 notification event (`notification_type: "ring"`, references the RTC session). Decline:
  MSC4310. **Exact event types and fields are unverified.** Take them from the MSCs and check against
  current Element Call behaviour (it's AGPL-3.0, so read it but don't copy it, see CREDITS.md).
  Unstable prefixes change.

### MatrixRTC 2.0 (2026-11-19)

Versions and commits checked (MSC4143, MSC4354, MSC4140, MSC4195, MSC4196, matrix-js-sdk, Element Call,
matrix-rust-rtc, ruma, lk-jwt-service v0.10.0, Synapse v1.165.0, mautrix-go v0.33.0) are listed in
`docs/MATRIXRTC2-SPEC.md` §0; wire formats are §1. Facts that changed earlier notes:

- Sticky `m.rtc.member` (MSC4354) replaces per-device state events. Type names and room slot spelling
  differ by client: js-sdk/Element Call `org.matrix.msc4143.rtc.member` + `m.call#ROOM`; matrix-rust-rtc,
  lk-jwt >= 0.10 and the 2026-11-19 production client (sable-next) `m.rtc.member` + `m.call#room`.
- lk-jwt-service >= 0.10 hashes `m.call#room` for every endpoint, including `/sfu/get` (v0.9: `ROOM`).
- LiveKit identity: `/sfu/get` gives `<user>:<device>`; the MSC4195 homeserver `get_token` gives
  `b64(sha256([user, member.id]))`. The JWT carries no attributes.
- Synapse reads only the unstable sticky parameter `org.matrix.msc4354.sticky_duration_ms` and the
  unstable delayed-event paths. mautrix `RTCTransports` reads the wrong key for the stable path.
- Unverified until WP0 (spec §9): AS masquerade through the MSC4512 proxy, delayed events for AS users,
  sticky fields in AS pushes.

## Go libraries

| Purpose | Library | Notes |
|---|---|---|
| SIP UA | `github.com/emiago/sipgo` | REGISTER, INVITE, digest auth. Used by livekit-sip and lhns. |
| (optional) SIP+media | `github.com/emiago/diago` | Higher-level UA on sipgo with RTP. Evaluate in S2. |
| SDP | `github.com/pion/sdp/v3` | |
| RTP | `github.com/pion/rtp`, `github.com/livekit/media-sdk/rtp` | |
| G.711 / Opus | `github.com/livekit/media-sdk` (`g711`, `opus`) | Opus via cgo `gopkg.in/hraban/opus.v2`, needs libopus. 8 kHz↔48 kHz resampling. |
| LiveKit participant | `github.com/livekit/server-sdk-go/v2` | Join room, publish a local audio track, subscribe to remote tracks. |
| Bridge framework | `maunium.net/go/mautrix` v0.32.x bridgev2 | Same as mautrix-jabber. |

## Sources

- https://feeding.cloud.geek.nz/posts/jmp-sip-account-on-asterisk/
- https://battlepenguin.com/tech/moving-my-phone-number-from-google-hangouts-voice-to-an-sip-xmpp-service/
- https://github.com/lhns/matrix-sip-bridge
- https://github.com/MTRNord/sip-bridge-mx
- https://github.com/gymnae/mumabridge
- https://github.com/element-hq/element-call
- https://github.com/element-hq/element-call/issues/3933
- https://docs.livekit.io/telephony/ (LiveKit SIP: REGISTER not supported)
- https://github.com/livekit/sip/pull/774 (proposed REGISTER support)
- https://github.com/livekit/media-sdk
