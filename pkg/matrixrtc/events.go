package matrixrtc

import (
	"time"

	"maunium.net/go/mautrix/event"
)

// All MatrixRTC event type names used by this project live in this file (CLAUDE.md rule).
// Checked 2026-10-16 against: MSC3401/MSC4143 legacy per-device membership (SessionMembershipData),
// MSC4075 head 4bdec0f963 (rtc.notification; now also contains decline, superseding MSC4310),
// Element Call v0.28.0 / main 8be229a, matrix-js-sdk d0e8162, ruma 09550bab (Element X).
// Details and rationale: docs/spikes/S1-matrixrtc-wire-spec.md §1.1, §2.1, §3.1.
//
// MatrixRTC 2.0 additions (sticky m.rtc.member, slots, transports, delayed events) checked 2026-11-19
// against: MSC4143 (PR head b7cfa8125d), MSC4354 sticky events (head 2df06b0e80), MSC4140 delayed events
// (head 02a1abfbdf), MSC4195 LiveKit transport (head 9259070646), MSC4196 m.call, matrix-js-sdk develop
// 484d22d (Element Call pins 6c8b1b4), element-call main 86ad9bc / v0.28.2, matrix-rust-rtc main 52af3d1,
// ruma main 44fec4f, lk-jwt-service v0.10.0 (bc31e4c), Synapse v1.165.0 (1b0fd0a), mautrix-go v0.33.0.
// Details and rationale: docs/MATRIXRTC2-SPEC.md §0 and §1.
// Class matters: appservice dispatch matches Type AND Class (appservice/eventprocessor.go:128).
var (
	// CallMember is the per-device membership state event. The only membership type Element X
	// understands. Send this one.
	CallMember = event.Type{Type: "org.matrix.msc3401.call.member", Class: event.StateEventType}
	// CallMemberAlias is the ruma alias; listened to and given a power level override, never sent.
	CallMemberAlias = event.Type{Type: "m.call.member", Class: event.StateEventType}
	// RTCNotification is the ring notification (MSC4075 unstable name). Send this one, not the stable alias.
	RTCNotification = event.Type{Type: "org.matrix.msc4075.rtc.notification", Class: event.MessageEventType}
	// RTCNotificationAlias is the ruma alias accepted by Element X; listened to only.
	RTCNotificationAlias = event.Type{Type: "m.rtc.notification", Class: event.MessageEventType}
	// RTCDecline is the decline event (MSC4310, folded into MSC4075). Listen for both names.
	RTCDecline = event.Type{Type: "org.matrix.msc4310.rtc.decline", Class: event.MessageEventType}
	// RTCDeclineAlias is the stable-name alias of RTCDecline.
	RTCDeclineAlias = event.Type{Type: "m.rtc.decline", Class: event.MessageEventType}
	// RTCSlot is the slot state event (MSC4143; state key = slot id, SlotIDRoom for the room call).
	// The ghost reads it (StateEvent) and opens it before publishing a sticky membership (calls.ensureSlots,
	// one state event per slot id and type). Ingest drops slot events: nothing reacts to them. Clients that
	// start a call natively open the slot first, so it also gets a power level override (wire spec §1.6,
	// matrix-js-sdk MatrixRTCSession.ensureRtcSlotOpen).
	RTCSlot = event.Type{Type: "org.matrix.msc4143.rtc.slot", Class: event.StateEventType}
	// RTCSlotAlias is the stable-name alias of RTCSlot; power level override only.
	RTCSlotAlias = event.Type{Type: "m.rtc.slot", Class: event.StateEventType}
	// RTCMember is the sticky per-device membership message event (MSC4143 + MSC4354), js-sdk name.
	RTCMember = event.Type{Type: "org.matrix.msc4143.rtc.member", Class: event.MessageEventType}
	// RTCMemberAlias is the MSC4143 stable name of RTCMember.
	RTCMemberAlias = event.Type{Type: "m.rtc.member", Class: event.MessageEventType}
)

// IngestEventTypes are the types hosts must route into core.Ingest. Order is fixed (tests rely on it).
func IngestEventTypes() []event.Type {
	return []event.Type{CallMember, CallMemberAlias, RTCNotification, RTCNotificationAlias, RTCDecline, RTCDeclineAlias,
		RTCMember, RTCMemberAlias, RTCSlot, RTCSlotAlias}
}

// IsMemberType reports whether t is one of the two sticky rtc.member types (Type and Class).
func IsMemberType(t event.Type) bool { return t == RTCMember || t == RTCMemberAlias }

// IsSlotType reports whether t is one of the two rtc.slot types (Type and Class).
func IsSlotType(t event.Type) bool { return t == RTCSlot || t == RTCSlotAlias }

// IsRoomSlot reports whether s is the room call slot id in either spelling (stable or legacy).
func IsRoomSlot(s string) bool { return s == SlotIDRoom || s == SlotIDRoomLegacy }

// SlotIDLabel is the value of the matrixrtc.slot_id span attribute: the slot id when it is one of the
// two room spellings, else "other" (a slot id is sender-controlled, so it is never recorded as is).
func SlotIDLabel(s string) string {
	if IsRoomSlot(s) {
		return s
	}
	return "other"
}

// CallMemberPowerLevelTypes must be set to power level 0 in portal rooms so the owner and the ghost
// can send memberships (wire spec §1.5).
func CallMemberPowerLevelTypes() []event.Type { return []event.Type{CallMember, CallMemberAlias} }

// CallPowerLevelTypes are all state events set to power level 0 in portal rooms: the call.member types
// plus the slot types, so the owner can start a call from the client's own call button (wire spec §1.5,
// §1.6). Order is fixed (tests rely on it).
func CallPowerLevelTypes() []event.Type {
	return []event.Type{CallMember, CallMemberAlias, RTCSlot, RTCSlotAlias}
}

// Well-known key listing MatrixRTC foci (wire §4.1, §8.2). ruma alias "m.rtc_foci" is not read.
const WellKnownRTCFociKey = "org.matrix.msc4143.rtc_foci"

// Content values (wire §1.3, §2.2).
const (
	ApplicationCall        = "m.call"
	ScopeRoom              = "m.room"
	CallIDRoom             = ""            // "" = the room call (legacy slot m.call#ROOM)
	SlotIDRoom             = "m.call#room" // stable slot id of the room call (MSC4143/4196)
	SlotIDRoomLegacy       = "m.call#ROOM" // js-sdk / Element Call spelling, what legacy events carry
	IntentAudio            = "audio"
	FocusTypeLiveKit       = "livekit"
	FocusSelectionMultiSFU = "multi_sfu"
	NotificationTypeRing   = "ring"

	NotificationTypeNotification = "notification" // non-ringing notification (group rooms)
)

// DefaultMembershipExpiry is `expires` for a fresh membership: a duration relative to created_ts
// (Element Call's DEFAULT_EXPIRE_DURATION, wire §1.3).
const DefaultMembershipExpiry = 4 * time.Hour

// Content field names read or written by this package (wire §1.3, §2.2, §3.1, §4.1).
const (
	FieldApplication       = "application"
	FieldCallID            = "call_id"
	FieldScope             = "scope"
	FieldDeviceID          = "device_id"
	FieldMembershipID      = "membershipID"
	FieldCreatedTS         = "created_ts"
	FieldExpires           = "expires"
	FieldCallIntent        = "m.call.intent"
	FieldFocusActive       = "focus_active"
	FieldFociPreferred     = "foci_preferred"
	FieldType              = "type"
	FieldFocusSelection    = "focus_selection"
	FieldLiveKitServiceURL = "livekit_service_url"
	FieldLiveKitAlias      = "livekit_alias"
	FieldNotificationType  = "notification_type"
	FieldSenderTS          = "sender_ts"
	FieldLifetime          = "lifetime"
	FieldMentions          = "m.mentions"
	FieldMentionsRoom      = "room" // m.mentions.room
	FieldUserIDs           = "user_ids"
	FieldRelatesTo         = "m.relates_to"
	FieldRelType           = "rel_type"
	FieldEventID           = "event_id"
	FieldSlotID            = "slot_id"
	RelTypeReference       = "m.reference"

	FieldMember            = "member"
	FieldUserID            = "user_id"
	FieldID                = "id"
	FieldMembership        = "membership"
	FieldTransports        = "transports"
	FieldPublished         = "published"
	FieldCanSubscribe      = "can_subscribe"
	FieldStickyKey         = "sticky_key"
	FieldStickyKeyUnstable = "msc4354_sticky_key"
	FieldLeaveReason       = "leave_reason"
	FieldCode              = "code"
	FieldStatus            = "status"
	FieldEncryption        = "encryption"
	FieldIntent            = "intent"
	FieldURL               = "url"
	FieldServerName        = "server_name"
	FieldRTCTransportsFlat = "rtc_transports"
	FieldTransportsResp    = "transports"
	MembershipJoin         = "join"
	MembershipLeave        = "leave"
	SlotStatusOpen         = "open"
	SlotStatusClosed       = "closed"
	LeaveCodeLeave         = "leave"
	FocusTypeLiveKitStable = "m.livekit"
	IntentAudioStable      = "m.audio"
)

// Endpoints and request parameters (spec §1.1, §1.4, §1.5, §1.6).
const (
	PathRTCTransports           = "/_matrix/client/v1/rtc/transports"
	PathRTCTransportsUnstable   = "/_matrix/client/unstable/org.matrix.msc4143/rtc/transports"
	PathLiveKitGetToken         = "/_matrix/client/v1/rtc/livekit/get_token"
	PathLiveKitGetTokenUnstable = "/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token"
	QueryStickyDuration         = "org.matrix.msc4354.sticky_duration_ms"
	BodyStickyDuration          = "org.matrix.msc4354.sticky_duration_ms"
	QueryDelay                  = "org.matrix.msc4140.delay"
	PathDelayedEventPrefix      = "/_matrix/client/unstable/org.matrix.msc4140"
)

// Feature flags in GET /_matrix/client/versions unstable_features (spec §1.7).
const (
	FeatureSticky        = "org.matrix.msc4354"
	FeatureDelayed       = "org.matrix.msc4140"
	FeatureRTC           = "org.matrix.msc4143"
	FeatureRTCStable     = "org.matrix.msc4143.stable"
	FeatureLiveKitStable = "org.matrix.msc4195.stable"
)

// Lifecycle durations for sticky memberships (spec §1.1, §1.3).
const (
	MemberStickyDuration  = time.Hour
	MemberStickyRefresh   = 40 * time.Minute
	DelayedLeaveDelay     = 20 * time.Second
	DelayedLeaveRestart   = 7 * time.Second
	NotificationStickyMin = 5 * time.Minute
)
