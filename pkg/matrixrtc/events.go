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
	// Never sent or read by us, but clients that start a call natively open the slot first, so it gets a
	// power level override (wire spec §1.6, matrix-js-sdk MatrixRTCSession.ensureRtcSlotOpen).
	RTCSlot = event.Type{Type: "org.matrix.msc4143.rtc.slot", Class: event.StateEventType}
	// RTCSlotAlias is the stable-name alias of RTCSlot; power level override only.
	RTCSlotAlias = event.Type{Type: "m.rtc.slot", Class: event.StateEventType}
)

// IngestEventTypes are the types hosts must route into core.Ingest. Order is fixed (tests rely on it).
func IngestEventTypes() []event.Type {
	return []event.Type{CallMember, CallMemberAlias, RTCNotification, RTCNotificationAlias, RTCDecline, RTCDeclineAlias}
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
	CallIDRoom             = ""            // "" = the room call (slot m.call#ROOM)
	SlotIDRoom             = "m.call#ROOM" // slot id of the room call
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
)
