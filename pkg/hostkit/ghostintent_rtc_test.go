package hostkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

const (
	testToken = "syt_secret_access_token"
	testJWT   = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"
)

type recReq struct {
	Method, Path, RawPath string
	Query                 map[string][]string
	Body                  map[string]any
	Auth                  string
}

// fakeHS is an httptest homeserver; handler returns status and JSON body for a request.
type fakeHS struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recReq
}

func newFakeHS(t *testing.T, h func(r recReq) (int, any)) *fakeHS {
	t.Helper()
	f := &fakeHS{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rr := recReq{Method: r.Method, Path: r.URL.Path, RawPath: r.URL.EscapedPath(), Query: r.URL.Query(), Auth: r.Header.Get("Authorization")}
		if len(bytes.TrimSpace(raw)) > 0 {
			_ = json.Unmarshal(raw, &rr.Body)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rr)
		f.mu.Unlock()
		status, body := h(rr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHS) requests() []recReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recReq(nil), f.reqs...)
}

func (f *fakeHS) intent(t *testing.T) *ghostIntent {
	t.Helper()
	cl, err := mautrix.NewClient(f.srv.URL, "@ghost:example.org", testToken)
	if err != nil {
		t.Fatal(err)
	}
	cl.SetAppServiceUserID = true
	return &ghostIntent{raw: &appservice.IntentAPI{Client: cl, UserID: "@ghost:example.org"}}
}

func errBody(code, msg string) map[string]any { return map[string]any{"errcode": code, "error": msg} }

func requireMasquerade(t *testing.T, reqs []recReq) {
	t.Helper()
	for _, r := range reqs {
		if got := r.Query["user_id"]; len(got) != 1 || got[0] != "@ghost:example.org" {
			t.Errorf("%s %s: user_id = %v", r.Method, r.Path, got)
		}
	}
}

func assertClean(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	s := err.Error()
	for _, bad := range append(secrets, testToken, testJWT, "user_id", "http://", "127.0.0.1") {
		if strings.Contains(s, bad) {
			t.Errorf("error %q contains %q", s, bad)
		}
	}
}

func TestGhostRTCAssertion(t *testing.T) {
	var _ core.GhostRTCIntent = (*ghostIntent)(nil)
}

func TestSendSticky(t *testing.T) {
	f := newFakeHS(t, func(r recReq) (int, any) { return 200, map[string]any{"event_id": "$s1"} })
	g := f.intent(t)
	evID, err := g.SendSticky(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{"a": "b"}, time.Hour)
	if err != nil || evID != "$s1" {
		t.Fatalf("got %q %v", evID, err)
	}
	r := f.requests()[0]
	if r.Method != "PUT" || !strings.HasPrefix(r.Path, "/_matrix/client/v3/rooms/!r:example.org/send/org.matrix.msc4143.rtc.member/") {
		t.Errorf("path %s %s", r.Method, r.Path)
	}
	if got := r.Query[matrixrtc.QueryStickyDuration]; len(got) != 1 || got[0] != "3600000" {
		t.Errorf("sticky query = %v", got)
	}
	if r.Body["a"] != "b" {
		t.Errorf("body %v", r.Body)
	}
	requireMasquerade(t, f.requests())
}

func TestSendStickyUnsupported(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   any
		want   bool
	}{
		{"404", 404, errBody("M_UNRECOGNIZED", "Unrecognized request"), true},
		{"unrecognized400", 400, errBody("M_UNRECOGNIZED", "x"), true},
		{"400sticky", 400, errBody("M_INVALID_PARAM", "Unknown parameter org.matrix.msc4354.sticky_duration_ms"), true},
		{"400other", 400, errBody("M_BAD_JSON", "bad json"), false},
		{"400unknownDelay", 400, errBody("M_UNKNOWN", "Unknown parameter org.matrix.msc4140.delay"), true},
		// Limits and refusals are never "unsupported", even when the message names the parameter.
		{"400limit", 400, errBody("M_LIMIT_EXCEEDED", "Too many sticky events"), false},
		{"400forbiddenSticky", 400, errBody("M_FORBIDDEN", "sticky not allowed"), false},
		{"400tooLarge", 400, errBody("M_TOO_LARGE", "sticky event too large"), false},
		{"400stickyExceeds", 400, errBody("M_UNKNOWN", "The requested sticky duration exceeds the allowed maximum"), false},
		{"400noParamName", 400, errBody("M_INVALID_PARAM", "invalid thing"), false},
		// 404 M_NOT_FOUND (e.g. unknown room) is not a missing endpoint; no errcode is.
		{"404notfound", 404, errBody("M_NOT_FOUND", "Unknown room"), false},
		{"404noErrcode", 404, map[string]any{}, true},
		{"403", 403, errBody("M_FORBIDDEN", "no"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeHS(t, func(recReq) (int, any) { return c.status, c.body })
			_, err := f.intent(t).SendSticky(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{}, time.Hour)
			if err == nil {
				t.Fatal("want error")
			}
			if got := errors.Is(err, core.ErrUnsupported); got != c.want {
				t.Errorf("Is(ErrUnsupported) = %v, want %v (%v)", got, c.want, err)
			}
			assertClean(t, err, "bad json")
		})
	}
}

func TestScheduleDelayedBodyForm(t *testing.T) {
	f := newFakeHS(t, func(r recReq) (int, any) { return 200, map[string]any{"delay_id": "d1"} })
	g := f.intent(t)
	did, err := g.ScheduleDelayed(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{"x": 1}, 20*time.Second, time.Hour)
	if err != nil || did != "d1" {
		t.Fatalf("got %q %v", did, err)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.Method != "PUT" || !strings.HasPrefix(r.Path, matrixrtc.PathDelayedEventPrefix+"/rooms/!r:example.org/delayed_event/org.matrix.msc4143.rtc.member/") {
		t.Errorf("path %s %s", r.Method, r.Path)
	}
	if r.Body["delay_ms"] != float64(20000) || r.Body[matrixrtc.BodyStickyDuration] != float64(3600000) {
		t.Errorf("body %v", r.Body)
	}
	if c, _ := r.Body["content"].(map[string]any); c["x"] != float64(1) {
		t.Errorf("content %v", r.Body["content"])
	}
	requireMasquerade(t, reqs)

	// No sticky: no sticky key.
	if _, err = g.ScheduleDelayed(context.Background(), "!r:example.org", matrixrtc.CallMember, map[string]any{}, time.Second, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.requests()[1].Body[matrixrtc.BodyStickyDuration]; ok {
		t.Error("sticky key present without sticky")
	}
}

func TestScheduleDelayedQueryFallback(t *testing.T) {
	f := newFakeHS(t, func(r recReq) (int, any) {
		if strings.Contains(r.Path, "/delayed_event/") {
			return 404, errBody("M_UNRECOGNIZED", "Unrecognized request")
		}
		return 200, map[string]any{"delay_id": "d2"}
	})
	did, err := f.intent(t).ScheduleDelayed(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{"x": 1}, 20*time.Second, time.Hour)
	if err != nil || did != "d2" {
		t.Fatalf("got %q %v", did, err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[1]
	if !strings.HasPrefix(r.Path, "/_matrix/client/v3/rooms/!r:example.org/send/") ||
		r.Query[matrixrtc.QueryDelay][0] != "20000" || r.Query[matrixrtc.QueryStickyDuration][0] != "3600000" {
		t.Errorf("fallback request %+v", r)
	}
	if r.Body["x"] != float64(1) {
		t.Errorf("fallback body must be the bare content: %v", r.Body)
	}
	requireMasquerade(t, reqs)
}

func TestScheduleDelayedUnsupportedEverywhere(t *testing.T) {
	f := newFakeHS(t, func(recReq) (int, any) { return 404, errBody("M_UNRECOGNIZED", "nope") })
	_, err := f.intent(t).ScheduleDelayed(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{}, time.Second, 0)
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	assertClean(t, err)

	// A homeserver ignoring the delay query and sending at once (no delay_id) is unsupported.
	f2 := newFakeHS(t, func(r recReq) (int, any) {
		if strings.Contains(r.Path, "/delayed_event/") {
			return 404, errBody("M_UNRECOGNIZED", "nope")
		}
		return 200, map[string]any{"event_id": "$sent"}
	})
	_, err = f2.intent(t).ScheduleDelayed(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{}, time.Second, 0)
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}

	// A delay limit (Synapse: 400 M_UNKNOWN, "exceeds the allowed maximum") is a refusal of this request:
	// no query-form fallback, no unsupported marking.
	f4 := newFakeHS(t, func(recReq) (int, any) {
		return 400, errBody("M_UNKNOWN", "The requested delay exceeds the allowed maximum.")
	})
	_, err = f4.intent(t).ScheduleDelayed(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{}, time.Hour*99, time.Hour)
	if err == nil || errors.Is(err, core.ErrUnsupported) || len(f4.requests()) != 1 {
		t.Fatalf("delay limit: err = %v reqs=%d", err, len(f4.requests()))
	}
	f5 := newFakeHS(t, func(recReq) (int, any) { return 400, errBody("M_LIMIT_EXCEEDED", "Too many delayed events") })
	_, err = f5.intent(t).ScheduleDelayed(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{}, time.Second, 0)
	if err == nil || errors.Is(err, core.ErrUnsupported) || len(f5.requests()) != 1 {
		t.Fatalf("delay count limit: err = %v reqs=%d", err, len(f5.requests()))
	}

	// Other errors pass through without fallback.
	f3 := newFakeHS(t, func(recReq) (int, any) { return 400, errBody("M_MAX_DELAY_EXCEEDED", "too long") })
	_, err = f3.intent(t).ScheduleDelayed(context.Background(), "!r:example.org", matrixrtc.RTCMember, map[string]any{}, time.Hour*99, 0)
	if err == nil || errors.Is(err, core.ErrUnsupported) || len(f3.requests()) != 1 {
		t.Fatalf("err = %v reqs=%d", err, len(f3.requests()))
	}
}

func TestUpdateDelayed(t *testing.T) {
	f := newFakeHS(t, func(recReq) (int, any) { return 200, map[string]any{} })
	g := f.intent(t)
	for _, a := range []string{"restart", "cancel", "send"} {
		if err := g.UpdateDelayed(context.Background(), "d1", a); err != nil {
			t.Fatal(err)
		}
	}
	for i, a := range []string{"restart", "cancel", "send"} {
		r := f.requests()[i]
		if r.Method != "POST" || r.Path != matrixrtc.PathDelayedEventPrefix+"/delayed_events/d1/"+a || len(r.Body) != 0 {
			t.Errorf("%s: %+v", a, r)
		}
	}
	requireMasquerade(t, f.requests())

	// Legacy fallback.
	f2 := newFakeHS(t, func(r recReq) (int, any) {
		if strings.HasSuffix(r.Path, "/restart") {
			return 404, errBody("M_UNRECOGNIZED", "x")
		}
		return 200, map[string]any{}
	})
	if err := f2.intent(t).UpdateDelayed(context.Background(), "d1", "restart"); err != nil {
		t.Fatal(err)
	}
	r := f2.requests()[1]
	if r.Path != matrixrtc.PathDelayedEventPrefix+"/delayed_events/d1" || r.Body["action"] != "restart" {
		t.Errorf("legacy request %+v", r)
	}

	// Unknown id: not unsupported, no fallback, errors.Is(MNotFound).
	f3 := newFakeHS(t, func(recReq) (int, any) { return 404, errBody("M_NOT_FOUND", "No such delayed event") })
	err := f3.intent(t).UpdateDelayed(context.Background(), "gone", "restart")
	if err == nil || errors.Is(err, core.ErrUnsupported) || !errors.Is(err, mautrix.MNotFound) || !errors.Is(err, core.ErrNotFound) || len(f3.requests()) != 1 {
		t.Fatalf("err = %v reqs=%d", err, len(f3.requests()))
	}

	// Neither form recognized.
	f4 := newFakeHS(t, func(recReq) (int, any) { return 404, errBody("M_UNRECOGNIZED", "x") })
	if err := f4.intent(t).UpdateDelayed(context.Background(), "d", "send"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestStateEvent(t *testing.T) {
	f := newFakeHS(t, func(r recReq) (int, any) {
		if strings.HasSuffix(r.Path, "/missing") {
			return 404, errBody("M_NOT_FOUND", "Event not found")
		}
		if strings.HasSuffix(r.Path, "/forbidden") {
			return 403, errBody("M_FORBIDDEN", "nope")
		}
		return 200, map[string]any{"status": "open"}
	})
	g := f.intent(t)
	got, err := g.StateEvent(context.Background(), "!r:example.org", matrixrtc.RTCSlot, "m.call#room")
	if err != nil || got["status"] != "open" {
		t.Fatalf("got %v %v", got, err)
	}
	r := f.requests()[0]
	if r.Method != "GET" || r.Path != "/_matrix/client/v3/rooms/!r:example.org/state/org.matrix.msc4143.rtc.slot/m.call#room" {
		// '#' is path-escaped on the wire and decoded in URL.Path
		t.Errorf("path %s", r.Path)
	}
	if !strings.HasSuffix(r.RawPath, "m.call%23room") {
		t.Errorf("rawpath %s", r.RawPath)
	}
	got, err = g.StateEvent(context.Background(), "!r:example.org", matrixrtc.RTCSlot, "missing")
	if got != nil || err != nil {
		t.Fatalf("404: got %v %v", got, err)
	}
	if _, err = g.StateEvent(context.Background(), "!r:example.org", matrixrtc.RTCSlot, "forbidden"); err == nil {
		t.Fatal("403 must be an error")
	}
	requireMasquerade(t, f.requests())
}

func TestRTCTransports(t *testing.T) {
	stableBody := map[string]any{"transports": []any{map[string]any{"type": "livekit", "livekit_service_url": "https://lk.example.org"}}}
	f := newFakeHS(t, func(r recReq) (int, any) { return 200, stableBody })
	data, err := f.intent(t).RTCTransports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ts, ok := matrixrtc.ParseTransports(data); !ok || len(ts) != 1 {
		t.Errorf("raw body did not parse: %s", data)
	}
	if reqs := f.requests(); len(reqs) != 1 || reqs[0].Path != matrixrtc.PathRTCTransports || reqs[0].Method != "GET" {
		t.Errorf("reqs %+v", reqs)
	}
	requireMasquerade(t, f.requests())

	// Stable missing -> unstable.
	f2 := newFakeHS(t, func(r recReq) (int, any) {
		if r.Path == matrixrtc.PathRTCTransports {
			return 404, errBody("M_UNRECOGNIZED", "x")
		}
		return 200, map[string]any{"rtc_transports": []any{map[string]any{"type": "livekit", "livekit_service_url": "https://lk.example.org"}}}
	})
	data, err = f2.intent(t).RTCTransports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ts, ok := matrixrtc.ParseTransports(data); !ok || len(ts) != 1 {
		t.Errorf("unstable body did not parse: %s", data)
	}
	if reqs := f2.requests(); len(reqs) != 2 || reqs[1].Path != matrixrtc.PathRTCTransportsUnstable {
		t.Errorf("reqs %+v", reqs)
	}

	// Both missing.
	f3 := newFakeHS(t, func(recReq) (int, any) { return 404, errBody("M_UNRECOGNIZED", "x") })
	if _, err = f3.intent(t).RTCTransports(context.Background()); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	// Auth error: no fallback.
	f4 := newFakeHS(t, func(recReq) (int, any) { return 401, errBody("M_UNKNOWN_TOKEN", "bad token "+testToken) })
	_, err = f4.intent(t).RTCTransports(context.Background())
	if err == nil || errors.Is(err, core.ErrUnsupported) || len(f4.requests()) != 1 {
		t.Fatalf("err = %v", err)
	}
	assertClean(t, err)
}

func TestLiveKitToken(t *testing.T) {
	f := newFakeHS(t, func(r recReq) (int, any) { return 200, map[string]any{"jwt": testJWT} })
	g := f.intent(t)
	jwt, err := g.LiveKitToken(context.Background(), core.LiveKitTokenRequest{URL: "wss://lk.example.org", RoomID: "!r:example.org", SlotID: "m.call#room", MemberID: "mem1"})
	if err != nil || jwt != testJWT {
		t.Fatalf("got %q %v", jwt, err)
	}
	r := f.requests()[0]
	if r.Method != "POST" || r.Path != matrixrtc.PathLiveKitGetToken {
		t.Errorf("%s %s", r.Method, r.Path)
	}
	want := map[string]any{"url": "wss://lk.example.org", "room_id": "!r:example.org", "slot_id": "m.call#room", "member_id": "mem1"}
	for k, v := range want {
		if r.Body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, r.Body[k], v)
		}
	}
	requireMasquerade(t, f.requests())

	// stable missing -> unstable
	f2 := newFakeHS(t, func(r recReq) (int, any) {
		if r.Path == matrixrtc.PathLiveKitGetToken {
			return 404, errBody("M_UNRECOGNIZED", "x")
		}
		return 200, map[string]any{"jwt": testJWT}
	})
	jwt, err = f2.intent(t).LiveKitToken(context.Background(), core.LiveKitTokenRequest{URL: "u", RoomID: "!r:e", SlotID: "s", MemberID: "m"})
	if err != nil || jwt != testJWT || f2.requests()[1].Path != matrixrtc.PathLiveKitGetTokenUnstable {
		t.Fatalf("got %q %v %+v", jwt, err, f2.requests())
	}

	// both missing
	f3 := newFakeHS(t, func(recReq) (int, any) { return 404, errBody("M_UNRECOGNIZED", "x") })
	_, err = f3.intent(t).LiveKitToken(context.Background(), core.LiveKitTokenRequest{})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}

	// Real failure echoing secrets: never in the error, no fallback.
	f4 := newFakeHS(t, func(recReq) (int, any) {
		return 403, errBody("M_FORBIDDEN", "not joined; jwt "+testJWT+" token="+testToken)
	})
	_, err = f4.intent(t).LiveKitToken(context.Background(), core.LiveKitTokenRequest{URL: "wss://lk.example.org"})
	if err == nil || errors.Is(err, core.ErrUnsupported) || len(f4.requests()) != 1 {
		t.Fatalf("err = %v", err)
	}
	assertClean(t, err, "lk.example.org")

	// 200 with a body that is not the expected JSON: the body (with a JWT) must not leak.
	f5 := newFakeHS(t, func(recReq) (int, any) { return 200, testJWT })
	_, err = f5.intent(t).LiveKitToken(context.Background(), core.LiveKitTokenRequest{})
	if err == nil {
		t.Fatal("want error")
	}
	assertClean(t, err)

	// Missing jwt.
	f6 := newFakeHS(t, func(recReq) (int, any) { return 200, map[string]any{} })
	if _, err = f6.intent(t).LiveKitToken(context.Background(), core.LiveKitTokenRequest{}); err == nil {
		t.Fatal("want error for empty jwt")
	}
}

func TestTransportErrorIsScrubbed(t *testing.T) {
	f := newFakeHS(t, func(recReq) (int, any) { return 200, map[string]any{} })
	g := f.intent(t)
	f.srv.Close() // connection refused; the underlying *url.Error contains the full URL
	_, err := g.LiveKitToken(context.Background(), core.LiveKitTokenRequest{URL: "wss://lk.example.org"})
	if err == nil {
		t.Fatal("want error")
	}
	assertClean(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = g.RTCTransports(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel not preserved: %v", err)
	}
	assertClean(t, err)
}

func TestCapabilitiesCache(t *testing.T) {
	sharedCaps = &capsCache{}
	now := time.Unix(1000, 0)
	capsNow = func() time.Time { return now }
	defer func() { capsNow = time.Now; sharedCaps = &capsCache{} }()

	fail := false
	f := newFakeHS(t, func(r recReq) (int, any) {
		if fail {
			return 500, errBody("M_UNKNOWN", "boom")
		}
		return 200, map[string]any{"versions": []string{"v1.11"}, "unstable_features": map[string]bool{matrixrtc.FeatureSticky: true, matrixrtc.FeatureDelayed: false}}
	})
	g1, g2 := f.intent(t), f.intent(t) // two ghosts of one bridge share the cache
	caps := g1.Capabilities(context.Background())
	if !caps[matrixrtc.FeatureSticky] || caps[matrixrtc.FeatureDelayed] {
		t.Errorf("caps %v", caps)
	}
	caps[matrixrtc.FeatureSticky] = false // a copy: must not poison the cache
	if !g2.Capabilities(context.Background())[matrixrtc.FeatureSticky] {
		t.Error("cache returned a shared map")
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("%d /versions requests, want 1", n)
	}
	if r := f.requests()[0]; r.Path != "/_matrix/client/versions" {
		t.Errorf("path %s", r.Path)
	}
	requireMasquerade(t, f.requests())

	now = now.Add(9 * time.Minute)
	g1.Capabilities(context.Background())
	if n := len(f.requests()); n != 1 {
		t.Fatalf("refetched before TTL: %d", n)
	}
	now = now.Add(2 * time.Minute)
	fail = true
	if got := g1.Capabilities(context.Background()); len(got) != 0 {
		t.Errorf("error must give an empty map, got %v", got)
	}
	if n := len(f.requests()); n != 2 {
		t.Fatalf("no refetch after TTL: %d", n)
	}
	// Failure is cached briefly, then retried.
	g2.Capabilities(context.Background())
	if n := len(f.requests()); n != 2 {
		t.Fatalf("error not cached: %d", n)
	}
	fail = false
	now = now.Add(capsErrTTL + time.Second)
	if !g2.Capabilities(context.Background())[matrixrtc.FeatureSticky] {
		t.Error("did not recover")
	}
}

func TestCapabilitiesRace(t *testing.T) {
	sharedCaps = &capsCache{}
	defer func() { sharedCaps = &capsCache{} }()
	f := newFakeHS(t, func(r recReq) (int, any) {
		return 200, map[string]any{"unstable_features": map[string]bool{matrixrtc.FeatureSticky: true}}
	})
	g := f.intent(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = g.Capabilities(context.Background()) }()
	}
	wg.Wait()
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d requests, want 1 (single flight)", n)
	}
}
