package matrixrtc

import (
	"encoding/json"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
)

func TestKindOf(t *testing.T) {
	tests := []struct {
		name string
		typ  event.Type
		want EventKind
	}{
		{"call member", CallMember, KindCallMember},
		{"call member alias", CallMemberAlias, KindCallMember},
		{"notification", RTCNotification, KindRTCNotification},
		{"notification alias", RTCNotificationAlias, KindRTCNotification},
		{"decline", RTCDecline, KindRTCDecline},
		{"decline alias", RTCDeclineAlias, KindRTCDecline},
		{"call member wrong class", event.Type{Type: CallMember.Type, Class: event.MessageEventType}, KindUnknown},
		{"room message", event.EventMessage, KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KindOf(tt.typ); got != tt.want {
				t.Fatalf("KindOf = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEventKindString(t *testing.T) {
	want := map[EventKind]string{
		KindUnknown: "unknown", KindCallMember: "call_member",
		KindRTCNotification: "rtc_notification", KindRTCDecline: "rtc_decline",
	}
	for k, s := range want {
		if k.String() != s {
			t.Errorf("%d.String() = %q, want %q", k, k.String(), s)
		}
	}
}

func TestFromMatrixEvent(t *testing.T) {
	t.Run("state event with VeryRaw", func(t *testing.T) {
		sk := "@a:example.org_DEVICE"
		evt := &event.Event{
			Type:      CallMember,
			RoomID:    "!room:example.org",
			ID:        "$ev",
			Sender:    "@a:example.org",
			StateKey:  &sk,
			Timestamp: 1700000000123,
			Content:   event.Content{VeryRaw: json.RawMessage(`{"application":"m.call"}`)},
		}
		got, ok := FromMatrixEvent(evt)
		if !ok {
			t.Fatal("ok = false")
		}
		if got.Kind != KindCallMember || got.Type != CallMember.Type || got.RoomID != evt.RoomID ||
			got.EventID != evt.ID || got.Sender != evt.Sender || !got.IsState || got.StateKey != sk {
			t.Fatalf("fields not copied: %+v", got)
		}
		if !got.Timestamp.Equal(time.UnixMilli(1700000000123)) {
			t.Fatalf("Timestamp = %v", got.Timestamp)
		}
		if string(got.Content) != `{"application":"m.call"}` {
			t.Fatalf("Content = %s", got.Content)
		}
	})
	t.Run("message event with only Raw", func(t *testing.T) {
		evt := &event.Event{
			Type:    RTCNotification,
			Content: event.Content{Raw: map[string]any{"lifetime": 30000}},
		}
		got, ok := FromMatrixEvent(evt)
		if !ok {
			t.Fatal("ok = false")
		}
		if got.IsState || got.StateKey != "" || !got.Timestamp.IsZero() {
			t.Fatalf("unexpected state/timestamp: %+v", got)
		}
		if string(got.Content) != `{"lifetime":30000}` {
			t.Fatalf("Content = %s", got.Content)
		}
	})
	t.Run("nil content", func(t *testing.T) {
		got, ok := FromMatrixEvent(&event.Event{Type: RTCDecline})
		if !ok || string(got.Content) != "{}" {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})
	t.Run("unknown type", func(t *testing.T) {
		if _, ok := FromMatrixEvent(&event.Event{Type: event.EventMessage}); ok {
			t.Fatal("ok = true")
		}
	})
	t.Run("nil event", func(t *testing.T) {
		if _, ok := FromMatrixEvent(nil); ok {
			t.Fatal("ok = true")
		}
	})
}

func TestIngestEventTypesOrder(t *testing.T) {
	want := []event.Type{CallMember, CallMemberAlias, RTCNotification, RTCNotificationAlias, RTCDecline, RTCDeclineAlias}
	got := IngestEventTypes()
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	pl := CallMemberPowerLevelTypes()
	if len(pl) != 2 || pl[0] != CallMember || pl[1] != CallMemberAlias {
		t.Fatalf("CallMemberPowerLevelTypes = %v", pl)
	}
	all := CallPowerLevelTypes()
	if len(all) != 4 || all[0] != CallMember || all[1] != CallMemberAlias || all[2] != RTCSlot || all[3] != RTCSlotAlias {
		t.Fatalf("CallPowerLevelTypes = %v", all)
	}
	for _, typ := range all {
		if !typ.IsState() {
			t.Errorf("%s is not a state event type", typ.Type)
		}
	}
}
