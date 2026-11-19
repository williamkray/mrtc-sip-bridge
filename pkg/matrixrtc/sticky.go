package matrixrtc

import (
	"crypto/rand"
	"encoding/json"
	"fmt"

	"maunium.net/go/mautrix/id"
)

// StickyJoinParams describes the ghost's sticky rtc.member join.
type StickyJoinParams struct {
	UserID     id.UserID
	DeviceID   string
	MemberID   string // member.id, also both sticky keys
	SlotID     string // empty = SlotIDRoom
	Transports []Transport
}

func transportContent(t Transport) map[string]any {
	m := map[string]any{FieldType: t.Type}
	if t.URL != "" {
		m[FieldURL] = t.URL
	}
	if t.ServiceURL != "" {
		m[FieldLiveKitServiceURL] = t.ServiceURL
	}
	if t.Alias != "" {
		m[FieldLiveKitAlias] = t.Alias
	}
	if t.ServerName != "" {
		m[FieldServerName] = t.ServerName
	}
	return m
}

// StickyJoinContent returns the spec §1.1 join content: slot_id, member {user_id, device_id, id,
// membership join}, application {type m.call, intent m.audio, m.call.intent audio}, transports
// {published, can_subscribe (distinct published types)} and both sticky keys (= MemberID).
func StickyJoinContent(p StickyJoinParams) map[string]any {
	slot := p.SlotID
	if slot == "" {
		slot = SlotIDRoom
	}
	published := make([]any, 0, len(p.Transports))
	subscribe := make([]any, 0, len(p.Transports))
	seen := map[string]bool{}
	for _, t := range p.Transports {
		published = append(published, transportContent(t))
		if !seen[t.Type] {
			seen[t.Type] = true
			subscribe = append(subscribe, t.Type)
		}
	}
	return map[string]any{
		FieldSlotID: slot,
		FieldMember: map[string]any{
			FieldUserID:     string(p.UserID),
			FieldDeviceID:   p.DeviceID,
			FieldID:         p.MemberID,
			FieldMembership: MembershipJoin,
		},
		FieldApplication: map[string]any{
			FieldType:       ApplicationCall,
			FieldIntent:     IntentAudioStable,
			FieldCallIntent: IntentAudio,
		},
		FieldTransports: map[string]any{
			FieldPublished:    published,
			FieldCanSubscribe: subscribe,
		},
		FieldStickyKey:         p.MemberID,
		FieldStickyKeyUnstable: p.MemberID,
	}
}

// StickyLeaveContent returns the spec §1.1 leave content (also used for the delayed leave).
func StickyLeaveContent(slotID, memberID string) map[string]any {
	return map[string]any{
		FieldSlotID: slotID,
		FieldMember: map[string]any{
			FieldID:         memberID,
			FieldMembership: MembershipLeave,
		},
		FieldLeaveReason:       map[string]any{FieldCode: LeaveCodeLeave},
		FieldStickyKey:         memberID,
		FieldStickyKeyUnstable: memberID,
	}
}

// SlotOpenContent returns the content of an open room slot. No encryption key: the portal rooms are
// unencrypted (spec §1.2).
func SlotOpenContent() map[string]any {
	return map[string]any{
		FieldStatus:      SlotStatusOpen,
		FieldApplication: map[string]any{FieldType: ApplicationCall},
	}
}

// Slot is a parsed rtc.slot state event.
type Slot struct {
	ID        string // the state key
	Open      bool   // status open and application.type m.call
	Encrypted bool   // content declares an encryption object
}

// ParseSlot parses a KindRTCSlot event. ok=false for other kinds or non-object content.
func ParseSlot(evt Event) (Slot, bool) {
	if evt.Kind != KindRTCSlot {
		return Slot{}, false
	}
	var c struct {
		Status      string `json:"status"`
		Application struct {
			Type string `json:"type"`
		} `json:"application"`
		Encryption json.RawMessage `json:"encryption"`
	}
	if json.Unmarshal(evt.Content, &c) != nil {
		return Slot{}, false
	}
	enc := string(c.Encryption)
	return Slot{
		ID:        evt.StateKey,
		Open:      c.Status == SlotStatusOpen && c.Application.Type == ApplicationCall,
		Encrypted: enc != "" && enc != "null" && enc != "{}",
	}, true
}

// ParseTransports decodes an /rtc/transports response body: {"transports":[...]} (stable) or
// {"rtc_transports":[...]} (unstable). ok=false if neither key holds an array.
func ParseTransports(body []byte) ([]Transport, bool) {
	var b map[string]json.RawMessage
	if json.Unmarshal(body, &b) != nil {
		return nil, false
	}
	for _, key := range []string{FieldTransportsResp, FieldRTCTransportsFlat} {
		raw, present := b[key]
		if !present {
			continue
		}
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			continue
		}
		return parseTransportList(raw), true
	}
	return nil, false
}

// NewMemberID returns a random UUIDv4 in text form.
func NewMemberID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
