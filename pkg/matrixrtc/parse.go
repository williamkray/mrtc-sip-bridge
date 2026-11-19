package matrixrtc

import (
	"encoding/json"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// EventKind classifies a MatrixRTC event independent of which type name it arrived under.
type EventKind int

// The known event kinds. KindUnknown is the zero value.
const (
	KindUnknown EventKind = iota
	KindCallMember
	KindRTCNotification
	KindRTCDecline
	KindRTCMember
	KindRTCSlot
)

// String returns a stable lowercase name for logging.
func (k EventKind) String() string {
	switch k {
	case KindCallMember:
		return "call_member"
	case KindRTCNotification:
		return "rtc_notification"
	case KindRTCDecline:
		return "rtc_decline"
	case KindRTCMember:
		return "rtc_member"
	case KindRTCSlot:
		return "rtc_slot"
	default:
		return "unknown"
	}
}

// Event is a host-neutral MatrixRTC event. Content is the raw JSON content (bridgev2 leaves
// Content.Parsed nil for these types, see S3 §1).
type Event struct {
	Kind      EventKind
	Type      string // raw type name as received, alias or unstable
	RoomID    id.RoomID
	EventID   id.EventID
	Sender    id.UserID
	StateKey  string // "" for message events
	IsState   bool
	Timestamp time.Time       // from origin_server_ts (UnixMilli); zero if 0
	Content   json.RawMessage // never nil for a returned event; "{}" if the event had no content

	StickyDuration  time.Duration // evt.Sticky.GetDuration() (clamped to 1 h); 0 = not sticky or unknown
	StickyRemaining time.Duration // evt.Unsigned.StickyDurationTTL (clamped to 1 h); 0 = absent
	Redacted        bool          // evt.Unsigned.RedactedBecause != nil
}

// KindOf maps a type (Type AND Class must match one of events.go) to a kind.
// CallMember/CallMemberAlias -> KindCallMember; RTCNotification/Alias -> KindRTCNotification;
// RTCDecline/Alias -> KindRTCDecline; RTCMember/Alias -> KindRTCMember; RTCSlot/Alias -> KindRTCSlot; anything else (including a known name with the wrong Class) -> KindUnknown.
func KindOf(t event.Type) EventKind {
	switch t {
	case CallMember, CallMemberAlias:
		return KindCallMember
	case RTCNotification, RTCNotificationAlias:
		return KindRTCNotification
	case RTCDecline, RTCDeclineAlias:
		return KindRTCDecline
	case RTCMember, RTCMemberAlias:
		return KindRTCMember
	case RTCSlot, RTCSlotAlias:
		return KindRTCSlot
	default:
		return KindUnknown
	}
}

// FromMatrixEvent converts evt. ok=false if evt is nil or KindOf(evt.Type) is KindUnknown.
// Content: evt.Content.VeryRaw if non-empty; else json.Marshal(evt.Content.Raw) if Raw != nil; else "{}".
// IsState = evt.StateKey != nil; StateKey = *evt.StateKey when set.
func FromMatrixEvent(evt *event.Event) (Event, bool) {
	if evt == nil {
		return Event{}, false
	}
	kind := KindOf(evt.Type)
	if kind == KindUnknown {
		return Event{}, false
	}
	out := Event{
		Kind:    kind,
		Type:    evt.Type.Type,
		RoomID:  evt.RoomID,
		EventID: evt.ID,
		Sender:  evt.Sender,
		IsState: evt.StateKey != nil,

		StickyDuration:  evt.Sticky.GetDuration(),
		StickyRemaining: max(0, min(evt.Unsigned.StickyDurationTTL.Duration, event.MaxStickyDuration)),
		Redacted:        evt.Unsigned.RedactedBecause != nil,
	}
	if evt.StateKey != nil {
		out.StateKey = *evt.StateKey
	}
	if evt.Timestamp != 0 {
		out.Timestamp = time.UnixMilli(evt.Timestamp)
	}
	switch {
	case len(evt.Content.VeryRaw) > 0:
		out.Content = evt.Content.VeryRaw
	case evt.Content.Raw != nil:
		if b, err := json.Marshal(evt.Content.Raw); err == nil {
			out.Content = b
		}
	}
	if out.Content == nil {
		out.Content = json.RawMessage("{}")
	}
	return out, true
}
