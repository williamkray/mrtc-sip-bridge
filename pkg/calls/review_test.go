package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
)

// stopAll ends every call so that the call.inbound / call.outbound span is exported.
func stopAll(h *harness) {
	h.m.Stop(context.Background())
	synctest.Wait()
}

// ---- F1: bridge-initiated, auto, no dialect behaves exactly as before for old clients ----

// The pre-change wire shapes (captured from the code before MatrixRTC 2.0) for fixed inputs.
const (
	goldenLegacyJoin = `{"application":"m.call","call_id":"","created_ts":%d,"device_id":"%s","expires":%d,` +
		`"foci_preferred":[{"livekit_alias":"%s","livekit_service_url":"%s","type":"livekit"}],` +
		`"focus_active":{"focus_selection":"multi_sfu","type":"livekit"},"m.call.intent":"audio",` +
		`"membershipID":"%s:%s","scope":"m.room"}`
	goldenLegacyRing = `{"lifetime":%d,"m.call.intent":"audio","m.mentions":{"user_ids":["%s"]},` +
		`"m.relates_to":{"event_id":"%s","rel_type":"m.reference"},"notification_type":"ring","sender_ts":%d,"slot_id":"m.call#ROOM"}`
)

func TestMatrixRTCGoldenLegacyShapes(t *testing.T) {
	// The golden strings are what JoinContent/RingContent produced before the change.
	got := jsonOf(matrixrtc.JoinContent(matrixrtc.JoinParams{UserID: "@ghost:example.org", DeviceID: "DEV", RoomID: "!room:example.org",
		ServiceURL: "https://lk-jwt.example.org", CreatedTS: time.UnixMilli(1700000000000), Expires: 4 * time.Hour}))
	want := fmt.Sprintf(goldenLegacyJoin, 1700000000000, "DEV", 14400000, "!room:example.org", "https://lk-jwt.example.org", "@ghost:example.org", "DEV")
	if got != want {
		t.Errorf("join content changed\n got %s\nwant %s", got, want)
	}
	got = jsonOf(matrixrtc.RingContent(matrixrtc.RingParams{Owner: "@owner:example.org", MembershipEventID: "$member1",
		Lifetime: 30 * time.Second, SenderTS: time.UnixMilli(1700000001000)}))
	want = fmt.Sprintf(goldenLegacyRing, 30000, "@owner:example.org", "$member1", 1700000001000)
	if got != want {
		t.Errorf("ring content changed\n got %s\nwant %s", got, want)
	}
}

func TestInboundNoDialectKeepsLegacyBehaviour(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		// Transports are available, which must not matter without a dialect.
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"))
		sc := h.invite()
		_ = sc

		// Legacy membership: byte-identical to the pre-change content.
		st := h.ghost.stateCalls()
		if len(st) != 1 || st[0].typ != matrixrtc.CallMember {
			t.Fatalf("legacy states %+v", st)
		}
		c := st[0].content
		device := c["device_id"].(string)
		want := fmt.Sprintf(goldenLegacyJoin, c["created_ts"], device, c["expires"], testRoom, testServiceURL, testGhost, device)
		if got := jsonOf(c); got != want {
			t.Errorf("legacy membership differs from the pre-change content\n got %s\nwant %s", got, want)
		}

		// Ring: legacy shape, plain message, not sticky.
		if len(stickyRings(g)) != 0 || len(plainRings(h)) != 1 {
			t.Fatalf("rings sticky=%d plain=%d", len(stickyRings(g)), len(plainRings(h)))
		}
		r := plainRings(h)[0]
		wantRing := fmt.Sprintf(goldenLegacyRing, 30000, testOwner, "$member1", r.content["sender_ts"])
		if got := jsonOf(r.content); got != wantRing {
			t.Errorf("ring differs from the pre-change content\n got %s\nwant %s", got, wantRing)
		}

		// Join: sfu_get, no MSC4195 parameters.
		jp := h.joiner.calls[0]
		if jp.Auth != rtc.AuthSFUGet || jp.MemberID != "" || jp.LiveKitURL != "" || jp.SlotID != "" {
			t.Errorf("join params %+v", jp)
		}

		// The sticky membership names a livekit_service_url transport: every client expects exactly the
		// identity "<ghost>:<device>", the same one the legacy membership implies.
		joins := joinsOf(g)
		if len(joins) != 1 {
			t.Fatalf("joins %d", len(joins))
		}
		raw, _ := json.Marshal(joins[0].content)
		mem, ok := matrixrtc.ParseMembership(matrixrtc.Event{Kind: matrixrtc.KindRTCMember, Type: joins[0].typ.Type, Sender: testGhost,
			Content: raw, Timestamp: h.clk.Now()}, h.clk.Now())
		if !ok {
			t.Fatal("sticky join does not parse")
		}
		ids := mem.BackendIdentities()
		wantID := matrixrtc.LegacyLiveKitIdentity(testGhost, device)
		if len(ids) != 1 || ids[0] != wantID {
			t.Errorf("sticky membership identities %v, want [%s]", ids, wantID)
		}
		if p := h.env.SpanByName("matrixrtc.plan.choose"); obstest.Attr(p, "matrixrtc.plan.reason").AsString() != "bridge_stable" ||
			obstest.Attr(p, "matrixrtc.transports.status").AsString() != "not_needed" {
			t.Error("plan attributes")
		}
	})
}

// What the clients read: the ring is org.matrix.msc4075.rtc.notification with the slot_id of the call
// the client has open (js-sdk MatrixRTCSessionManager default m.call#ROOM; the dialect's spelling once
// known), the legacy membership is org.matrix.msc3401.call.member, and the sticky membership uses the
// owner client's own type name and slot spelling.
func TestRingAndMembershipTypesMatchClients(t *testing.T) {
	cases := []struct {
		name       string
		dialect    mevent.Type // zero = none
		slot       string
		wantRing   string
		wantSticky bool
		wantMember mevent.Type
	}{
		{"no dialect", mevent.Type{}, "", "m.call#ROOM", false, matrixrtc.RTCMemberAlias},
		{"js-sdk dialect", matrixrtc.RTCMember, "m.call#ROOM", "m.call#ROOM", true, matrixrtc.RTCMember},
		{"stable dialect", matrixrtc.RTCMemberAlias, "m.call#room", "m.call#room", true, matrixrtc.RTCMemberAlias},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				var g *fakeRTCGhost
				h := newHarness(t, withRTCGhost(&g))
				if tc.dialect.Type != "" {
					h.matrix(stickyMemberEvent(tc.dialect, testOwner, "ownermem", tc.slot, h.clk.Now()))
				}
				h.invite()
				var ring map[string]any
				var typ mevent.Type
				if tc.wantSticky {
					r := stickyRings(g)
					if len(r) != 1 || len(plainRings(h)) != 0 {
						t.Fatalf("rings sticky=%d plain=%d", len(r), len(plainRings(h)))
					}
					ring, typ = r[0].content, r[0].typ
					if ring[matrixrtc.FieldStickyKeyUnstable] != tc.wantRing {
						t.Errorf("sticky key %v", ring[matrixrtc.FieldStickyKeyUnstable])
					}
				} else {
					r := plainRings(h)
					if len(r) != 1 || len(stickyRings(g)) != 0 {
						t.Fatalf("rings sticky=%d plain=%d", len(stickyRings(g)), len(r))
					}
					ring, typ = r[0].content, r[0].typ
				}
				if typ != matrixrtc.RTCNotification || ring[matrixrtc.FieldSlotID] != tc.wantRing {
					t.Errorf("ring type %v slot %v, want %v %v", typ, ring[matrixrtc.FieldSlotID], matrixrtc.RTCNotification, tc.wantRing)
				}
				if st := h.ghost.stateCalls(); len(st) != 1 || st[0].typ != matrixrtc.CallMember {
					t.Errorf("legacy membership %+v", st)
				}
				if j := joinsOf(g); len(j) != 1 || j[0].typ != tc.wantMember {
					t.Errorf("sticky join %+v, want type %v", j, tc.wantMember)
				}
			})
		})
	}
}

// ---- F1(b): both slot types ----

// Any existing slot that declares encryption refuses the call, even the second type's, and nothing is
// opened before every slot was read.
func TestSlotEncryptedUnderOtherTypeRefusesBeforeOpening(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withFormat("dual"), withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.typed[matrixrtc.RTCSlot.Type+"|m.call#ROOM"] = map[string]any{"status": "open", "application": map[string]any{"type": "m.call"},
				"encryption": map[string]any{"type": "m.per_member"}}
		}))
		sc := h.invite()
		if !sc.has("reject:480") {
			t.Errorf("sip %v", sc.commands())
		}
		if got := g.openedList(); len(got) != 0 {
			t.Errorf("slots opened before the refusal: %v", got)
		}
		if len(g.stickyList()) != 0 || len(h.ghost.stateCalls()) != 0 {
			t.Error("published into a room with an encrypted slot")
		}
		assertNotices(t, h, noticeEncrypted)
	})
}

// A slot that exists under one type only is still opened under the other type.
func TestSlotOpenedUnderMissingType(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		open := map[string]any{"status": "open", "application": map[string]any{"type": "m.call"}}
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.typed[matrixrtc.RTCSlotAlias.Type+"|m.call#room"] = open
			g.typed[matrixrtc.RTCSlotAlias.Type+"|m.call#ROOM"] = open
		}))
		h.invite()
		want := []string{"org.matrix.msc4143.rtc.slot|m.call#room", "org.matrix.msc4143.rtc.slot|m.call#ROOM"}
		if got := g.openedList(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("opened %v, want %v", got, want)
		}
	})
}

// ---- F2: a mirror plan never fails the call because of sticky or slot problems ----

func TestMirrorSlotReadErrorProceeds(t *testing.T) {
	for name, mod := range map[string]func(*fakeRTCGhost){
		"read":  func(g *fakeRTCGhost) { g.stateErr = errors.New("boom") },
		"write": func(g *fakeRTCGhost) { g.slotOpenErr = errors.New("forbidden") },
	} {
		t.Run(name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				var g *fakeRTCGhost
				h := newHarness(t, withOutbound(""), withRTCGhost(&g, mod))
				ownerStickyStart(h, matrixrtc.RTCMember, "m.call#ROOM")
				h.dialed()
				if len(joinsOf(g)) != 1 || len(h.joiner.calls) != 1 {
					t.Fatalf("sticky joins %d, joins %d; ops %v", len(joinsOf(g)), len(h.joiner.calls), h.ops.list())
				}
				stopAll(h)
				failed := false
				for _, s := range h.env.SpansByName("matrixrtc.slot.open") {
					if obstest.Attr(s, "matrixrtc.slot.action").AsString() == "failed" {
						failed = true
					}
				}
				if !failed {
					t.Error("no slot span with action failed")
				}
				if got := h.outAttr("matrixrtc.plan.downgrade"); got != "slot_error" {
					t.Errorf("downgrade attr %q", got)
				}
				if len(h.ghost.stateCalls()) != 0 {
					t.Error("legacy membership published although only the slot failed")
				}
			})
		})
	}
}

func TestMirrorStickyFailureProceedsOnLegacy(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"unsupported": {fmt.Errorf("send: %w", core.ErrUnsupported), "sticky_unsupported"},
		"error":       {errors.New("HTTP 500"), "sticky_error"},
	} {
		t.Run(name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				var g *fakeRTCGhost
				h := newHarness(t, withOutbound(""), withRTCGhost(&g, func(g *fakeRTCGhost) {
					g.stickyErr = func(_ int, c stickyCall) error {
						if matrixrtc.IsMemberType(c.typ) {
							return tc.err
						}
						return nil
					}
				}))
				ownerStickyStart(h, matrixrtc.RTCMember, "m.call#ROOM")
				h.dialed()
				// The call went on: legacy membership published, joined with OpenID, dialled.
				st := h.ghost.stateCalls()
				if len(st) != 1 || st[0].typ != matrixrtc.CallMember {
					t.Fatalf("legacy membership %+v; ops %v", st, h.ops.list())
				}
				if jp := h.joiner.calls[0]; jp.Auth != rtc.AuthSFUGet || jp.MemberID != "" || jp.LiveKitURL != "" {
					t.Errorf("join params %+v", jp)
				}
				if h.ops.index("delayed:cancel") < 0 {
					t.Error("delayed leave of the failed sticky join not cancelled")
				}
				// Ending sends the legacy clear and no sticky leave.
				h.ownerJoinedLK("EC")
				h.dialed().answer()
				synctest.Wait()
				h.matrix(h.ownerMember("OWNERDEV", false))
				synctest.Wait()
				if len(leavesOf(g)) != 0 {
					t.Errorf("sticky leave for a downgraded call: %v", leavesOf(g))
				}
				stopAll(h)
				if got := h.outAttr("matrixrtc.plan.downgrade"); got != tc.reason {
					t.Errorf("downgrade attr %q, want %q", got, tc.reason)
				}
			})
		})
	}
}

// ---- F3: token fallback ----

func TestTokenFallbackOnPreConnectTimeout(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"),
			withJoinSteps(joinStep{err: &rtc.JoinError{Stage: "timeout", Err: context.DeadlineExceeded, BeforeConnect: true}}))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now()))
		h.invite()
		if h.joiner.attempts() != 2 || h.joiner.calls[0].Auth != rtc.AuthMSC4195 || h.joiner.calls[1].Auth != rtc.AuthSFUGet {
			t.Fatalf("attempts %d: %+v", h.joiner.attempts(), h.joiner.calls)
		}
		stopAll(h)
		if got := h.setupAttr("rtc.token.fallback"); got != "true" {
			t.Errorf("rtc.token.fallback = %q", got)
		}
	})
}

// A timeout after the LiveKit connect started is not a token problem.
func TestNoFallbackOnPostConnectTimeout(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"),
			withJoinSteps(joinStep{err: &rtc.JoinError{Stage: "timeout", Err: context.DeadlineExceeded}}))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now()))
		h.invite()
		stopAll(h)
		if h.joiner.calls[1].Auth != rtc.AuthMSC4195 || h.setupAttr("rtc.token.fallback") == "true" {
			t.Errorf("unexpected fallback: %+v", h.joiner.calls)
		}
	})
}

// The fallback is free: the normal two attempts remain afterwards, nothing blocks, the call connects.
func TestTokenFallbackDoesNotConsumeAnAttempt(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"),
			withJoinSteps(joinStep{err: tokenErr()},
				joinStep{err: &rtc.JoinError{Stage: "connect_error", Err: errors.New("ice")}}))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now()))
		sc := h.invite()
		if h.joiner.attempts() != 3 {
			t.Fatalf("attempts %d", h.joiner.attempts())
		}
		a := []rtc.AuthMode{h.joiner.calls[0].Auth, h.joiner.calls[1].Auth, h.joiner.calls[2].Auth}
		if a[0] != rtc.AuthMSC4195 || a[1] != rtc.AuthSFUGet || a[2] != rtc.AuthSFUGet {
			t.Errorf("auth sequence %v", a)
		}
		h.answer(sc, "DEV") // the call connects
	})
}

// After a fallback the login skips MSC4195 for msc4195FailedTTL, then tries it again.
func TestTokenFailureIsRemembered(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"),
			withJoinSteps(joinStep{err: tokenErr()}))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now()))
		sc := h.invite()
		h.answer(sc, "DEV")
		h.matrix(h.ownerMember("DEV", false))
		synctest.Wait()
		n := h.joiner.attempts()

		h.advance(time.Minute)
		sc2 := h.newSIPCall()
		h.m.HandleInvite(testLogin, testLocal, sc2)
		synctest.Wait()
		if h.joiner.attempts() != n+1 || h.joiner.calls[n].Auth != rtc.AuthSFUGet {
			t.Fatalf("second call joined with %+v", h.joiner.calls[n:])
		}
		plans := h.env.SpansByName("matrixrtc.plan.choose")
		last := plans[len(plans)-1]
		if r := obstest.Attr(last, "matrixrtc.plan.reason").AsString(); r != reasonMSC4195Failed {
			t.Errorf("reason %q", r)
		}
		if s := obstest.Attr(last, "matrixrtc.transports.status").AsString(); s != "not_needed" {
			t.Errorf("transports fetched while MSC4195 is paused: %q", s)
		}
		sc2.cancel()
		synctest.Wait()

		h.advance(msc4195FailedTTL)
		h.m.HandleInvite(testLogin, testLocal, h.newSIPCall())
		synctest.Wait()
		if h.joiner.calls[len(h.joiner.calls)-1].Auth != rtc.AuthMSC4195 {
			t.Errorf("MSC4195 not tried again after the memory expired: %+v", h.joiner.calls[len(h.joiner.calls)-1])
		}
	})
}

// ---- F5: all live owner devices ----

func TestTriggerCollectsEveryOwnerDeviceIdentity(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g))
		now := h.clk.Now()
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "devA", "m.call#ROOM", now.Add(-time.Second)))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "devB", "m.call#ROOM", now))
		h.matrix(stickyNotifEvent(testOwner, testNotif, h.clk.Now(), "m.call#ROOM"))
		h.dialed()
		jp := h.joiner.calls[0]
		have := map[string]bool{}
		for _, o := range jp.Owners {
			have[o] = true
		}
		for _, d := range []string{"devA", "devB"} {
			if !have[matrixrtc.LiveKitIdentity(testOwner, d)] {
				t.Errorf("identity of %s missing from %v", d, jp.Owners)
			}
		}
		if len(jp.Owners) != 2 {
			t.Errorf("owners %v", jp.Owners)
		}
	})
}

// ---- owner joins LiveKit under the hashed identity ----

// The owner's client joins LiveKit as hash([owner, member.id]); the identity comes from its membership,
// the (fake, rtc-like) session announces it and the call keeps running past the grace period.
func TestOwnerJoinsUnderHashedIdentity(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		hash := matrixrtc.LiveKitIdentity(testOwner, "ownermem")
		h := newHarness(t, withOutbound(""), withRTCGhost(&g))
		h.joiner.present = []string{hash, "@stranger:example.org:X", "ABCDEF-not-the-owner"}
		ownerStickyStart(h, matrixrtc.RTCMember, "m.call#ROOM")
		h.dialed().answer()
		synctest.Wait()
		s := h.joiner.lastSession()
		if m := s.matchedOwners(); len(m) != 1 || m[0] != hash {
			t.Fatalf("matched owners %v, want only %s", m, hash)
		}
		// The owner is present for the whole call: no grace teardown.
		h.advance(2 * ownerRTCGrace)
		if h.callCount() != 1 {
			t.Fatalf("call ended although the owner is in LiveKit; ops %v", h.ops.list())
		}
		if s.leaveCount() != 0 {
			t.Error("session left")
		}
	})
}

// Without the owner's identity in the membership nothing matches and the grace timer ends the call.
func TestOwnerHashedIdentityNeedsMembership(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		hash := matrixrtc.LiveKitIdentity(testOwner, "ownermem")
		h := newHarness(t, withOutbound(""), withRTCGhost(&g))
		h.joiner.present = []string{hash}
		// A legacy owner device: only the "<owner>:<device>" prefix identity is known.
		h.ownerStart("OWNERDEV")
		h.dialed().answer()
		synctest.Wait()
		if m := h.joiner.lastSession().matchedOwners(); len(m) != 0 {
			t.Errorf("hashed identity matched without a membership naming it: %v", m)
		}
	})
}

// ---- F6: sticky join sends are serialised with the keeper ----

type gateGhost struct {
	core.GhostRTCIntent // only SendSticky is used

	mu      sync.Mutex
	sent    []string
	entered chan struct{} // closed-on-first-entry signal (buffered)
	gate    chan struct{} // the first send blocks on it; nil = no blocking
	first   bool
}

func (g *gateGhost) SendSticky(_ context.Context, _ id.RoomID, _ mevent.Type, content map[string]any, _ time.Duration) (id.EventID, error) {
	g.mu.Lock()
	first := !g.first
	g.first = true
	g.mu.Unlock()
	if first && g.gate != nil {
		g.entered <- struct{}{}
		<-g.gate
	}
	g.mu.Lock()
	g.sent = append(g.sent, content["v"].(string))
	g.mu.Unlock()
	return "$e", nil
}

func (g *gateGhost) sentList() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.sent...)
}

func TestStickyJoinSendsSerialisedWithKeeper(t *testing.T) {
	newCall := func(k *delayKeeper) *call {
		return &call{room: &core.CallRoom{RoomID: testRoom}, plan: plan{typ: matrixrtc.RTCMember}, keeper: k}
	}
	oldJoin := map[string]any{"v": "old"}
	newJoin := map[string]any{"v": "new"}

	// The keeper's re-send is in flight (it read "old") when the fallback publishes "new": "new" lands last
	// and the keeper's stored content ends up "new".
	t.Run("keeper first", func(t *testing.T) {
		k := &delayKeeper{join: oldJoin}
		g := &gateGhost{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
		c := newCall(k)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := k.resendJoin(context.Background(), g, testRoom, matrixrtc.RTCMember); err != nil {
				t.Error(err)
			}
		}()
		<-g.entered // the keeper holds the send lock and is inside its send
		started := make(chan struct{})
		go func() {
			defer wg.Done()
			close(started)
			if _, err := c.sendStickyJoin(context.Background(), g, newJoin); err != nil {
				t.Error(err)
			}
		}()
		<-started
		time.Sleep(20 * time.Millisecond)
		if got := g.sentList(); len(got) != 0 {
			t.Errorf("fallback send overtook the keeper's: %v", got)
		}
		close(g.gate)
		wg.Wait()
		if got := g.sentList(); strings.Join(got, ",") != "old,new" {
			t.Errorf("send order %v, want old,new", got)
		}
		if k.currentJoin()["v"] != "new" {
			t.Errorf("keeper content %v", k.currentJoin())
		}
	})

	// The fallback ran first: the keeper's later re-send carries the new content.
	t.Run("fallback first", func(t *testing.T) {
		k := &delayKeeper{join: oldJoin}
		g := &gateGhost{}
		c := newCall(k)
		if _, err := c.sendStickyJoin(context.Background(), g, newJoin); err != nil {
			t.Fatal(err)
		}
		if err := k.resendJoin(context.Background(), g, testRoom, matrixrtc.RTCMember); err != nil {
			t.Fatal(err)
		}
		if got := g.sentList(); strings.Join(got, ",") != "new,new" {
			t.Errorf("send order %v, want new,new", got)
		}
	})

	// No keeper: a plain send.
	t.Run("no keeper", func(t *testing.T) {
		g := &gateGhost{}
		if _, err := newCall(nil).sendStickyJoin(context.Background(), g, newJoin); err != nil || len(g.sentList()) != 1 {
			t.Fatalf("%v %v", err, g.sentList())
		}
	})
}

// ---- shutdown while joining ----

func TestShutdownWhileJoiningStopsKeeper(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withJoinSteps(joinStep{block: true}))
		sc := h.invite()
		_ = sc
		if h.joiner.attempts() != 1 || len(joinsOf(g)) != 1 || len(g.delayList()) != 1 {
			t.Fatalf("joiner %d sticky joins %d delayed %d", h.joiner.attempts(), len(joinsOf(g)), len(g.delayList()))
		}
		h.advance(matrixrtc.DelayedLeaveRestart) // the keeper is alive and restarting
		if len(g.updateList()) != 1 {
			t.Fatalf("restarts %d", len(g.updateList()))
		}
		h.m.Stop(context.Background())
		synctest.Wait()
		if h.callCount() != 0 {
			t.Fatal("call still registered after Stop")
		}
		// The keeper is gone, the delayed leave was fired, and no explicit leave is needed.
		before := len(g.updateList())
		h.advance(time.Minute)
		if got := len(g.updateList()); got != before {
			t.Errorf("keeper kept restarting after shutdown: %d -> %d", before, got)
		}
		up := g.updateList()
		if last := up[len(up)-1]; last.action != "send" || last.id != "delay1" {
			t.Errorf("last delayed update %+v, want send of delay1", last)
		}
		if len(leavesOf(g)) != 0 {
			t.Errorf("explicit sticky leave although the delayed leave fired: %v", leavesOf(g))
		}
		opsIndex(t, h, "delayed:send", "clear")
	})
}

// Without delayed events the same shutdown leaves explicitly (no keeper to stop).
func TestShutdownWhileJoiningNoDelayedLeavesExplicitly(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) { g.caps[matrixrtc.FeatureDelayed] = false }),
			withJoinSteps(joinStep{block: true}))
		h.invite()
		h.m.Stop(context.Background())
		synctest.Wait()
		if len(leavesOf(g)) != 1 {
			t.Errorf("leaves %d", len(leavesOf(g)))
		}
	})
}

// ---- F8(6): no leave span without a ghost capability ----

func TestNoLeaveSpanWithoutRTCGhost(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.answer(sc, "DEV")
		h.matrix(h.ownerMember("DEV", false))
		synctest.Wait()
		if s := h.env.SpanByName("matrixrtc.member.leave"); s != nil {
			t.Errorf("leave span without a GhostRTCIntent")
		}
	})
}

// ---- F8(3): slot id attribute ----

func TestSlotIDSpanAttributeIsClosedSet(t *testing.T) {
	for in, want := range map[string]string{
		"m.call#room": "m.call#room", "m.call#ROOM": "m.call#ROOM", "": "other", "m.call#evil-" + strings.Repeat("x", 50): "other", "x": "other",
	} {
		if got := matrixrtc.SlotIDLabel(in); got != want {
			t.Errorf("SlotIDLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
