package rtc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

type fakeRTCGhost struct {
	fakeGhost
	core.GhostRTCIntent // nil: only the methods below are used
	calls               atomic.Int32
	body                []byte
	err                 error
	gate                chan struct{}
	tokReq              core.LiveKitTokenRequest
	tokJWT              string
	tokErr              error
}

func (g *fakeRTCGhost) RTCTransports(context.Context) ([]byte, error) {
	g.calls.Add(1)
	if g.gate != nil {
		<-g.gate
	}
	return g.body, g.err
}

func (g *fakeRTCGhost) LiveKitToken(_ context.Context, r core.LiveKitTokenRequest) (string, error) {
	g.tokReq = r
	return g.tokJWT, g.tokErr
}

const transportsBody = `{"transports":[{"type":"livekit","livekit_service_url":"https://svc.test"},{"type":"m.livekit","url":"wss://lk.test"}]}`

func TestTransportResolverTTLs(t *testing.T) {
	clk := clock.NewFake(time.Unix(1000, 0))
	r := NewTransportResolver(clk)
	g := &fakeRTCGhost{err: errors.New("boom")}
	if _, err := r.Get(context.Background(), g); err == nil {
		t.Fatal("want error")
	}
	if _, err := r.Get(context.Background(), g); err == nil || g.calls.Load() != 1 {
		t.Fatalf("failure not cached: calls=%d", g.calls.Load())
	}
	clk.Advance(61 * time.Second)
	g.err, g.body = nil, []byte(transportsBody)
	ts, err := r.Get(context.Background(), g)
	if err != nil || len(ts) != 2 || g.calls.Load() != 2 {
		t.Fatalf("%v %v calls=%d", ts, err, g.calls.Load())
	}
	clk.Advance(59 * time.Minute)
	if _, err := r.Get(context.Background(), g); err != nil || g.calls.Load() != 2 {
		t.Fatalf("success not cached 1h: calls=%d", g.calls.Load())
	}
	clk.Advance(2 * time.Minute)
	if _, err := r.Get(context.Background(), g); err != nil || g.calls.Load() != 3 {
		t.Fatalf("success not expired: calls=%d", g.calls.Load())
	}
}

func TestTransportResolverUnparsable(t *testing.T) {
	r := NewTransportResolver(clock.NewFake(time.Unix(0, 0)))
	g := &fakeRTCGhost{body: []byte(`{"x":1}`)}
	if _, err := r.Get(context.Background(), g); err == nil {
		t.Fatal("want error")
	}
}

func TestTransportResolverSingleFlight(t *testing.T) {
	r := NewTransportResolver(clock.NewFake(time.Unix(0, 0)))
	g := &fakeRTCGhost{body: []byte(transportsBody), gate: make(chan struct{})}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ts, err := r.Get(context.Background(), g); err != nil || len(ts) != 2 {
				t.Errorf("%v %v", ts, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(g.gate)
	wg.Wait()
	if g.calls.Load() != 1 {
		t.Fatalf("calls=%d", g.calls.Load())
	}
}

func TestSelectLiveKit(t *testing.T) {
	ts, _ := matrixrtc.ParseTransports([]byte(transportsBody))
	if u, s := SelectLiveKit(ts); u != "wss://lk.test" || s != "" {
		t.Fatalf("%q %q", u, s)
	}
	if u, s := SelectLiveKit(ts[:1]); u != "" || s != "https://svc.test" {
		t.Fatalf("%q %q", u, s)
	}
	if u, s := SelectLiveKit(nil); u != "" || s != "" {
		t.Fatal("empty expected")
	}
}
