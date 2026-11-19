package matrixrtc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	tMember = "0b9c7c1e-5a4d-4a52-9d3f-1f6f2f1e7a10"
	tURL    = "wss://sfu.example.org"
)

func stickyEvt(typ event.Type, sender id.UserID, content string, ts time.Time) Event {
	return Event{Kind: KindOf(typ), Type: typ.Type, Sender: sender, Content: json.RawMessage(content),
		Timestamp: ts, StickyDuration: time.Hour}
}

func TestStickyJoinContentGolden(t *testing.T) {
	got := mustJSON(t, StickyJoinContent(StickyJoinParams{
		UserID: ghost, DeviceID: device, MemberID: tMember, SlotID: SlotIDRoom,
		Transports: []Transport{{Type: "livekit", URL: tURL}, {Type: "m.livekit", URL: tURL}},
	}))
	want := `{"application":{"intent":"m.audio","m.call.intent":"audio","type":"m.call"},` +
		`"member":{"device_id":"` + device + `","id":"` + tMember + `","membership":"join","user_id":"` + string(ghost) + `"},` +
		`"msc4354_sticky_key":"` + tMember + `","slot_id":"m.call#room","sticky_key":"` + tMember + `",` +
		`"transports":{"can_subscribe":["livekit","m.livekit"],"published":[{"type":"livekit","url":"wss://sfu.example.org"},{"type":"m.livekit","url":"wss://sfu.example.org"}]}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// default slot
	c := StickyJoinContent(StickyJoinParams{UserID: ghost, MemberID: "m"})
	if c[FieldSlotID] != SlotIDRoom {
		t.Fatalf("slot = %v", c[FieldSlotID])
	}
	// service-url transport
	c = StickyJoinContent(StickyJoinParams{UserID: ghost, MemberID: "m", Transports: []Transport{{Type: "livekit", ServiceURL: "https://lk.example.org"}}})
	if !strings.Contains(mustJSON(t, c), `"livekit_service_url":"https://lk.example.org"`) {
		t.Fatalf("content = %s", mustJSON(t, c))
	}
}

func TestStickyLeaveAndSlotContent(t *testing.T) {
	got := mustJSON(t, StickyLeaveContent(SlotIDRoom, tMember))
	want := `{"leave_reason":{"code":"leave"},"member":{"id":"` + tMember + `","membership":"leave"},` +
		`"msc4354_sticky_key":"` + tMember + `","slot_id":"m.call#room","sticky_key":"` + tMember + `"}`
	if got != want {
		t.Fatalf("got %s", got)
	}
	if got := mustJSON(t, SlotOpenContent()); got != `{"application":{"type":"m.call"},"status":"open"}` {
		t.Fatalf("slot = %s", got)
	}
}

func TestParseSlot(t *testing.T) {
	tests := []struct {
		name, content string
		typ           event.Type
		want          Slot
		ok            bool
	}{
		{"open", `{"status":"open","application":{"type":"m.call"}}`, RTCSlot, Slot{ID: "m.call#room", Open: true}, true},
		{"alias closed", `{"status":"closed","application":{"type":"m.call"}}`, RTCSlotAlias, Slot{ID: "m.call#room"}, true},
		{"foreign app", `{"status":"open","application":{"type":"x"}}`, RTCSlot, Slot{ID: "m.call#room"}, true},
		{"encrypted", `{"status":"open","application":{"type":"m.call"},"encryption":{"type":"m.per_member"}}`, RTCSlot, Slot{ID: "m.call#room", Open: true, Encrypted: true}, true},
		{"empty", `{}`, RTCSlot, Slot{ID: "m.call#room"}, true},
		{"not json", `x`, RTCSlot, Slot{}, false},
		{"wrong kind", `{}`, RTCMember, Slot{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := stickyEvt(tt.typ, owner, tt.content, testNow)
			e.StateKey = "m.call#room"
			got, ok := ParseSlot(e)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("got %+v %v", got, ok)
			}
		})
	}
}

func TestParseTransports(t *testing.T) {
	got, ok := ParseTransports([]byte(`{"transports":[{"type":"m.livekit","url":"` + tURL + `","server_name":"example.org"},{"type":"livekit","livekit_service_url":"https://lk"}]}`))
	if !ok || len(got) != 2 || got[0] != (Transport{Type: "m.livekit", URL: tURL, ServerName: "example.org"}) || got[1].ServiceURL != "https://lk" {
		t.Fatalf("stable: %+v %v", got, ok)
	}
	got, ok = ParseTransports([]byte(`{"rtc_transports":[{"type":"livekit","url":"` + tURL + `"}]}`))
	if !ok || len(got) != 1 || got[0].URL != tURL {
		t.Fatalf("unstable: %+v %v", got, ok)
	}
	if got, ok = ParseTransports([]byte(`{"transports":[]}`)); !ok || len(got) != 0 {
		t.Fatalf("empty: %+v %v", got, ok)
	}
	for _, bad := range []string{``, `[]`, `{}`, `{"transports":"x"}`} {
		if _, ok := ParseTransports([]byte(bad)); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNewMemberID(t *testing.T) {
	a, b := NewMemberID(), NewMemberID()
	if a == b || len(a) != 36 || a[14] != '4' || strings.Count(a, "-") != 4 {
		t.Fatalf("ids %q %q", a, b)
	}
}

func TestNotificationHelpers(t *testing.T) {
	if NotificationStickyDuration(90*time.Second) != 5*time.Minute || NotificationStickyDuration(4*time.Minute) != 8*time.Minute {
		t.Fatal("sticky duration")
	}
	if got := mustJSON(t, RingWithdrawContent(SlotIDRoom)); got != `{"msc4354_sticky_key":"m.call#room"}` {
		t.Fatal(got)
	}
	// Ring variants.
	r := RingContent(RingParams{Owner: owner, Lifetime: 90 * time.Second, SenderTS: testNow, SlotID: SlotIDRoom, Sticky: true})
	if _, has := r[FieldRelatesTo]; has || r[FieldSlotID] != SlotIDRoom || r[FieldStickyKeyUnstable] != SlotIDRoom || r[FieldNotificationType] != "ring" {
		t.Fatalf("sticky ring = %v", r)
	}
	r = RingContent(RingParams{Owner: owner, MembershipEventID: "$m", SenderTS: testNow})
	if r[FieldSlotID] != "m.call#ROOM" || r[FieldRelatesTo] == nil || r[FieldStickyKeyUnstable] != nil {
		t.Fatalf("legacy ring = %v", r)
	}
	n, ok := ParseNotification(Event{Kind: KindRTCNotification, Content: json.RawMessage(`{"msc4354_sticky_key":"m.call#room"}`)})
	if !ok || !n.Withdrawn || n.SlotID != SlotIDRoom {
		t.Fatalf("withdraw %+v %v", n, ok)
	}
	n, _ = ParseNotification(Event{Kind: KindRTCNotification, Content: json.RawMessage(`{"slot_id":"m.call#room","sender_ts":1,"lifetime":2,"msc4354_sticky_key":"m.call#room","m.mentions":{"user_ids":["@a:b"]}}`)})
	if n.Withdrawn || n.SlotID != SlotIDRoom || n.Lifetime != 2*time.Millisecond {
		t.Fatalf("notification %+v", n)
	}
}

func TestParseStickyMembership(t *testing.T) {
	join := func(mod func(m map[string]any)) string {
		c := StickyJoinContent(StickyJoinParams{UserID: owner, DeviceID: "OWNDEV", MemberID: tMember, SlotID: SlotIDRoom,
			Transports: []Transport{{Type: "livekit", URL: tURL}}})
		if mod != nil {
			mod(c)
		}
		return mustJSON(t, c)
	}
	fresh := testNow.Add(-time.Minute)
	tests := []struct {
		name     string
		typ      event.Type
		content  string
		sender   id.UserID
		ts       time.Time
		dur      time.Duration
		redacted bool
		ok, live bool
	}{
		{"unstable name join", RTCMember, join(nil), owner, fresh, time.Hour, false, true, true},
		{"stable name join", RTCMemberAlias, join(nil), owner, fresh, time.Hour, false, true, true},
		{"legacy slot spelling", RTCMember, join(func(m map[string]any) { m[FieldSlotID] = SlotIDRoomLegacy }), owner, fresh, time.Hour, false, true, true},
		{"foreign slot", RTCMember, join(func(m map[string]any) { m[FieldSlotID] = "m.call#other" }), owner, fresh, time.Hour, false, true, false},
		{"foreign application", RTCMember, join(func(m map[string]any) { m[FieldApplication] = map[string]any{"type": "x"} }), owner, fresh, time.Hour, false, true, false},
		{"leave with reason", RTCMember, mustJSON(t, StickyLeaveContent(SlotIDRoom, tMember)), owner, fresh, time.Hour, false, true, false},
		{"tombstone with slot", RTCMember, `{"slot_id":"m.call#room","msc4354_sticky_key":"` + tMember + `"}`, owner, fresh, time.Hour, false, true, false},
		{"bare tombstone", RTCMember, `{"msc4354_sticky_key":"` + tMember + `"}`, owner, fresh, time.Hour, false, true, false},
		{"mismatched keys", RTCMember, join(func(m map[string]any) { m[FieldStickyKey] = "other" }), owner, fresh, time.Hour, false, false, false},
		{"no sticky key", RTCMember, `{"slot_id":"m.call#room"}`, owner, fresh, time.Hour, false, false, false},
		{"user_id differs from sender", RTCMember, join(nil), ghost, fresh, time.Hour, false, true, false},
		{"expired by duration", RTCMember, join(nil), owner, testNow.Add(-10 * time.Minute), 5 * time.Minute, false, true, false},
		{"within short duration", RTCMember, join(nil), owner, testNow.Add(-2 * time.Minute), 5 * time.Minute, false, true, true},
		{"duration unknown capped to 1h", RTCMember, join(nil), owner, testNow.Add(-2 * time.Hour), 0, false, true, false},
		{"redacted", RTCMember, join(nil), owner, fresh, time.Hour, true, true, false},
		{"unknown fields", RTCMember, join(func(m map[string]any) { m["x.extra"] = 1; m[FieldMember].(map[string]any)["x"] = true }), owner, fresh, time.Hour, false, true, true},
		{"not json", RTCMember, `nope`, owner, fresh, time.Hour, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := stickyEvt(tt.typ, tt.sender, tt.content, tt.ts)
			e.StickyDuration, e.Redacted = tt.dur, tt.redacted
			m, ok := ParseMembership(e, testNow)
			if ok != tt.ok {
				t.Fatalf("ok = %v", ok)
			}
			if !ok {
				return
			}
			if m.Live != tt.live || m.Format != FormatSticky || m.TypeName != tt.typ.Type || m.UserID != tt.sender {
				t.Fatalf("m = %+v", m)
			}
			if m.Live && m.Expires.IsZero() || !m.Live && !m.Expires.IsZero() {
				t.Fatalf("expires = %v live=%v", m.Expires, m.Live)
			}
		})
	}
}

func TestParseStickyMembershipFields(t *testing.T) {
	e := stickyEvt(RTCMember, owner, mustJSON(t, StickyJoinContent(StickyJoinParams{
		UserID: owner, DeviceID: "OWNDEV", MemberID: tMember, SlotID: SlotIDRoom,
		Transports: []Transport{{Type: "livekit", URL: tURL}, {Type: "m.livekit", URL: tURL}}})), testNow)
	m, _ := ParseMembership(e, testNow)
	if m.Key != "sticky\x00"+tMember || m.DeviceID != "OWNDEV" || m.MemberID != tMember || m.SlotID != SlotIDRoom ||
		len(m.Transports) != 2 || m.PreferredLiveKitURL() != tURL || !m.Expires.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("m = %+v", m)
	}
	ids := m.BackendIdentities()
	if len(ids) != 1 || ids[0] != LiveKitIdentity(owner, tMember) {
		t.Fatalf("identities = %v", ids)
	}

	// Flat rtc_transports (Element Call 2025 dialect) is lifted into the published list; a service-url
	// transport gives the legacy identity.
	e = stickyEvt(RTCMemberAlias, owner, `{"slot_id":"m.call#ROOM","member":{"user_id":"`+string(owner)+`","device_id":"D","id":"`+tMember+`"},`+
		`"application":{"type":"m.call"},"rtc_transports":[{"type":"livekit","livekit_service_url":"https://lk.example.org"}],"sticky_key":"`+tMember+`"}`, testNow)
	m, ok := ParseMembership(e, testNow)
	if !ok || !m.Live || len(m.Transports) != 1 {
		t.Fatalf("flat m = %+v ok=%v", m, ok)
	}
	if ids := m.BackendIdentities(); len(ids) != 1 || ids[0] != string(owner)+":D" {
		t.Fatalf("identities = %v", ids)
	}
	if m.PreferredLiveKitURL() != "" {
		t.Fatal("service-url transport must not give a url")
	}

	// m.livekit is preferred over livekit.
	m = Membership{Transports: []Transport{{Type: "livekit", URL: "a"}, {Type: "m.livekit", URL: "b"}}}
	if m.PreferredLiveKitURL() != "b" {
		t.Fatal("preference")
	}
}

func TestParseLegacyMembershipNewFields(t *testing.T) {
	e := Event{Kind: KindCallMember, Type: CallMember.Type, Sender: ghost, StateKey: MemberStateKey(ghost, device), Timestamp: testNow,
		Content: json.RawMessage(mustJSON(t, JoinContent(JoinParams{UserID: ghost, DeviceID: device, RoomID: "!r:s", ServiceURL: "https://lk.example.org",
			CreatedTS: testNow, Expires: time.Hour, MemberID: tMember, LiveKitURL: tURL})))}
	m, ok := ParseMembership(e, testNow)
	if !ok || !m.Live || m.Format != FormatLegacy || m.Key != e.StateKey || m.MemberID != tMember || m.SlotID != SlotIDRoomLegacy ||
		m.TypeName != CallMember.Type || !m.Expires.Equal(testNow.Add(time.Hour)) || len(m.Transports) != 1 ||
		m.Transports[0].ServiceURL != "https://lk.example.org" || m.Transports[0].URL != tURL || m.Transports[0].Alias != "!r:s" {
		t.Fatalf("m = %+v ok=%v", m, ok)
	}
	if ids := m.BackendIdentities(); len(ids) != 2 || ids[0] != LiveKitIdentity(ghost, tMember) || ids[1] != string(ghost)+":"+device {
		t.Fatalf("identities = %v", ids)
	}
	// JoinParams zero values keep today's wire shape.
	c := JoinContent(JoinParams{UserID: ghost, DeviceID: device, RoomID: "!r:s", ServiceURL: "u", CreatedTS: testNow, Expires: time.Hour})
	if c[FieldMembershipID] != string(ghost)+":"+device || strings.Contains(mustJSON(t, c), `"url"`) {
		t.Fatalf("default join = %v", c)
	}
	// Left legacy membership: no transports, legacy identity, not live.
	e.Content = json.RawMessage(`{}`)
	m, _ = ParseMembership(e, testNow)
	if m.Live || !m.Expires.IsZero() || m.MemberID != string(ghost)+":"+device {
		t.Fatalf("left = %+v", m)
	}
	// A non-member kind is rejected.
	if _, ok := ParseMembership(Event{Kind: KindRTCSlot}, testNow); ok {
		t.Fatal("slot parsed as membership")
	}
}

func TestEventConstants(t *testing.T) {
	if !IsRoomSlot("m.call#room") || !IsRoomSlot("m.call#ROOM") || IsRoomSlot("m.call#x") || IsRoomSlot("") {
		t.Fatal("IsRoomSlot")
	}
	if !IsMemberType(RTCMember) || !IsMemberType(RTCMemberAlias) || IsMemberType(CallMember) ||
		!IsSlotType(RTCSlot) || !IsSlotType(RTCSlotAlias) || IsSlotType(RTCMember) ||
		IsMemberType(event.Type{Type: RTCMember.Type, Class: event.StateEventType}) {
		t.Fatal("type helpers")
	}
	if KindRTCMember.String() != "rtc_member" || KindRTCSlot.String() != "rtc_slot" {
		t.Fatal("kind strings")
	}
}
