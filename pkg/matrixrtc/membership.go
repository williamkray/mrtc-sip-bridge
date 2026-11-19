package matrixrtc

import (
	"encoding/json"
	"strings"
	"time"

	"maunium.net/go/mautrix/id"
)

// MemberStateKey builds the per-device membership state key (wire §1.2):
// "_" + userID + "_" + deviceID + "_" + ApplicationCall + CallIDRoom.
func MemberStateKey(userID id.UserID, deviceID string) string {
	return "_" + string(userID) + "_" + deviceID + "_" + ApplicationCall + CallIDRoom
}

// ParseMemberStateKey reverses MemberStateKey the way ruma does (wire §1.2): strip one leading "_"
// (accept keys without it too), the user ID runs up to the first "_" after the first ":", the rest is
// the member id ("<device>_m.call"). ok=false if there is no ":" or no "_" after it.
func ParseMemberStateKey(stateKey string) (userID id.UserID, memberID string, ok bool) {
	s := strings.TrimPrefix(stateKey, "_")
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return "", "", false
	}
	us := strings.IndexByte(s[colon:], '_')
	if us < 0 {
		return "", "", false
	}
	us += colon
	return id.UserID(s[:us]), s[us+1:], true
}

// JoinParams describes the ghost's membership.
type JoinParams struct {
	UserID     id.UserID     // the ghost
	DeviceID   string        // invented, [A-Z0-9]
	RoomID     id.RoomID     // also used as livekit_alias (value ignored by clients, required by ruma)
	ServiceURL string        // lk-jwt-service base URL, goes into foci_preferred[0].livekit_service_url
	CreatedTS  time.Time     // created_ts (UnixMilli)
	Expires    time.Duration // expires (ms), relative to CreatedTS
	// MemberID is membershipID; empty = "<user>:<device>" (legacy default).
	MemberID string
	// LiveKitURL, when non-empty, is added as foci_preferred[0].url (hashed-identity dialect, spec §2).
	LiveKitURL string
}

// JoinContent returns exactly the wire §1.3 content (keys: application, call_id, scope, device_id,
// membershipID = "<user>:<device>", created_ts, expires, m.call.intent = "audio", focus_active
// {type: livekit, focus_selection: multi_sfu}, foci_preferred [{type: livekit, livekit_service_url,
// livekit_alias}]). Never a "url" key (wire §1.3).
func JoinContent(p JoinParams) map[string]any {
	memberID := p.MemberID
	if memberID == "" {
		memberID = string(p.UserID) + ":" + p.DeviceID
	}
	focus := map[string]any{
		FieldType:              FocusTypeLiveKit,
		FieldLiveKitServiceURL: p.ServiceURL,
		FieldLiveKitAlias:      string(p.RoomID),
	}
	if p.LiveKitURL != "" {
		focus[FieldURL] = p.LiveKitURL
	}
	return map[string]any{
		FieldApplication:  ApplicationCall,
		FieldCallID:       CallIDRoom,
		FieldScope:        ScopeRoom,
		FieldDeviceID:     p.DeviceID,
		FieldMembershipID: memberID,
		FieldCreatedTS:    p.CreatedTS.UnixMilli(),
		FieldExpires:      p.Expires.Milliseconds(),
		FieldCallIntent:   IntentAudio,
		FieldFocusActive: map[string]any{
			FieldType:           FocusTypeLiveKit,
			FieldFocusSelection: FocusSelectionMultiSFU,
		},
		FieldFociPreferred: []any{focus},
	}
}

// LeaveContent is the empty membership content (wire §1.4).
func LeaveContent() map[string]any { return map[string]any{} }

// RingParams describes the MSC4075 ring.
type RingParams struct {
	Owner             id.UserID
	MembershipEventID id.EventID    // the ghost's call.member event (m.reference target)
	Lifetime          time.Duration // = ring timeout
	SenderTS          time.Time     // set right before sending
	// SlotID is slot_id; empty = SlotIDRoomLegacy (today's wire value).
	SlotID string
	// Sticky adds msc4354_sticky_key = slot id (the caller adds the sticky duration to the send).
	Sticky bool
}

// RingContent returns exactly the wire §2.2 content: notification_type "ring", sender_ts, lifetime (ms),
// m.mentions.user_ids [owner], m.relates_to {rel_type m.reference, event_id}, m.call.intent "audio",
// slot_id "m.call#ROOM".
//
// An empty MembershipEventID omits m.relates_to (sticky-only rings have no legacy membership event).
func RingContent(p RingParams) map[string]any {
	slot := p.SlotID
	if slot == "" {
		slot = SlotIDRoomLegacy
	}
	c := map[string]any{
		FieldNotificationType: NotificationTypeRing,
		FieldSenderTS:         p.SenderTS.UnixMilli(),
		FieldLifetime:         p.Lifetime.Milliseconds(),
		FieldMentions:         map[string]any{FieldUserIDs: []any{string(p.Owner)}},
		FieldCallIntent:       IntentAudio,
		FieldSlotID:           slot,
	}
	if p.MembershipEventID != "" {
		c[FieldRelatesTo] = map[string]any{
			FieldRelType: RelTypeReference,
			FieldEventID: string(p.MembershipEventID),
		}
	}
	if p.Sticky {
		c[FieldStickyKeyUnstable] = slot
	}
	return c
}

// RingWithdrawContent withdraws a sticky ring: only the sticky key (spec §1.3).
func RingWithdrawContent(slotID string) map[string]any {
	return map[string]any{FieldStickyKeyUnstable: slotID}
}

// NotificationStickyDuration is the sticky duration for a ring: max(2 x lifetime, 5 min) (MSC4354).
func NotificationStickyDuration(lifetime time.Duration) time.Duration {
	return max(2*lifetime, NotificationStickyMin)
}

// Format tells which membership dialect a Membership was parsed from.
type Format int

// The membership formats. The zero value is invalid.
const (
	FormatLegacy Format = 1 // org.matrix.msc3401.call.member state event
	FormatSticky Format = 2 // sticky rtc.member message event
)

// Transport is one entry of foci_preferred (legacy) or transports.published (sticky).
type Transport struct {
	Type       string // "livekit" or "m.livekit"
	URL        string // SFU url (hashed identity dialect)
	ServiceURL string // livekit_service_url (legacy identity dialect)
	Alias      string // livekit_alias (legacy)
	ServerName string // server_name (MSC4195)
}

// Membership is a parsed call.member or rtc.member event.
type Membership struct {
	Format Format
	// Key identifies the device membership: legacy = state key; sticky = "sticky\x00" + sticky key.
	Key      string
	UserID   id.UserID // legacy: from the state key; sticky: the sender
	DeviceID string    // legacy: content device_id, else the member id without "_m.call"; sticky: member.device_id ("" on leave)
	MemberID string    // sticky member.id; legacy membershipID, else "<user>:<device>"
	SlotID   string    // as received; legacy = SlotIDRoomLegacy
	TypeName string    // evt.Type as received (dialect)
	Live     bool      // see ParseMembership
	// Created is created_ts if present, else the event timestamp (the base used for Live). Zero for
	// non-live (empty or foreign) legacy content; the event timestamp for sticky events.
	Created time.Time
	// Expires is when the membership stops being live (legacy: base+expires; sticky: timestamp plus the
	// sticky duration, at most 1 h). Zero if not live.
	Expires    time.Time
	Transports []Transport
}

// ParseMembership parses a KindCallMember or KindRTCMember event. Legacy: Live uses S1's memberIsLive
// rule: base = created_ts if present else evt.Timestamp; expires defaults to DefaultMembershipExpiry.
// ok=false if evt is not a member event, the state key does not parse, or the state key user differs from
// evt.Sender (wire §1.5: the sender is the member). Sticky: see parseStickyMembership.
func ParseMembership(evt Event, now time.Time) (Membership, bool) {
	switch evt.Kind {
	case KindCallMember:
		return parseLegacyMembership(evt, now)
	case KindRTCMember:
		return parseStickyMembership(evt, now)
	}
	return Membership{}, false
}

func parseLegacyMembership(evt Event, now time.Time) (Membership, bool) {
	user, memberID, ok := ParseMemberStateKey(evt.StateKey)
	if !ok || user != evt.Sender {
		return Membership{}, false
	}
	m := Membership{
		Format:   FormatLegacy,
		Key:      evt.StateKey,
		UserID:   user,
		DeviceID: strings.TrimSuffix(memberID, "_"+ApplicationCall+CallIDRoom),
		SlotID:   SlotIDRoomLegacy,
		TypeName: evt.Type,
	}
	m.MemberID = string(user) + ":" + m.DeviceID
	var c map[string]json.RawMessage
	if err := json.Unmarshal(evt.Content, &c); err != nil || len(c) == 0 {
		return m, true // empty or unparseable content means "left"
	}
	if dev, ok := rawString(c[FieldDeviceID]); ok && dev != "" {
		m.DeviceID = dev
		m.MemberID = string(user) + ":" + dev
	}
	if mid, ok := rawString(c[FieldMembershipID]); ok && mid != "" {
		m.MemberID = mid
	}
	m.Transports = parseTransportList(c[FieldFociPreferred])
	if app, _ := rawString(c[FieldApplication]); app != ApplicationCall {
		return m, true
	}
	if cid, present := c[FieldCallID]; present {
		if v, ok := rawString(cid); !ok || v != CallIDRoom {
			return m, true // another slot (non-empty call_id) is not the room call
		}
	}
	if raw, present := c[FieldScope]; present {
		if sc, ok := rawString(raw); !ok || sc != ScopeRoom {
			return m, true
		}
	}
	var base int64
	if !evt.Timestamp.IsZero() {
		base = evt.Timestamp.UnixMilli()
	}
	if v, ok := rawInt(c[FieldCreatedTS]); ok {
		base = v
	}
	exp := DefaultMembershipExpiry.Milliseconds()
	if v, ok := rawInt(c[FieldExpires]); ok {
		exp = v
	}
	m.Live = base+exp > now.UnixMilli()
	if base != 0 {
		m.Created = time.UnixMilli(base)
	}
	if m.Live {
		m.Expires = time.UnixMilli(base + exp)
	}
	return m, true
}

// stickyContent is the rtc.member content as read (unknown fields are ignored).
type stickyContent struct {
	SlotID string `json:"slot_id"`
	Member struct {
		UserID     *string `json:"user_id"`
		DeviceID   string  `json:"device_id"`
		ID         string  `json:"id"`
		Membership string  `json:"membership"`
	} `json:"member"`
	Application struct {
		Type string `json:"type"`
	} `json:"application"`
	Transports struct {
		Published json.RawMessage `json:"published"`
	} `json:"transports"`
	RTCTransports json.RawMessage `json:"rtc_transports"`
	StickyKey     *string         `json:"sticky_key"`
	StickyKeyUn   *string         `json:"msc4354_sticky_key"`
}

// parseStickyMembership parses a sticky rtc.member event (spec §1.1). ok=false when the content is not a
// JSON object, there is no sticky key, or both sticky keys are present and differ. Live requires: not
// redacted, a join (membership absent or "join", member.user_id absent or the sender), application type
// m.call, a room slot id, and now before Expires. A tombstone (only sticky keys) is not live.
func parseStickyMembership(evt Event, now time.Time) (Membership, bool) {
	var c stickyContent
	if json.Unmarshal(evt.Content, &c) != nil {
		return Membership{}, false
	}
	key := ""
	switch {
	case c.StickyKey != nil && c.StickyKeyUn != nil:
		if *c.StickyKey != *c.StickyKeyUn {
			return Membership{}, false
		}
		key = *c.StickyKey
	case c.StickyKey != nil:
		key = *c.StickyKey
	case c.StickyKeyUn != nil:
		key = *c.StickyKeyUn
	default:
		return Membership{}, false
	}
	m := Membership{
		Format:   FormatSticky,
		Key:      "sticky\x00" + key,
		UserID:   evt.Sender,
		DeviceID: c.Member.DeviceID,
		MemberID: c.Member.ID,
		SlotID:   c.SlotID,
		TypeName: evt.Type,
	}
	if m.MemberID == "" {
		m.MemberID = key
	}
	if len(c.Transports.Published) > 0 {
		m.Transports = parseTransportList(c.Transports.Published)
	} else {
		m.Transports = parseTransportList(c.RTCTransports)
	}
	base := evt.Timestamp
	if base.IsZero() {
		base = now
	}
	m.Created = base
	dur := evt.StickyDuration
	if dur <= 0 || dur > MemberStickyDuration {
		dur = MemberStickyDuration
	}
	exp := base.Add(dur)
	live := !evt.Redacted &&
		(c.Member.Membership == "" || c.Member.Membership == MembershipJoin) &&
		(c.Member.UserID == nil || *c.Member.UserID == string(evt.Sender)) &&
		c.Application.Type == ApplicationCall &&
		IsRoomSlot(c.SlotID) &&
		now.Before(exp)
	if live {
		m.Live = true
		m.Expires = exp
	}
	return m, true
}

// parseTransportList decodes a JSON array of transports; anything malformed yields nil. Entries without
// a type are skipped.
func parseTransportList(raw json.RawMessage) []Transport {
	if len(raw) == 0 {
		return nil
	}
	var list []struct {
		Type       string `json:"type"`
		URL        string `json:"url"`
		ServiceURL string `json:"livekit_service_url"`
		Alias      string `json:"livekit_alias"`
		ServerName string `json:"server_name"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var out []Transport
	for _, t := range list {
		if t.Type == "" {
			continue
		}
		out = append(out, Transport{Type: t.Type, URL: t.URL, ServiceURL: t.ServiceURL, Alias: t.Alias, ServerName: t.ServerName})
	}
	return out
}

// BackendIdentities lists the LiveKit identities clients accept for this membership (spec §1.5): a url
// transport gives LiveKitIdentity(user, MemberID), a livekit_service_url transport gives
// LegacyLiveKitIdentity(user, device). A legacy membership without usable transports gets the legacy
// identity. Deduplicated, in transport order.
func (m Membership) BackendIdentities() []string {
	var out []string
	add := func(s string) {
		for _, o := range out {
			if o == s {
				return
			}
		}
		out = append(out, s)
	}
	for _, t := range m.Transports {
		if t.URL != "" {
			add(LiveKitIdentity(m.UserID, m.MemberID))
		}
		if t.ServiceURL != "" {
			add(LegacyLiveKitIdentity(m.UserID, m.DeviceID))
		}
	}
	if len(out) == 0 && m.Format == FormatLegacy {
		add(LegacyLiveKitIdentity(m.UserID, m.DeviceID))
	}
	return out
}

// PreferredLiveKitURL returns the first transport url, preferring type "m.livekit" over "livekit"; ""
// when no transport has a url.
func (m Membership) PreferredLiveKitURL() string {
	for _, t := range m.Transports {
		if t.Type == FocusTypeLiveKitStable && t.URL != "" {
			return t.URL
		}
	}
	for _, t := range m.Transports {
		if t.URL != "" {
			return t.URL
		}
	}
	return ""
}

func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func rawInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	if f, err := n.Float64(); err == nil {
		return int64(f), true
	}
	return 0, false
}

// Notification is a parsed MSC4075 rtc.notification (wire §2.2).
type Notification struct {
	Type        string        // notification_type ("ring", "notification", or "" if absent)
	SenderTS    time.Time     // sender_ts, zero if absent
	Lifetime    time.Duration // lifetime (ms), 0 if absent
	Reference   id.EventID    // m.relates_to.event_id when rel_type is m.reference (or absent), "" otherwise
	Mentions    []id.UserID   // m.mentions.user_ids
	RoomMention bool          // m.mentions.room
	// SlotID is slot_id; for a withdrawal without slot_id it is the sticky key.
	SlotID string
	// Withdrawn is true for a sticky withdrawal: only sticky keys, no type, timestamps or mentions.
	Withdrawn bool
}

// ParseNotification parses a KindRTCNotification event. ok=false for any other kind or unparsable
// content. Never fails on missing optional fields.
func ParseNotification(evt Event) (Notification, bool) {
	if evt.Kind != KindRTCNotification {
		return Notification{}, false
	}
	var c struct {
		Type     string          `json:"notification_type"`
		SenderTS json.RawMessage `json:"sender_ts"`
		Lifetime json.RawMessage `json:"lifetime"`
		Mentions struct {
			UserIDs []string `json:"user_ids"`
			Room    bool     `json:"room"`
		} `json:"m.mentions"`
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relates_to"`
		SlotID      string `json:"slot_id"`
		StickyKey   string `json:"sticky_key"`
		StickyKeyUn string `json:"msc4354_sticky_key"`
	}
	if json.Unmarshal(evt.Content, &c) != nil {
		return Notification{}, false
	}
	n := Notification{Type: c.Type, RoomMention: c.Mentions.Room, SlotID: c.SlotID}
	if sk := c.StickyKeyUn; sk != "" || c.StickyKey != "" {
		if sk == "" {
			sk = c.StickyKey
		}
		if c.Type == "" && len(c.SenderTS) == 0 && len(c.Lifetime) == 0 &&
			len(c.Mentions.UserIDs) == 0 && !c.Mentions.Room {
			n.Withdrawn = true
			if n.SlotID == "" {
				n.SlotID = sk
			}
		}
	}
	if v, ok := rawInt(c.SenderTS); ok && v > 0 {
		n.SenderTS = time.UnixMilli(v)
	}
	if v, ok := rawInt(c.Lifetime); ok && v > 0 {
		n.Lifetime = time.Duration(v) * time.Millisecond
	}
	if c.RelatesTo.RelType == "" || c.RelatesTo.RelType == RelTypeReference {
		n.Reference = id.EventID(c.RelatesTo.EventID)
	}
	for _, u := range c.Mentions.UserIDs {
		n.Mentions = append(n.Mentions, id.UserID(u))
	}
	return n, true
}

// DeclineContent returns the MSC4310 decline content referencing notificationID (wire §3.1):
// {"m.relates_to": {"rel_type": "m.reference", "event_id": notificationID}}. Sent as RTCDecline
// (unstable name), never the alias.
func DeclineContent(notificationID id.EventID) map[string]any {
	return map[string]any{
		FieldRelatesTo: map[string]any{
			FieldRelType: RelTypeReference,
			FieldEventID: string(notificationID),
		},
	}
}

// DeclineTarget returns the m.reference event_id of a KindRTCDecline event. ok=false otherwise.
func DeclineTarget(evt Event) (id.EventID, bool) {
	if evt.Kind != KindRTCDecline {
		return "", false
	}
	var c struct {
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relates_to"`
	}
	if json.Unmarshal(evt.Content, &c) != nil || c.RelatesTo.EventID == "" {
		return "", false
	}
	if c.RelatesTo.RelType != "" && c.RelatesTo.RelType != RelTypeReference {
		return "", false
	}
	return id.EventID(c.RelatesTo.EventID), true
}

// ParseRTCFoci extracts the first livekit_service_url from a .well-known/matrix/client body
// (WellKnownRTCFociKey array, first entry with type "livekit" and a non-empty livekit_service_url,
// returned byte-for-byte: Element Call keys connections by that string). ok=false if absent or malformed.
func ParseRTCFoci(wellKnown []byte) (serviceURL string, ok bool) {
	var body map[string]json.RawMessage
	if json.Unmarshal(wellKnown, &body) != nil {
		return "", false
	}
	var foci []struct {
		Type       string `json:"type"`
		ServiceURL string `json:"livekit_service_url"`
	}
	if json.Unmarshal(body[WellKnownRTCFociKey], &foci) != nil {
		return "", false
	}
	for _, f := range foci {
		if f.Type == FocusTypeLiveKit && f.ServiceURL != "" {
			return f.ServiceURL, true
		}
	}
	return "", false
}
