package rtc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

const transportsSuccessTTL = time.Hour

var errTransportsUnparsable = errors.New("rtc transports response not understood")

type transportsFlight struct {
	done chan struct{}
	ts   []matrixrtc.Transport
	err  error
}

// TransportResolver caches the homeserver's MSC4143 transports (GhostRTCIntent.RTCTransports): successes
// for 1 h, failures for 60 s, one in-flight fetch shared by concurrent callers. The cache is not keyed by
// ghost: the list is the homeserver's, not the user's.
type TransportResolver struct {
	clk clock.Clock

	mu       sync.Mutex
	ts       []matrixrtc.Transport
	okUntil  time.Time
	err      error
	errUntil time.Time
	inflight *transportsFlight
}

// NewTransportResolver returns a resolver; clk nil = clock.Real().
func NewTransportResolver(clk clock.Clock) *TransportResolver {
	if clk == nil {
		clk = clock.Real()
	}
	return &TransportResolver{clk: clk}
}

// Get returns the (possibly cached) transports. The returned slice is a copy.
func (r *TransportResolver) Get(ctx context.Context, g core.GhostRTCIntent) ([]matrixrtc.Transport, error) {
	r.mu.Lock()
	now := r.clk.Now()
	if r.okUntil.After(now) {
		out := append([]matrixrtc.Transport(nil), r.ts...)
		r.mu.Unlock()
		return out, nil
	}
	if r.err != nil && now.Before(r.errUntil) {
		err := r.err
		r.mu.Unlock()
		return nil, err
	}
	f := r.inflight
	if f == nil {
		f = &transportsFlight{done: make(chan struct{})}
		r.inflight = f
		go r.fetch(context.WithoutCancel(ctx), g, f)
	}
	r.mu.Unlock()
	select {
	case <-f.done:
		return append([]matrixrtc.Transport(nil), f.ts...), f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *TransportResolver) fetch(ctx context.Context, g core.GhostRTCIntent, f *transportsFlight) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	var ts []matrixrtc.Transport
	body, err := g.RTCTransports(ctx)
	if err == nil {
		var ok bool
		if ts, ok = matrixrtc.ParseTransports(body); !ok {
			err = errTransportsUnparsable
		}
	}
	r.mu.Lock()
	f.ts, f.err = ts, err
	now := r.clk.Now()
	if err != nil {
		r.err, r.errUntil = err, now.Add(negativeTTL)
	} else {
		r.ts, r.okUntil, r.err = ts, now.Add(transportsSuccessTTL), nil
	}
	r.inflight = nil
	r.mu.Unlock()
	close(f.done)
}

// SelectLiveKit picks the LiveKit transport: type "m.livekit" first, then "livekit"; the first one of the
// type that has a url or a service url. url is the SFU URL (MSC4195 hashed identity), serviceURL the
// lk-jwt-service base (legacy identity). Both empty when there is none.
func SelectLiveKit(ts []matrixrtc.Transport) (url, serviceURL string) {
	for _, typ := range []string{matrixrtc.FocusTypeLiveKitStable, matrixrtc.FocusTypeLiveKit} {
		for _, t := range ts {
			if t.Type == typ && (t.URL != "" || t.ServiceURL != "") {
				return t.URL, t.ServiceURL
			}
		}
	}
	return "", ""
}
