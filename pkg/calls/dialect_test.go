package calls

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

const testRTCURL = "wss://sfu.example.org"

// stickyMemberEvent is an owner sticky rtc.member join (stable name unless typ is given) sent at ts.
func stickyMemberEvent(typ mevent.Type, user id.UserID, memberID, slot string, ts time.Time) *mevent.Event {
	raw, _ := json.Marshal(matrixrtc.StickyJoinContent(matrixrtc.StickyJoinParams{
		UserID: user, DeviceID: "EC" + memberID, MemberID: memberID, SlotID: slot,
		Transports: []matrixrtc.Transport{{Type: "livekit", URL: testRTCURL}},
	}))
	return &mevent.Event{
		Type: typ, RoomID: testRoom, ID: id.EventID("$s-" + memberID + strconv.FormatInt(ts.UnixNano(), 10)),
		Sender: user, Timestamp: ts.UnixMilli(), Content: mevent.Content{VeryRaw: raw},
	}
}

func stickyLeaveEvent(typ mevent.Type, user id.UserID, memberID, slot string, ts time.Time) *mevent.Event {
	e := stickyMemberEvent(typ, user, memberID, slot, ts)
	e.Content.VeryRaw, _ = json.Marshal(matrixrtc.StickyLeaveContent(slot, memberID))
	return e
}

// stickyNotifEvent is the production new-format notification: no notification_type, no m.relates_to.
func stickyNotifEvent(sender id.UserID, evID id.EventID, ts time.Time, slot string) *mevent.Event {
	ms := strconv.FormatInt(ts.UnixMilli(), 10)
	return &mevent.Event{
		Type: matrixrtc.RTCNotification, RoomID: testRoom, ID: evID, Sender: sender, Timestamp: ts.UnixMilli(),
		Content: mevent.Content{VeryRaw: []byte(`{"slot_id":"` + slot + `","sender_ts":` + ms +
			`,"lifetime":30000,"m.mentions":{"user_ids":[],"room":true},"msc4354_sticky_key":"` + slot + `"}`)},
	}
}

func withdrawEvent(sender id.UserID, evID id.EventID, ts time.Time, slot string) *mevent.Event {
	return &mevent.Event{
		Type: matrixrtc.RTCNotification, RoomID: testRoom, ID: evID, Sender: sender, Timestamp: ts.UnixMilli(),
		Content: mevent.Content{VeryRaw: []byte(`{"msc4354_sticky_key":"` + slot + `"}`)},
	}
}

func slotEvent(typ mevent.Type, slot string, open bool, ts time.Time) *mevent.Event {
	status := "closed"
	if open {
		status = "open"
	}
	sk := slot
	return &mevent.Event{
		Type: typ, RoomID: testRoom, ID: "$slot", Sender: testOwner, StateKey: &sk, Timestamp: ts.UnixMilli(),
		Content: mevent.Content{VeryRaw: []byte(`{"status":"` + status + `","application":{"type":"m.call"}}`)},
	}
}

func TestStickyProductionReplay(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		now := h.clk.Now()
		h.matrix(slotEvent(matrixrtc.RTCSlot, "m.call#room", true, now))
		if h.dialer.count() != 0 {
			t.Fatal("a slot event triggered a call")
		}
		h.matrix(stickyMemberEvent(matrixrtc.RTCMemberAlias, testOwner, "mem1", "m.call#room", now))
		if h.dialer.count() != 0 {
			t.Fatal("membership alone triggered in ring mode")
		}
		d, ok := h.m.dialectFor(testLogin)
		if !ok || d.TypeName != "m.rtc.member" || d.SlotID != "m.call#room" || d.URL != testRTCURL {
			t.Fatalf("dialect %+v %v", d, ok)
		}
		h.matrix(stickyNotifEvent(testOwner, testNotif, now, "m.call#room"))
		oc := h.dialed()
		_ = oc
		h.m.mu.Lock()
		c := h.m.active[testLogin]
		h.m.mu.Unlock()
		if c == nil || c.ownerFormat != matrixrtc.FormatSticky || c.ownerMem.MemberID != "mem1" || c.ownerNotif != testNotif {
			t.Fatalf("call %+v", c)
		}
		if _, ok := c.devices["sticky\x00mem1"]; !ok {
			t.Errorf("devices %v", c.devices)
		}
	})
}

func TestStickyNotificationFirst(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		now := h.clk.Now()
		h.matrix(stickyNotifEvent(testOwner, testNotif, now, "m.call#ROOM"))
		if h.dialer.count() != 0 {
			t.Fatal("notification alone triggered")
		}
		h.advance(2 * time.Second)
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#ROOM", h.clk.Now()))
		h.dialed()
		d, _ := h.m.dialectFor(testLogin)
		if d.TypeName != string(matrixrtc.RTCMember.Type) || d.SlotID != "m.call#ROOM" {
			t.Errorf("dialect %+v", d)
		}
	})
}

func TestStickyMembershipMode(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound("membership"))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", h.clk.Now()))
		h.dialed()
	})
}

func TestStickyTombstoneLeaveEndsCall(t *testing.T) {
	for _, leave := range []func(time.Time) *mevent.Event{
		func(ts time.Time) *mevent.Event {
			e := stickyMemberEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", ts)
			e.Content.VeryRaw = []byte(`{"msc4354_sticky_key":"mem1"}`) // tombstone
			return e
		},
		func(ts time.Time) *mevent.Event { // explicit leave with leave_reason
			return stickyLeaveEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", ts)
		},
	} {
		run(t, func(t *testing.T) {
			h := newHarness(t, withOutbound(""))
			now := h.clk.Now()
			h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", now))
			h.matrix(stickyNotifEvent(testOwner, testNotif, now, "m.call#room"))
			oc := h.dialed()
			h.ownerJoinedLK("EC")
			h.matrix(leave(h.clk.Now()))
			if oc.hangupCount() != 1 {
				t.Fatalf("hangups %d (devices not removed)", oc.hangupCount())
			}
		})
	}
}

func TestStickyMixedDevices(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		h.matrix(h.ownerMember("TABLET", true))
		h.advance(time.Second)
		now := h.clk.Now()
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", now))
		h.matrix(stickyNotifEvent(testOwner, testNotif, now, "m.call#room"))
		oc := h.dialed()
		h.m.mu.Lock()
		c := h.m.active[testLogin]
		h.m.mu.Unlock()
		if len(c.devices) != 2 || c.ownerFormat != matrixrtc.FormatSticky {
			t.Fatalf("devices %v format %v", c.devices, c.ownerFormat)
		}
		h.matrix(stickyLeaveEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", h.clk.Now()))
		if oc.hangupCount() != 0 {
			t.Fatal("legacy device still present, call must continue")
		}
		h.matrix(h.ownerMember("TABLET", false))
		if oc.hangupCount() != 1 {
			t.Fatalf("hangups %d", oc.hangupCount())
		}
	})
}

func TestStickyWithdrawalCancelsPending(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		now := h.clk.Now()
		h.matrix(stickyNotifEvent(testOwner, testNotif, now, "m.call#room"))
		h.matrix(withdrawEvent(testOwner, "$w", now, "m.call#room"))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", h.clk.Now()))
		if h.dialer.count() != 0 {
			t.Fatal("withdrawn notification triggered")
		}
	})
}

func TestStickyGhostEchoIgnored(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		now := h.clk.Now()
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testGhost, "g1", "m.call#room", now))
		h.matrix(stickyNotifEvent(testOwner, testNotif, now, "m.call#room"))
		if h.dialer.count() != 0 {
			t.Fatal("ghost membership paired with the owner notification")
		}
		if _, ok := h.m.dialectFor(testLogin); ok {
			t.Fatal("ghost membership set the dialect")
		}
	})
}

func TestStickyExpiredNotLive(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		now := h.clk.Now()
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", now.Add(-2*time.Hour)))
		h.matrix(stickyNotifEvent(testOwner, testNotif, now, "m.call#room"))
		if h.dialer.count() != 0 {
			t.Fatal("expired sticky membership counted as live")
		}
		if _, ok := h.m.dialectFor(testLogin); ok {
			t.Fatal("expired membership set the dialect")
		}
	})
}

func TestDialectPerLogin(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound("off"))
		now := h.clk.Now()
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "mem1", "m.call#room", now))
		if _, ok := h.m.dialectFor(core.LoginRef("other")); ok {
			t.Fatal("dialect leaked to another login")
		}
		if _, ok := h.m.dialectFor(testLogin); !ok {
			t.Fatal("dialect not remembered")
		}
		// Slot events are dropped by the manager: no state, no trigger.
		h.matrix(slotEvent(matrixrtc.RTCSlotAlias, "m.call#ROOM", true, now))
		h.matrix(slotEvent(matrixrtc.RTCSlot, "m.call#room", false, now))
		if h.dialer.count() != 0 {
			t.Error("slot event triggered a call")
		}
	})
}
