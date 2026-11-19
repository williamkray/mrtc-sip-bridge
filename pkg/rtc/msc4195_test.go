package rtc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

// remoteRoom is a fakeRoom with remote participants; ScanRemote records which ones were subscribed.
type remoteRoom struct {
	fakeRoom
	rmu        sync.Mutex
	remotes    []string
	subscribed map[string]int
	localID    string
}

func (r *remoteRoom) LocalIdentity() string { return r.localID }
func (r *remoteRoom) ScanRemote(fn func(string) bool) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	for _, id := range r.remotes {
		if fn(id) {
			if r.subscribed == nil {
				r.subscribed = map[string]int{}
			}
			r.subscribed[id]++
		}
	}
}
func (r *remoteRoom) subs(id string) int {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	return r.subscribed[id]
}

func msc4195Params(g *fakeRTCGhost) JoinParams {
	p := params(g, "")
	p.Auth, p.SlotID, p.MemberID, p.LiveKitURL = AuthMSC4195, matrixrtc.SlotIDRoomLegacy, "mem-1", "wss://lk.test"
	return p
}

func TestJoinMSC4195(t *testing.T) {
	g := &fakeRTCGhost{tokJWT: testJWT}
	ownerHash := matrixrtc.LiveKitIdentity("@o:s", "owner-mem")
	fr := &remoteRoom{localID: matrixrtc.LiveKitIdentity("@ghost:s", "mem-1"), remotes: []string{ownerHash, "@o:s:PHONE", "@other:s:X"}}
	var gotURL, gotJWT string
	j := testJoiner(func(u, jwt string, _ *lksdk.RoomCallback, _ time.Duration) (roomConn, error) {
		gotURL, gotJWT = u, jwt
		return fr, nil
	})
	p := msc4195Params(g)
	p.Owners = []string{ownerHash}
	s, err := j.Join(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != "wss://lk.test" || gotJWT != testJWT {
		t.Fatalf("%q %q", gotURL, gotJWT)
	}
	if g.tokReq.URL != "wss://lk.test" || g.tokReq.RoomID != "!r:s" || g.tokReq.SlotID != matrixrtc.SlotIDRoomLegacy || g.tokReq.MemberID != "mem-1" {
		t.Fatalf("req %+v", g.tokReq)
	}
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		got[(<-s.Events()).Identity] = true
	}
	if !got[ownerHash] || !got["@o:s:PHONE"] || fr.subs("@other:s:X") != 0 {
		t.Fatalf("events %v subs other=%d", got, fr.subs("@other:s:X"))
	}
}

func TestJoinMSC4195Errors(t *testing.T) {
	dial := func(string, string, *lksdk.RoomCallback, time.Duration) (roomConn, error) { return &fakeRoom{}, nil }
	j := testJoiner(dial)
	stage := func(err error) string {
		var je *JoinError
		if !errors.As(err, &je) {
			t.Fatalf("not a JoinError: %v", err)
		}
		return je.Stage
	}
	// Ghost without GhostRTCIntent.
	p := msc4195Params(&fakeRTCGhost{})
	p.Ghost = &fakeGhost{}
	if _, err := j.Join(context.Background(), p); stage(err) != "token_error" {
		t.Fatalf("err %v", err)
	}
	// get_token failure: error text may be wrapped but a JWT-looking value is never added.
	g := &fakeRTCGhost{tokErr: errors.New("get_token: HTTP 403 M_FORBIDDEN")}
	_, err := j.Join(context.Background(), msc4195Params(g))
	if stage(err) != "token_error" || strings.Contains(err.Error(), testJWT) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err %v", err)
	}
	// Empty jwt and empty URL.
	if _, err := j.Join(context.Background(), msc4195Params(&fakeRTCGhost{})); stage(err) != "token_error" {
		t.Fatalf("err %v", err)
	}
	p = msc4195Params(&fakeRTCGhost{tokJWT: testJWT})
	p.LiveKitURL = ""
	if _, err := j.Join(context.Background(), p); stage(err) != "token_error" {
		t.Fatalf("err %v", err)
	}
}

func TestAddOwnerIdentitiesLate(t *testing.T) {
	hash := matrixrtc.LiveKitIdentity("@o:s", "owner-mem")
	fr := &remoteRoom{localID: "@ghost:s:DEV", remotes: []string{hash, "@other:s:X"}}
	j := testJoiner(func(string, string, *lksdk.RoomCallback, time.Duration) (roomConn, error) { return fr, nil })
	j.hc = defaultHTTPClient()
	s, err := j.Join(context.Background(), params(&fakeGhost{}, jwtServer(t)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("unexpected %+v", ev)
	default:
	}
	if fr.subs(hash) != 0 {
		t.Fatal("subscribed before match")
	}
	s.AddOwnerIdentities(hash)
	ev := <-s.Events()
	if ev.Kind != EventOwnerJoined || ev.Identity != hash || fr.subs(hash) != 1 {
		t.Fatalf("%+v subs=%d", ev, fr.subs(hash))
	}
	// Idempotent: no second event or subscription.
	s.AddOwnerIdentities(hash, "")
	select {
	case ev := <-s.Events():
		t.Fatalf("duplicate %+v", ev)
	default:
	}
	if fr.subs(hash) != 1 || fr.subs("@other:s:X") != 0 {
		t.Fatalf("subs %d %d", fr.subs(hash), fr.subs("@other:s:X"))
	}
}

// A non-owner participant never produces an event and is never subscribed, whatever the callbacks do;
// an owner is announced once, and again after it left and came back (reconnect).
func TestParticipantCallbacksOwnersOnly(t *testing.T) {
	fr := &remoteRoom{localID: "@ghost:s:DEV", remotes: []string{"@other:s:X", "HASH", "@o:s:PHONE"}}
	fr.present = map[string]bool{}
	s := newSession(JoinParams{Owner: "@o:s", Owners: []string{"HASH"}}, zerologNop(), nil2())
	s.room = fr
	s.roomPtr.Store(&roomHolder{fr})
	if !s.owners.match("HASH") || !s.owners.match("@o:s:D") || s.owners.match("@other:s:D") {
		t.Fatal("matcher")
	}
	cb := s.callbacks()
	if cb.OnParticipantConnected == nil || cb.OnParticipantDisconnected == nil {
		t.Fatal("no callbacks")
	}
	next := func() (Event, bool) {
		select {
		case ev := <-s.Events():
			return ev, true
		default:
			return Event{}, false
		}
	}

	// Non-owner: connect and disconnect are silent and nothing is subscribed.
	s.onParticipantConnected("@other:s:X")
	s.onParticipantDisconnected("@other:s:X")
	s.rescan()
	if ev, ok := next(); ok && ev.Identity == "@other:s:X" {
		t.Fatalf("non-owner event %+v", ev)
	}
	if fr.subs("@other:s:X") != 0 {
		t.Fatal("non-owner subscribed")
	}
	// Drain the owners announced by rescan.
	for {
		if _, ok := next(); !ok {
			break
		}
	}

	// Owner already announced by rescan: a connect callback does not announce it a second time.
	s.onParticipantConnected("HASH")
	if ev, ok := next(); ok {
		t.Fatalf("duplicate announce %+v", ev)
	}
	// Reconnect: the departure (participant really gone) clears the mark, the next connect announces.
	s.onParticipantDisconnected("HASH")
	if ev, ok := next(); !ok || ev.Kind != EventOwnerLeft || ev.Identity != "HASH" {
		t.Fatalf("want OwnerLeft, got %+v %v", ev, ok)
	}
	s.onParticipantConnected("HASH")
	if ev, ok := next(); !ok || ev.Kind != EventOwnerJoined || ev.Identity != "HASH" {
		t.Fatalf("want OwnerJoined after reconnect, got %+v %v", ev, ok)
	}
	s.onParticipantConnected("HASH")
	if ev, ok := next(); ok {
		t.Fatalf("second connect announced again %+v", ev)
	}
	// A stale departure (participant is back in the room) is not reported and keeps the mark.
	fr.mu.Lock()
	fr.present["HASH"] = true
	fr.mu.Unlock()
	s.onParticipantDisconnected("HASH")
	s.onParticipantConnected("HASH")
	if ev, ok := next(); ok {
		t.Fatalf("stale departure produced %+v", ev)
	}
}

// get_token is bounded by tokenTimeout: a stalled homeserver gives a token_error (not "timeout", the
// join context is still alive) that happened before the connect, so the caller can fall back to sfu_get.
func TestJoinMSC4195TokenTimeout(t *testing.T) {
	old := tokenTimeout
	tokenTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tokenTimeout = old })
	dialed := false
	j := testJoiner(func(string, string, *lksdk.RoomCallback, time.Duration) (roomConn, error) {
		dialed = true
		return &fakeRoom{}, nil
	})
	g := &stallRTCGhost{}
	p := msc4195Params(&g.fakeRTCGhost)
	p.Ghost = g
	start := time.Now()
	_, err := j.Join(context.Background(), p)
	var je *JoinError
	if !errors.As(err, &je) || je.Stage != "token_error" || !je.BeforeConnect || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second || dialed {
		t.Fatalf("took %v dialed=%v", d, dialed)
	}

	// A failure after the connect started is not "before connect".
	j2 := testJoiner(func(string, string, *lksdk.RoomCallback, time.Duration) (roomConn, error) {
		return nil, errors.New("ice failed")
	})
	_, err = j2.Join(context.Background(), msc4195Params(&fakeRTCGhost{tokJWT: testJWT}))
	if !errors.As(err, &je) || je.Stage != "connect_error" || je.BeforeConnect {
		t.Fatalf("err = %v", err)
	}
}

// stallRTCGhost's LiveKitToken blocks until its context ends.
type stallRTCGhost struct{ fakeRTCGhost }

func (g *stallRTCGhost) LiveKitToken(ctx context.Context, _ core.LiveKitTokenRequest) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func zerologNop() zerolog.Logger { return zerolog.Nop() }
func nil2() clock.Clock          { return clock.NewFake(time.Unix(0, 0)) }
