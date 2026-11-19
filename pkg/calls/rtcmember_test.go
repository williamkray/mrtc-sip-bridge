package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	mevent "maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
)

// ---- helpers ----

func withTransports(url string) harnessOpt {
	return withConfig(func(c *Config) {
		c.Transports = func(context.Context, core.GhostRTCIntent) ([]matrixrtc.Transport, error) {
			return []matrixrtc.Transport{{Type: "m.livekit", URL: url}}, nil
		}
	})
}

func withFormat(f string) harnessOpt {
	return withConfig(func(c *Config) { c.MembershipFormat = f })
}

func withAuth(a string) harnessOpt { return withConfig(func(c *Config) { c.LiveKitAuth = a }) }

func tokenErr() error {
	return &rtc.JoinError{Stage: "token_error", Err: errors.New("get_token: 404")}
}

// memberIDOf returns member.id of a sticky join/leave content.
func memberIDOf(c map[string]any) string {
	m, _ := c[matrixrtc.FieldMember].(map[string]any)
	s, _ := m[matrixrtc.FieldID].(string)
	return s
}

func transportsOf(c map[string]any) []map[string]any {
	t, _ := c[matrixrtc.FieldTransports].(map[string]any)
	var out []map[string]any
	for _, p := range t[matrixrtc.FieldPublished].([]any) {
		out = append(out, p.(map[string]any))
	}
	return out
}

func joinsOf(g *fakeRTCGhost) []stickyCall {
	var out []stickyCall
	for _, s := range g.stickyList() {
		if matrixrtc.IsMemberType(s.typ) && memberOf(s.content)[matrixrtc.FieldMembership] == matrixrtc.MembershipJoin {
			out = append(out, s)
		}
	}
	return out
}

func leavesOf(g *fakeRTCGhost) []stickyCall {
	var out []stickyCall
	for _, s := range g.stickyList() {
		if matrixrtc.IsMemberType(s.typ) && memberOf(s.content)[matrixrtc.FieldMembership] == matrixrtc.MembershipLeave {
			out = append(out, s)
		}
	}
	return out
}

func memberOf(c map[string]any) map[string]any {
	m, _ := c[matrixrtc.FieldMember].(map[string]any)
	return m
}

// ringsOf returns the ring (not withdraw) notification sends: sticky ones from the RTC ghost and plain
// ones from the base fake.
func plainRings(h *harness) []msgCall {
	var out []msgCall
	for _, m := range h.ghost.msgCalls() {
		if m.typ == matrixrtc.RTCNotification {
			out = append(out, m)
		}
	}
	return out
}

func stickyRings(g *fakeRTCGhost) []stickyCall {
	var out []stickyCall
	for _, s := range g.stickyList() {
		if s.typ == matrixrtc.RTCNotification {
			if _, ok := s.content[matrixrtc.FieldNotificationType]; ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func withdrawsOf(g *fakeRTCGhost) []stickyCall {
	var out []stickyCall
	for _, s := range g.stickyList() {
		if s.typ == matrixrtc.RTCNotification {
			if _, ok := s.content[matrixrtc.FieldNotificationType]; !ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func opsIndex(t *testing.T, h *harness, ops ...string) {
	t.Helper()
	last := -1
	for _, o := range ops {
		i := h.ops.index(o)
		if i < 0 || i <= last {
			t.Fatalf("want %v in order; ops %v", ops, h.ops.list())
		}
		last = i
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// ---- bridge-initiated (inbound) ----

// A bridge-initiated call with no dialect yet (nobody has shown the new format) publishes dual in the
// stable names and opens both slot spellings under both slot types, but everything an old client sees is
// unchanged: the ring is the legacy shape (slot m.call#ROOM, m.relates_to, not sticky), the join uses
// sfu_get and the legacy membership is the pre-MatrixRTC-2.0 content. The delayed leave is kept alive;
// ending the call sends the delayed leave before the legacy clear.
func TestInboundDualStable(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g))
		sc := h.invite()
		joins := joinsOf(g)
		if len(joins) != 1 {
			t.Fatalf("joins %d; ops %v", len(joins), h.ops.list())
		}
		j := joins[0]
		if j.typ != matrixrtc.RTCMemberAlias || j.d != matrixrtc.MemberStickyDuration {
			t.Errorf("join %v %v", j.typ, j.d)
		}
		mid := memberOf(j.content)[matrixrtc.FieldID].(string)
		if len(mid) != 36 || j.content[matrixrtc.FieldSlotID] != "m.call#room" || j.content[matrixrtc.FieldStickyKeyUnstable] != mid {
			t.Errorf("join content %v", j.content)
		}
		ts := transportsOf(j.content)
		if len(ts) != 1 || ts[0]["type"] != "livekit" || ts[0][matrixrtc.FieldLiveKitServiceURL] != testServiceURL || ts[0]["url"] != nil {
			t.Errorf("OpenID plan must publish a livekit_service_url transport: %v", ts)
		}
		// Slots: both spellings under both slot types (the stable one first).
		want := []string{"m.rtc.slot|m.call#room", "org.matrix.msc4143.rtc.slot|m.call#room",
			"m.rtc.slot|m.call#ROOM", "org.matrix.msc4143.rtc.slot|m.call#ROOM"}
		if got := g.openedList(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("opened %v, want %v", got, want)
		}
		// Delayed leave: 20 s, sticky 1 h, StickyLeaveContent for the same member id.
		dl := g.delayList()
		if len(dl) != 1 || dl[0].delay != matrixrtc.DelayedLeaveDelay || dl[0].dur != matrixrtc.MemberStickyDuration ||
			dl[0].typ != matrixrtc.RTCMemberAlias || jsonOf(dl[0].content) != jsonOf(matrixrtc.StickyLeaveContent("m.call#room", mid)) {
			t.Errorf("delayed %+v", dl)
		}
		// Legacy membership is the unchanged shape (no member id override, no url).
		st := h.ghost.stateCalls()
		if len(st) != 1 || st[0].content["membershipID"] == mid {
			t.Fatalf("legacy states %+v", st)
		}
		if fp := st[0].content["foci_preferred"].([]any)[0].(map[string]any); fp["url"] != nil {
			t.Errorf("legacy focus %v", fp)
		}
		// Ring: the legacy shape (not sticky, slot m.call#ROOM, legacy event referenced).
		if len(stickyRings(g)) != 0 || len(plainRings(h)) != 1 {
			t.Fatalf("rings sticky=%d plain=%d", len(stickyRings(g)), len(plainRings(h)))
		}
		r := plainRings(h)[0]
		if r.content[matrixrtc.FieldSlotID] != "m.call#ROOM" || r.content[matrixrtc.FieldNotificationType] != "ring" {
			t.Errorf("ring %+v", r)
		}
		if _, has := r.content[matrixrtc.FieldStickyKeyUnstable]; has {
			t.Errorf("ring has a sticky key: %v", r.content)
		}
		if rel, _ := r.content[matrixrtc.FieldRelatesTo].(map[string]any); rel == nil || rel[matrixrtc.FieldEventID] != "$member1" {
			t.Errorf("ring m.relates_to %v", r.content[matrixrtc.FieldRelatesTo])
		}
		opsIndex(t, h, "slot:open:m.call#room", "delayed", "sticky:join", "publish", "ring", "join")
		// Joined with OpenID.
		jp := h.joiner.calls[0]
		if jp.Auth != rtc.AuthSFUGet {
			t.Errorf("auth %v", jp.Auth)
		}

		// Keeper: one restart per 7 s of the scheduled delayed event.
		for i := 1; i <= 3; i++ {
			h.advance(matrixrtc.DelayedLeaveRestart)
			if got := len(g.updateList()); got != i {
				t.Fatalf("after %d intervals: %d restarts", i, got)
			}
		}
		if u := g.updateList()[0]; u.id != "delay1" || u.action != "restart" {
			t.Errorf("restart %+v", u)
		}
		if got := h.env.Sum(t, "mrtcsip.matrixrtc.delayed_leave.restarts", "outcome", "ok"); got != 3 {
			t.Errorf("restart metric = %v", got)
		}
		if n := len(h.env.SpansByName("matrixrtc.delayed_leave.restart")); n != 0 {
			t.Errorf("keeper spans: %d", n)
		}

		// Answer, then the owner leaves: teardown order.
		h.matrix(h.ownerMember("DEV", true))
		h.ownerJoinedLK("DEV")
		if !sc.has("answer") {
			t.Fatalf("not answered: %v", sc.commands())
		}
		h.matrix(h.ownerMember("DEV", false))
		restartsAtEnd := len(g.updateList())
		h.advance(30 * time.Second)
		if got := len(g.updateList()); got != restartsAtEnd {
			t.Errorf("keeper still restarting after teardown: %d -> %d", restartsAtEnd, got)
		}
		opsIndex(t, h, "delayed:send", "clear")
		if len(leavesOf(g)) != 0 {
			t.Errorf("explicit leave sent although the delayed leave fired: %v", leavesOf(g))
		}
		up := g.updateList()
		if last := up[len(up)-1]; last.action != "send" || last.id != "delay1" {
			t.Errorf("last update %+v", last)
		}
		leave := h.env.SpanByName("matrixrtc.member.leave")
		if leave == nil || obstest.Attr(leave, "matrixrtc.member.leave_via").AsString() != "delayed_send" {
			t.Fatalf("leave span %v", leave)
		}
		// The ring was not sticky: nothing to withdraw.
		if w := withdrawsOf(g); len(w) != 0 {
			t.Errorf("withdraw for a plain ring: %+v", w)
		}
		h.assertEnded("answered", "2xx")

		// Privacy: the member id never appears in an Info+ log line.
		for _, line := range strings.Split(h.logs.String(), "\n") {
			if strings.Contains(line, mid) && !strings.Contains(line, `"level":"debug"`) {
				t.Errorf("member id in a non-debug log line: %s", line)
			}
		}
	})
}

// Plan and publish spans carry the attributes that say which dialect was chosen.
func TestInboundPlanSpans(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g))
		sc := h.invite()
		plan := h.env.SpanByName("matrixrtc.plan.choose")
		if plan == nil {
			t.Fatal("no matrixrtc.plan.choose span")
		}
		for k, want := range map[string]string{
			"matrixrtc.plan.format": "dual", "matrixrtc.plan.reason": "bridge_stable", "rtc.token.endpoint": "sfu_get",
			"matrixrtc.transports.status": "not_needed", "matrixrtc.slot_id": "m.call#room",
			"matrixrtc.member.event_type": "m.rtc.member",
		} {
			if got := obstest.Attr(plan, k).Emit(); got != want {
				t.Errorf("plan %s = %q, want %q", k, got, want)
			}
		}
		pub := h.env.SpanByName("matrixrtc.member.publish")
		if pub == nil || obstest.Attr(pub, "matrixrtc.membership.format").AsString() != "sticky" ||
			!obstest.Attr(pub, "matrixrtc.delayed_leave").AsBool() {
			t.Fatalf("publish span %v", pub)
		}
		slots := h.env.SpansByName("matrixrtc.slot.open")
		if len(slots) != 4 || obstest.Attr(slots[0], "matrixrtc.slot.action").AsString() != "opened" {
			t.Fatalf("slot spans %d, want one per (type, id) pair = 4", len(slots))
		}
		sc.cancel()
		synctest.Wait()
		if h.setupAttr("matrixrtc.plan.format") != "dual" {
			t.Error("setup span lacks the plan format")
		}
	})
}

// With a dialect remembered from the owner's sticky membership the ghost mirrors it, and with
// /rtc/transports the join uses MSC4195 and both memberships name the same hashed identity.
func TestInboundDualDialectMSC4195(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now()))
		h.matrix(h.ownerMember("OLD", false)) // unrelated event: nothing may change
		sc := h.invite()
		j := joinsOf(g)[0]
		if j.typ != matrixrtc.RTCMember || j.content[matrixrtc.FieldSlotID] != "m.call#ROOM" {
			t.Errorf("dialect not mirrored: %v %v", j.typ, j.content[matrixrtc.FieldSlotID])
		}
		mid := memberOf(j.content)[matrixrtc.FieldID].(string)
		ts := transportsOf(j.content)
		if len(ts) != 2 || ts[0]["type"] != "m.livekit" || ts[0]["url"] != "wss://lk.example.org" || ts[1]["type"] != "livekit" {
			t.Errorf("transports %v", ts)
		}
		want := []string{"org.matrix.msc4143.rtc.slot|m.call#ROOM", "m.rtc.slot|m.call#ROOM",
			"org.matrix.msc4143.rtc.slot|m.call#room", "m.rtc.slot|m.call#room"}
		if got := g.openedList(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("opened %v, want %v", got, want)
		}
		// Legacy content: membershipID = member.id and foci_preferred[0].url.
		st := h.ghost.stateCalls()[0].content
		fp := st["foci_preferred"].([]any)[0].(map[string]any)
		if st["membershipID"] != mid || fp["url"] != "wss://lk.example.org" {
			t.Errorf("legacy content %v", st)
		}
		// Ring slot_id follows the dialect.
		if r := stickyRings(g); len(r) != 1 || r[0].content[matrixrtc.FieldSlotID] != "m.call#ROOM" {
			t.Errorf("ring %+v", r)
		}
		jp := h.joiner.calls[0]
		if jp.Auth != rtc.AuthMSC4195 || jp.SlotID != "m.call#ROOM" || jp.MemberID != mid || jp.LiveKitURL != "wss://lk.example.org" {
			t.Errorf("join params %+v", jp)
		}
		if h.env.SpanByName("matrixrtc.plan.choose") == nil ||
			obstest.Attr(h.env.SpanByName("matrixrtc.plan.choose"), "matrixrtc.transports.status").AsString() != "ok" {
			t.Error("transports status not ok")
		}
		_ = sc
	})
}

// MSC4195 -> sfu_get: a token_error on the first attempt switches to OpenID, re-publishes both
// memberships (same member id) with the service-url identity and retries.
func TestInboundTokenFallback(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"),
			withJoinSteps(joinStep{err: tokenErr()}))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now())) // dialect
		sc := h.invite()
		if h.joiner.attempts() != 2 {
			t.Fatalf("attempts %d", h.joiner.attempts())
		}
		p1, p2 := h.joiner.calls[0], h.joiner.calls[1]
		if p1.Auth != rtc.AuthMSC4195 || p2.Auth != rtc.AuthSFUGet {
			t.Fatalf("auth %v then %v", p1.Auth, p2.Auth)
		}
		joins := joinsOf(g)
		if len(joins) != 2 || len(leavesOf(g)) != 0 {
			t.Fatalf("joins %d", len(joins))
		}
		mid := memberOf(joins[0].content)[matrixrtc.FieldID]
		if memberOf(joins[1].content)[matrixrtc.FieldID] != mid {
			t.Error("member id changed on fallback")
		}
		ts := transportsOf(joins[1].content)
		if len(ts) != 1 || ts[0][matrixrtc.FieldLiveKitServiceURL] != testServiceURL || ts[0]["url"] != nil {
			t.Errorf("fallback transports %v", ts)
		}
		st := h.ghost.stateCalls()
		if len(st) != 2 || st[0].content["membershipID"] != mid || st[1].content["membershipID"] == mid {
			t.Fatalf("legacy republish %+v", st)
		}
		if fp := st[1].content["foci_preferred"].([]any)[0].(map[string]any); fp["url"] != nil {
			t.Errorf("legacy url after fallback %v", fp)
		}
		fb := 0
		for _, s := range h.env.SpansByName("matrixrtc.member.publish") {
			if obstest.Attr(s, "rtc.token.fallback").AsBool() {
				fb++
			}
		}
		if fb != 1 {
			t.Errorf("fallback publish spans %d", fb)
		}
		h.answer(sc, "DEV")
		// Refresh after the fallback uses the OpenID content too.
		h.advance(matrixrtc.MemberStickyRefresh)
		js := joinsOf(g)
		last := js[len(js)-1]
		if last.content[matrixrtc.FieldStickyKey] != mid || transportsOf(last.content)[0][matrixrtc.FieldLiveKitServiceURL] != testServiceURL {
			t.Errorf("refresh content %v", last.content)
		}
		h.matrix(h.ownerMember("DEV", false))
		if got := h.setupAttr("rtc.token.fallback"); got != "true" {
			t.Errorf("setup span rtc.token.fallback = %q", got)
		}
	})
}

// A token error with livekit_auth=msc4195 is not retried with sfu_get.
func TestInboundTokenErrorNoFallbackWhenForced(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"), withAuth(authMSC4195),
			withJoinSteps(joinStep{err: tokenErr()}, joinStep{err: tokenErr()}))
		h.invite()
		if h.joiner.attempts() != 2 || h.joiner.calls[1].Auth != rtc.AuthMSC4195 {
			t.Fatalf("attempts %d, second %+v", h.joiner.attempts(), h.joiner.calls[1])
		}
		if len(joinsOf(g)) != 1 {
			t.Errorf("republished although no fallback: %d joins", len(joinsOf(g)))
		}
	})
}

// A non-token join failure never triggers the fallback.
func TestInboundOtherJoinErrorNoFallback(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withTransports("wss://lk.example.org"),
			withJoinSteps(joinStep{err: &rtc.JoinError{Stage: "connect_error", Err: errors.New("x")}}))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now())) // dialect
		h.invite()
		if h.joiner.calls[1].Auth != rtc.AuthMSC4195 || len(joinsOf(g)) != 1 {
			t.Errorf("unexpected fallback: %+v, joins %d", h.joiner.calls[1], len(joinsOf(g)))
		}
	})
}

// ---- config and capability rows ----

func TestInboundNoStickyCapability(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) { g.caps = map[string]bool{matrixrtc.FeatureDelayed: true} }))
		h.invite()
		if len(g.stickyList()) != 0 || len(g.delayList()) != 0 || len(g.openedList()) != 0 {
			t.Errorf("sticky traffic without the capability: %+v", g.stickyList())
		}
		if len(plainRings(h)) != 1 || plainRings(h)[0].content[matrixrtc.FieldSlotID] != "m.call#ROOM" {
			t.Errorf("rings %+v", plainRings(h))
		}
		if _, has := plainRings(h)[0].content[matrixrtc.FieldStickyKeyUnstable]; has {
			t.Error("legacy ring has a sticky key")
		}
		if h.ghost.stateCalls()[0].content["membershipID"] == "" {
			t.Error("legacy membership missing")
		}
		plan := h.env.SpanByName("matrixrtc.plan.choose")
		if obstest.Attr(plan, "matrixrtc.plan.reason").AsString() != "no_sticky_support" ||
			obstest.Attr(plan, "matrixrtc.plan.format").AsString() != "legacy" {
			t.Errorf("plan span attrs wrong")
		}
	})
}

func TestInboundConfigLegacySkipsCapabilities(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withFormat(fmtLegacy), withTransports("wss://x"))
		h.invite()
		if len(g.stickyList()) != 0 || len(g.reads) != 0 || len(g.openedList()) != 0 {
			t.Error("sticky or slot traffic with matrixrtc_format: legacy")
		}
		if plan := h.env.SpanByName("matrixrtc.plan.choose"); plan == nil ||
			obstest.Attr(plan, "matrixrtc.plan.reason").AsString() != "config_legacy" {
			t.Errorf("plan span %v", plan)
		}
	})
}

func TestInboundConfigSticky(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withFormat(fmtSticky))
		sc := h.invite()
		if len(h.ghost.stateCalls()) != 0 {
			t.Errorf("legacy membership published with sticky only: %+v", h.ghost.stateCalls())
		}
		if len(joinsOf(g)) != 1 {
			t.Fatal("no sticky join")
		}
		r := stickyRings(g)
		if len(r) != 1 {
			t.Fatalf("rings %d", len(r))
		}
		if _, has := r[0].content[matrixrtc.FieldRelatesTo]; has {
			t.Error("sticky-only ring carries m.relates_to")
		}
		// No legacy refresh timer: only the sticky one (40 min) fires.
		h.answer(sc, "DEV")
		h.advance(matrixrtc.MemberStickyRefresh)
		if len(joinsOf(g)) != 2 || len(h.ghost.stateCalls()) != 0 {
			t.Errorf("refresh: %d joins, %d legacy states", len(joinsOf(g)), len(h.ghost.stateCalls()))
		}
		h.matrix(h.ownerMember("DEV", false))
		opsIndex(t, h, "delayed:send")
		if h.ops.count("clear") != 0 {
			t.Error("legacy clear sent for a sticky-only call")
		}
	})
}

func TestInboundConfigDual(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g), withFormat(fmtDual))
		h.invite()
		if len(joinsOf(g)) != 1 || len(h.ghost.stateCalls()) != 1 {
			t.Errorf("dual: %d joins, %d legacy", len(joinsOf(g)), len(h.ghost.stateCalls()))
		}
	})
}

// Without GhostRTCIntent (plain fakeGhost) the call is the legacy call, with no plan span.
func TestInboundNoRTCIntentLegacy(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		h.invite()
		if h.env.SpanByName("matrixrtc.plan.choose") != nil {
			t.Error("plan span for a ghost without the capability")
		}
		if len(plainRings(h)) != 1 {
			t.Error("legacy ring missing")
		}
	})
}

// ---- slots ----

// Explicit dual: an encrypted slot refuses the call like an encrypted room.
func TestSlotEncryptedRefusesCall(t *testing.T) {
	for _, spelling := range []string{"m.call#room", "m.call#ROOM"} {
		run(t, func(t *testing.T) {
			var g *fakeRTCGhost
			h := newHarness(t, withFormat("dual"), withRTCGhost(&g, func(g *fakeRTCGhost) {
				g.slots[spelling] = map[string]any{"status": "open", "application": map[string]any{"type": "m.call"},
					"encryption": map[string]any{"type": "m.per_member"}}
			}))
			sc := h.invite()
			if len(g.stickyList()) != 0 || len(h.ghost.stateCalls()) != 0 || len(plainRings(h)) != 0 {
				t.Error("something was published into an encrypted slot")
			}
			if !sc.has("reject:480") {
				t.Errorf("sip %v", sc.commands())
			}
			assertNotices(t, h, noticeEncrypted)
			h.assertEnded("rejected", "")
			for _, o := range g.openedList() {
				if strings.HasSuffix(o, "|"+spelling) {
					t.Errorf("encrypted slot rewritten: %v", o)
				}
			}
			spans := h.env.SpansByName("matrixrtc.slot.open")
			refused := false
			for _, s := range spans {
				if obstest.Attr(s, "matrixrtc.slot.action").AsString() == "refused_encrypted" {
					refused = true
				}
			}
			if !refused {
				t.Error("no refused_encrypted slot span")
			}
		})
	}
}

// Auto, bridge-initiated: an encrypted slot only drops the optional sticky part; the call rings on the
// legacy path as it did before MatrixRTC 2.0 support.
func TestSlotEncryptedAutoBridgeDegradesToLegacy(t *testing.T) {
	for _, spelling := range []string{"m.call#room", "m.call#ROOM"} {
		run(t, func(t *testing.T) {
			var g *fakeRTCGhost
			h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
				g.slots[spelling] = map[string]any{"status": "open", "application": map[string]any{"type": "m.call"},
					"encryption": map[string]any{"type": "m.per_member"}}
			}))
			sc := h.invite()
			if len(g.stickyList()) != 0 {
				t.Errorf("sticky published: %v", g.stickyList())
			}
			if got := g.openedList(); len(got) != 0 {
				t.Errorf("slots opened: %v", got)
			}
			if len(h.ghost.stateCalls()) == 0 {
				t.Error("no legacy call.member published")
			}
			if len(plainRings(h)) == 0 {
				t.Error("no ring")
			}
			if sc.has("reject:480") {
				t.Errorf("call refused: %v", sc.commands())
			}
			assertNotices(t, h)
		})
	}
}

func TestSlotOpenClosedAndNoop(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.slots["m.call#room"] = map[string]any{"status": "open", "application": map[string]any{"type": "m.call"}}
			g.slots["m.call#ROOM"] = map[string]any{"status": "closed", "application": map[string]any{"type": "m.call"}}
		}))
		h.invite()
		if got := g.openedList(); len(got) != 2 || got[0] != "m.rtc.slot|m.call#ROOM" || got[1] != "org.matrix.msc4143.rtc.slot|m.call#ROOM" {
			t.Errorf("opened %v", got)
		}
		actions := map[string]string{}
		for _, s := range h.env.SpansByName("matrixrtc.slot.open") {
			actions[obstest.Attr(s, "matrixrtc.slot_id").AsString()] = obstest.Attr(s, "matrixrtc.slot.action").AsString()
		}
		if actions["m.call#room"] != "noop" || actions["m.call#ROOM"] != "opened" {
			t.Errorf("actions %v", actions)
		}
	})
}

// A slot that only exists under the other slot event type is found (and not recreated).
func TestSlotFoundUnderOtherType(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.slots["m.call#room"] = map[string]any{"status": "open", "application": map[string]any{"type": "m.call"}}
			g.slots["m.call#ROOM"] = map[string]any{"status": "open", "application": map[string]any{"type": "m.call"}}
		}))
		h.invite()
		if len(g.openedList()) != 0 {
			t.Errorf("opened %v", g.openedList())
		}
	})
}

// A slot read failure degrades a dual plan to legacy; a slot open failure too.
func TestSlotErrorDegradesToLegacy(t *testing.T) {
	for name, mod := range map[string]func(*fakeRTCGhost){
		"read":  func(g *fakeRTCGhost) { g.stateErr = errors.New("boom") },
		"write": func(g *fakeRTCGhost) { g.slotOpenErr = errors.New("forbidden") },
	} {
		t.Run(name, func(t *testing.T) {
			run(t, func(t *testing.T) {
				var g *fakeRTCGhost
				h := newHarness(t, withRTCGhost(&g, mod))
				sc := h.invite()
				if len(g.stickyList()) != 0 || len(g.delayList()) != 0 {
					t.Error("sticky published after a slot error")
				}
				if len(h.ghost.stateCalls()) != 1 || len(plainRings(h)) != 1 {
					t.Errorf("legacy fallback missing: %d states, %d rings", len(h.ghost.stateCalls()), len(plainRings(h)))
				}
				sc.cancel()
				synctest.Wait()
				if got := h.setupAttr("matrixrtc.plan.downgrade"); got != "slot_error" {
					t.Errorf("downgrade attr %q", got)
				}
			})
		})
	}
}

// ---- ErrUnsupported ----

func TestStickyUnsupportedDowngradesAndRemembers(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.stickyErr = func(_ int, c stickyCall) error {
				if matrixrtc.IsMemberType(c.typ) {
					return fmt.Errorf("send: %w", core.ErrUnsupported)
				}
				return nil
			}
		}))
		sc := h.invite()
		// The delayed leave that was scheduled is cancelled, the call continues legacy.
		if h.ops.index("delayed:cancel") < 0 {
			t.Errorf("delayed leave not cancelled; ops %v", h.ops.list())
		}
		if len(h.ghost.stateCalls()) != 1 || len(plainRings(h)) != 1 || len(stickyRings(g)) != 0 {
			t.Errorf("legacy fallback: %d states, %d plain rings, %d sticky rings", len(h.ghost.stateCalls()), len(plainRings(h)), len(stickyRings(g)))
		}
		// A refused join must not leave a keeper behind or a sticky leave at the end.
		h.answer(sc, "DEV")
		h.advance(30 * time.Second)
		if len(g.updateList()) != 1 {
			t.Errorf("updates after downgrade: %+v", g.updateList())
		}
		h.matrix(h.ownerMember("DEV", false))
		if got := h.setupAttr("matrixrtc.plan.downgrade"); got != "sticky_unsupported" {
			t.Errorf("downgrade %q", got)
		}
		if len(leavesOf(g)) != 0 || h.ops.count("delayed:send") != 0 {
			t.Error("sticky leave for a call that never published sticky")
		}
		h.assertEnded("answered", "2xx")

		// Within 10 minutes the next call is legacy without trying sticky.
		before := len(g.stickyList())
		h.advance(time.Minute)
		sc2 := h.newSIPCall()
		h.m.HandleInvite(testLogin, testLocal, sc2)
		synctest.Wait()
		if len(g.stickyList()) != before {
			t.Errorf("sticky tried again within the block window")
		}
		plan := h.env.SpansByName("matrixrtc.plan.choose")
		if r := obstest.Attr(plan[len(plan)-1], "matrixrtc.plan.reason").AsString(); r != "sticky_unsupported_recently" {
			t.Errorf("second plan reason %q", r)
		}
		sc2.cancel()
		synctest.Wait()

		// After the window it is tried again.
		h.advance(stickyUnsupportedTTL)
		h.m.HandleInvite(testLogin, testLocal, h.newSIPCall())
		synctest.Wait()
		if len(g.stickyList()) == before {
			t.Error("sticky never retried after the block window")
		}
	})
}

func TestStickyOnlyUnsupportedFailsCall(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withFormat(fmtSticky), withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.stickyErr = func(int, stickyCall) error { return core.ErrUnsupported }
		}))
		sc := h.invite()
		if len(h.ghost.stateCalls()) != 0 || len(plainRings(h)) != 0 {
			t.Error("legacy published for a sticky-only plan")
		}
		if !sc.has("reject:480") {
			t.Errorf("sip %v", sc.commands())
		}
		h.assertEnded("failed", "")
	})
}

// A refused sticky ring is re-sent as a plain ring.
func TestStickyRingUnsupportedFallsBack(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.stickyErr = func(_ int, c stickyCall) error {
				if c.typ == matrixrtc.RTCNotification {
					return core.ErrUnsupported
				}
				return nil
			}
		}))
		h.invite()
		if len(plainRings(h)) != 1 {
			t.Fatalf("plain rings %d", len(plainRings(h)))
		}
		if _, has := plainRings(h)[0].content[matrixrtc.FieldStickyKeyUnstable]; has {
			t.Error("plain ring has a sticky key")
		}
		h.advance(h.cfg.RingTimeout)
		if len(withdrawsOf(g)) != 0 {
			t.Error("withdraw for a ring that was not sticky")
		}
	})
}

// ---- ring end ----

func TestStickyRingTimeoutRedactsAndWithdraws(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now())) // dialect: sticky ring
		sc := h.invite()
		h.advance(30 * time.Second)
		if !sc.has("reject:480") {
			t.Fatalf("sip %v", sc.commands())
		}
		opsIndex(t, h, "sticky:ring", "redact", "sticky:withdraw")
		opsIndex(t, h, "redact", "delayed:send", "clear")
		if len(withdrawsOf(g)) != 1 {
			t.Errorf("withdraws %d", len(withdrawsOf(g)))
		}
		if h.env.SpanByName("matrixrtc.ring.withdraw") == nil {
			t.Error("no withdraw span")
		}
		h.assertEnded("timeout", "")
	})
}

// ---- keeper ----

func TestKeeperGoneReschedulesAndRejoins(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.updateErr = func(n int, c updateCall) error {
				if n == 0 {
					return fmt.Errorf("restart: %w", core.ErrNotFound)
				}
				return nil
			}
		}))
		h.invite()
		h.advance(matrixrtc.DelayedLeaveRestart)
		if len(g.delayList()) != 2 {
			t.Fatalf("delayed leave not rescheduled: %d", len(g.delayList()))
		}
		if len(joinsOf(g)) != 2 {
			t.Fatalf("join not re-sent: %d", len(joinsOf(g)))
		}
		h.advance(matrixrtc.DelayedLeaveRestart)
		up := g.updateList()
		if len(up) != 2 || up[1].id != "delay2" {
			t.Errorf("updates %+v", up)
		}
		if h.env.Sum(t, "mrtcsip.matrixrtc.delayed_leave.restarts", "outcome", "gone") != 1 ||
			h.env.Sum(t, "mrtcsip.matrixrtc.delayed_leave.restarts", "outcome", "ok") != 1 {
			t.Errorf("metrics %+v", h.env.MetricPoints(t, "mrtcsip.matrixrtc.delayed_leave.restarts"))
		}
		// The rescheduled one is what the teardown fires.
		h.advance(30 * time.Second) // ring timeout
		if u := g.updateList(); u[len(u)-1].id != "delay2" || u[len(u)-1].action != "send" {
			t.Errorf("last update %+v", u[len(u)-1])
		}
	})
}

func TestKeeperErrorsKeepTicking(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.updateErr = func(n int, c updateCall) error {
				if c.action == "restart" && n == 0 {
					return errors.New("429")
				}
				return nil
			}
		}))
		h.invite()
		h.advance(matrixrtc.DelayedLeaveRestart)
		h.advance(matrixrtc.DelayedLeaveRestart)
		if len(g.updateList()) != 2 || len(g.delayList()) != 1 {
			t.Errorf("updates %d delays %d", len(g.updateList()), len(g.delayList()))
		}
		if h.env.Sum(t, "mrtcsip.matrixrtc.delayed_leave.restarts", "outcome", "error") != 1 {
			t.Error("error outcome not counted")
		}
	})
}

// A failed reschedule ends the keeper; the teardown then leaves explicitly.
func TestKeeperLostThenExplicitLeave(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.updateErr = func(int, updateCall) error { return core.ErrNotFound }
			g.delayErr = func(n int) error {
				if n > 0 {
					return errors.New("limit")
				}
				return nil
			}
		}))
		h.invite()
		h.advance(matrixrtc.DelayedLeaveRestart)
		h.advance(matrixrtc.DelayedLeaveRestart)
		if len(g.updateList()) != 1 {
			t.Errorf("keeper kept running after losing the delayed leave: %d", len(g.updateList()))
		}
		h.advance(30 * time.Second)
		if len(leavesOf(g)) != 1 || h.ops.count("delayed:send") != 0 {
			t.Errorf("want one explicit leave; leaves %d, ops %v", len(leavesOf(g)), h.ops.list())
		}
		leave := h.env.SpanByName("matrixrtc.member.leave")
		if obstest.Attr(leave, "matrixrtc.member.leave_via").AsString() != "explicit" {
			t.Error("leave_via not explicit")
		}
	})
}

// No delayed-event support: no keeper, no delayed leave, explicit leave with the right content.
func TestNoDelayedSupportExplicitLeave(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) { g.caps = map[string]bool{matrixrtc.FeatureSticky: true} }))
		h.invite()
		if len(g.delayList()) != 0 {
			t.Fatal("delayed leave scheduled")
		}
		h.advance(10 * time.Second)
		if len(g.updateList()) != 0 {
			t.Error("keeper ran without a delayed event")
		}
		if pub := h.env.SpanByName("matrixrtc.member.publish"); obstest.Attr(pub, "matrixrtc.delayed_leave").AsBool() {
			t.Error("delayed_leave attr true")
		}
		h.advance(20 * time.Second)
		mid := memberOf(joinsOf(g)[0].content)[matrixrtc.FieldID].(string)
		ls := leavesOf(g)
		if len(ls) != 1 || jsonOf(ls[0].content) != jsonOf(matrixrtc.StickyLeaveContent("m.call#room", mid)) ||
			ls[0].d != matrixrtc.MemberStickyDuration || ls[0].typ != matrixrtc.RTCMemberAlias {
			t.Errorf("leave %+v", ls)
		}
		opsIndex(t, h, "sticky:leave", "clear")
	})
}

// A failing delayed "send" falls back to the explicit leave.
func TestLeaveDelayedSendFailsExplicit(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.updateErr = func(_ int, c updateCall) error {
				if c.action == "send" {
					return core.ErrNotFound
				}
				return nil
			}
		}))
		h.invite()
		h.advance(30 * time.Second)
		if len(leavesOf(g)) != 1 {
			t.Errorf("explicit leave missing; ops %v", h.ops.list())
		}
		opsIndex(t, h, "delayed:send", "sticky:leave", "clear")
	})
}

// ---- refresh ----

func TestStickyRefreshSameMemberID(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withRTCGhost(&g))
		sc := h.invite()
		h.answer(sc, "DEV")
		mid := memberOf(joinsOf(g)[0].content)[matrixrtc.FieldID]
		n := len(joinsOf(g))
		// Step in keeper-sized slices so the fake ticker is serviced; refresh is due at 40 min.
		for i := 0; i < 342; i++ { // 2394 s < 40 min
			h.advance(matrixrtc.DelayedLeaveRestart)
		}
		if len(joinsOf(g)) != n {
			t.Fatalf("refreshed early")
		}
		h.advance(time.Minute)
		js := joinsOf(g)
		if len(js) != n+1 || memberOf(js[n].content)[matrixrtc.FieldID] != mid || js[n].d != matrixrtc.MemberStickyDuration {
			t.Fatalf("refresh: %d joins", len(js))
		}
		sp := h.env.SpanByName("call.membership.refresh")
		if sp == nil || obstest.Attr(sp, "matrixrtc.membership.format").AsString() != "sticky" {
			t.Fatalf("refresh span %v", sp)
		}
		if len(h.env.Links(sp)) != 1 {
			t.Error("refresh span not linked to the setup span")
		}
		// A failing refresh retries after refreshRetryDelay.
		g.rmu.Lock()
		g.stickyErr = func(n int, c stickyCall) error {
			if matrixrtc.IsMemberType(c.typ) {
				return errors.New("500")
			}
			return nil
		}
		g.rmu.Unlock()
		for i := 0; i < 342; i++ {
			h.advance(matrixrtc.DelayedLeaveRestart)
		}
		failed := len(joinsOf(g))
		g.rmu.Lock()
		g.stickyErr = nil
		g.rmu.Unlock()
		h.advance(refreshRetryDelay)
		if len(joinsOf(g)) <= failed {
			t.Error("no retry after a failed refresh")
		}
	})
}

// ---- owner-initiated ----

func ownerStickyStart(h *harness, typ mevent.Type, slot string) {
	h.matrix(stickyMemberEvent(typ, testOwner, "ownermem", slot, h.clk.Now()))
	h.matrix(stickyNotifEvent(testOwner, testNotif, h.clk.Now(), slot))
}

// Owner-initiated sticky call: the ghost mirrors the owner's dialect, sticky only, MSC4195 join with the
// owner's url, no ring, owner identity known from the membership.
func TestOwnerStickyMirror(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g))
		ownerStickyStart(h, matrixrtc.RTCMember, "m.call#ROOM")
		oc := h.dialed()
		_ = oc
		joins := joinsOf(g)
		if len(joins) != 1 {
			t.Fatalf("joins %d; ops %v", len(joins), h.ops.list())
		}
		j := joins[0]
		if j.typ != matrixrtc.RTCMember || j.content[matrixrtc.FieldSlotID] != "m.call#ROOM" {
			t.Errorf("not mirrored: %v %v", j.typ, j.content[matrixrtc.FieldSlotID])
		}
		ts := transportsOf(j.content)
		if len(ts) != 2 || ts[0]["url"] != testRTCURL {
			t.Errorf("transports %v", ts)
		}
		if len(h.ghost.stateCalls()) != 0 {
			t.Errorf("legacy membership published: %+v", h.ghost.stateCalls())
		}
		if len(stickyRings(g)) != 0 || len(plainRings(h)) != 0 {
			t.Error("ring sent on the owner-initiated path")
		}
		if got := g.openedList(); len(got) != 1 || got[0] != "org.matrix.msc4143.rtc.slot|m.call#ROOM" {
			t.Errorf("opened %v", got)
		}
		mid := memberOf(j.content)[matrixrtc.FieldID].(string)
		jp := h.joiner.calls[0]
		wantOwner := matrixrtc.LiveKitIdentity(testOwner, "ownermem")
		if jp.Auth != rtc.AuthMSC4195 || jp.LiveKitURL != testRTCURL || jp.MemberID != mid || jp.SlotID != "m.call#ROOM" ||
			len(jp.Owners) != 1 || jp.Owners[0] != wantOwner {
			t.Errorf("join params %+v (want owner %s)", jp, wantOwner)
		}
		opsIndex(t, h, "slot:open:m.call#ROOM", "delayed", "sticky:join", "join", "dial")
		// Ending: the sticky leave and no legacy clear.
		h.ownerJoinedLK("EC")
		h.dialed().answer()
		synctest.Wait()
		h.matrix(stickyLeaveEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now()))
		opsIndex(t, h, "delayed:send")
		if h.ops.count("clear") != 0 {
			t.Error("legacy clear for a sticky-only call")
		}
	})
}

// Identities learned after the join reach the session, and the session gets the ones known before.
func TestOwnerIdentitiesReachSession(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g))
		ownerStickyStart(h, matrixrtc.RTCMemberAlias, "m.call#room")
		h.dialed()
		s := h.joiner.lastSession()
		want := matrixrtc.LiveKitIdentity(testOwner, "ownermem")
		found := false
		for _, id := range s.ownerIdentities() {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("session owner identities %v lack %s", s.ownerIdentities(), want)
		}
		// A second owner device that joins later is added to the live session.
		n := len(s.ownerIdentities())
		h.matrix(stickyMemberEvent(matrixrtc.RTCMemberAlias, testOwner, "second", "m.call#room", h.clk.Now()))
		got := s.ownerIdentities()
		if len(got) != n+1 || got[n] != matrixrtc.LiveKitIdentity(testOwner, "second") {
			t.Errorf("after second device: %v", got)
		}
		// The same membership again adds nothing.
		h.matrix(stickyMemberEvent(matrixrtc.RTCMemberAlias, testOwner, "second", "m.call#room", h.clk.Now()))
		if len(s.ownerIdentities()) != n+1 {
			t.Error("duplicate identity passed to the session")
		}
	})
}

// A legacy owner keeps the call exactly as before even when the ghost has the capability.
func TestOwnerLegacyUnchanged(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g), withTransports("wss://x"))
		h.ownerStart("OWNERDEV")
		h.dialed()
		if len(g.stickyList()) != 0 || len(g.delayList()) != 0 || len(g.openedList()) != 0 || len(g.reads) != 0 {
			t.Error("MatrixRTC 2.0 traffic for a legacy owner")
		}
		if len(h.ghost.stateCalls()) != 1 {
			t.Errorf("legacy states %d", len(h.ghost.stateCalls()))
		}
		if jp := h.joiner.calls[0]; jp.Auth != rtc.AuthSFUGet || jp.MemberID != "" || jp.LiveKitURL != "" {
			t.Errorf("join params %+v", jp)
		}
		if p := h.env.SpanByName("matrixrtc.plan.choose"); obstest.Attr(p, "matrixrtc.plan.reason").AsString() != "owner_legacy" {
			t.Error("plan reason")
		}
	})
}

// MSC4195 failure for an owner-mirrored call: OpenID with a service-url transport, same member id.
func TestOwnerStickyTokenFallback(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g), withJoinSteps(joinStep{err: tokenErr()}))
		ownerStickyStart(h, matrixrtc.RTCMemberAlias, "m.call#room")
		h.dialed()
		joins := joinsOf(g)
		if len(joins) != 2 {
			t.Fatalf("joins %d", len(joins))
		}
		ts := transportsOf(joins[1].content)
		if len(ts) != 1 || ts[0][matrixrtc.FieldLiveKitServiceURL] != testServiceURL {
			t.Errorf("transports %v", ts)
		}
		if memberOf(joins[0].content)[matrixrtc.FieldID] != memberOf(joins[1].content)[matrixrtc.FieldID] {
			t.Error("member id changed")
		}
		if h.joiner.calls[1].Auth != rtc.AuthSFUGet {
			t.Errorf("second attempt %+v", h.joiner.calls[1])
		}
	})
}

// A sticky owner notification is declined sticky; a legacy owner is declined with a plain message.
func TestOwnerStickyDecline(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g))
		h.dialer.err = errors.New("no route")
		ownerStickyStart(h, matrixrtc.RTCMemberAlias, "m.call#room")
		synctest.Wait()
		var d []stickyCall
		for _, s := range g.stickyList() {
			if s.typ == matrixrtc.RTCDecline {
				d = append(d, s)
			}
		}
		if len(d) != 1 || d[0].d != matrixrtc.NotificationStickyMin {
			t.Fatalf("sticky declines %+v; ops %v", d, h.ops.list())
		}
		if len(h.declines()) != 0 {
			t.Error("plain decline sent as well")
		}
		// The failed call also left the sticky membership.
		opsIndex(t, h, "sticky:decline")
		if h.ops.index("delayed:send") < 0 {
			t.Errorf("no sticky leave; ops %v", h.ops.list())
		}
	})
}

func TestOwnerLegacyDeclinePlain(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g))
		h.dialer.err = errors.New("no route")
		h.ownerStart("OWNERDEV")
		synctest.Wait()
		if len(h.declines()) != 1 {
			t.Fatalf("declines %d", len(h.declines()))
		}
		if h.ops.index("sticky:decline") >= 0 {
			t.Error("sticky decline for a legacy notification")
		}
	})
}

// An encrypted slot refuses an owner-initiated sticky call like an encrypted room.
func TestOwnerStickyEncryptedSlot(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g, func(g *fakeRTCGhost) {
			g.slots["m.call#room"] = map[string]any{"status": "open", "application": map[string]any{"type": "m.call"},
				"encryption": map[string]any{"type": "m.per_member"}}
		}))
		ownerStickyStart(h, matrixrtc.RTCMemberAlias, "m.call#room")
		synctest.Wait()
		if h.dialer.count() != 0 || len(g.stickyList()) != 1 { // only the decline
			t.Errorf("dials %d, sticky %+v", h.dialer.count(), g.stickyList())
		}
		assertNotices(t, h, noticeOutEncrypted)
		h.assertEnded("rejected", "")
	})
}

// ---- command-initiated ----

func TestCommandCallIsBridgeInitiated(t *testing.T) {
	run(t, func(t *testing.T) {
		var g *fakeRTCGhost
		h := newHarness(t, withOutbound(""), withRTCGhost(&g))
		h.matrix(stickyMemberEvent(matrixrtc.RTCMember, testOwner, "ownermem", "m.call#ROOM", h.clk.Now())) // dialect only
		if _, _, err := h.m.PlaceCall(context.Background(), testLogin, PlaceCallRequest{RoomID: testRoom}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(joinsOf(g)) != 1 || len(h.ghost.stateCalls()) != 1 || len(stickyRings(g)) != 1 {
			t.Fatalf("joins %d legacy %d rings %d", len(joinsOf(g)), len(h.ghost.stateCalls()), len(stickyRings(g)))
		}
		if joinsOf(g)[0].typ != matrixrtc.RTCMember || len(g.openedList()) != 4 {
			t.Errorf("dialect %v opened %v", joinsOf(g)[0].typ, g.openedList())
		}
		// Ring withdrawn and redacted when the callback ring times out.
		h.advance(h.cfg.RingTimeout)
		opsIndex(t, h, "redact", "sticky:withdraw")
	})
}

// ---- misc ----
