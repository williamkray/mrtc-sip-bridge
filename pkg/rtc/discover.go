package rtc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

const (
	discoverTimeout = 5 * time.Second
	negativeTTL     = 60 * time.Second
)

var errNoFoci = errors.New("no " + matrixrtc.WellKnownRTCFociKey + " livekit entry in .well-known; set network.livekit_service_url")

type flight struct {
	done chan struct{}
	url  string
	err  error
}

// resolverState is the unexported state of ServiceURLResolver.
type resolverState struct {
	override   string
	serverName string
	hc         *http.Client
	clk        clock.Clock
	base       string // test hook: replaces "https://<serverName>"

	mu       sync.Mutex
	url      string // cached success
	err      error  // cached failure
	errUntil time.Time
	inflight *flight
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: obs.WrapHTTPTransport(http.DefaultTransport, PathTemplate)}
}

func newServiceURLResolver(override, serverName string, hc *http.Client, clk clock.Clock) *ServiceURLResolver {
	if hc == nil {
		hc = defaultHTTPClient()
	}
	if clk == nil {
		clk = clock.Real()
	}
	return &ServiceURLResolver{resolverState{
		override:   override,
		serverName: serverName,
		hc:         hc,
		clk:        clk,
	}}
}

func (r *ServiceURLResolver) get(ctx context.Context) (string, error) {
	if r.override != "" {
		return r.override, nil
	}
	r.mu.Lock()
	if r.url != "" {
		u := r.url
		r.mu.Unlock()
		return u, nil
	}
	if r.err != nil && r.clk.Now().Before(r.errUntil) {
		err := r.err
		r.mu.Unlock()
		return "", err
	}
	f := r.inflight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		r.inflight = f
		// The fetch outlives a cancelled first caller (others share it) and is bounded by discoverTimeout.
		go r.fetch(context.WithoutCancel(ctx), f)
	}
	r.mu.Unlock()
	select {
	case <-f.done:
		return f.url, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (r *ServiceURLResolver) fetch(ctx context.Context, f *flight) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	u, err := r.fetchWellKnown(ctx)
	r.mu.Lock()
	f.url, f.err = u, err
	if err != nil {
		r.err, r.errUntil = err, r.clk.Now().Add(negativeTTL)
	} else {
		r.url, r.err = u, nil
	}
	r.inflight = nil
	r.mu.Unlock()
	close(f.done)
}

func (r *ServiceURLResolver) fetchWellKnown(ctx context.Context) (string, error) {
	base := r.base
	if base == "" {
		base = "https://" + r.serverName
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/matrix/client", nil)
	if err != nil {
		return "", fmt.Errorf("well-known: %s", obs.Scrub(err.Error()))
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("well-known: %s", obs.Scrub(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("well-known: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("well-known: %s", obs.Scrub(err.Error()))
	}
	u, ok := matrixrtc.ParseRTCFoci(raw)
	if !ok {
		return "", errNoFoci
	}
	return u, nil
}

func (r *ServiceURLResolver) source() string {
	if r.override != "" {
		return "config"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.url != "" {
		return "well_known"
	}
	return ""
}

func liveKitRoomName(roomID id.RoomID) string {
	return matrixrtc.LiveKitRoomAlias(roomID, matrixrtc.SlotIDRoom)
}

// ownerMatcher decides which LiveKit identities are owner devices: an exact identity set (MSC4195 hashed
// identities, extended at runtime) plus the legacy prefix "<owner>:". Safe for concurrent use.
type ownerMatcher struct {
	prefix string // "" = no legacy prefix

	mu  sync.RWMutex
	ids map[string]struct{}
}

func newOwnerMatcher(owner id.UserID, ids []string) *ownerMatcher {
	m := &ownerMatcher{ids: make(map[string]struct{}, len(ids))}
	if owner != "" {
		m.prefix = string(owner) + ":"
	}
	m.add(ids...)
	return m
}

func (m *ownerMatcher) add(ids ...string) {
	m.mu.Lock()
	for _, i := range ids {
		if i != "" {
			m.ids[i] = struct{}{}
		}
	}
	m.mu.Unlock()
}

func (m *ownerMatcher) match(identity string) bool {
	if m.prefix != "" && strings.HasPrefix(identity, m.prefix) {
		return true
	}
	m.mu.RLock()
	_, ok := m.ids[identity]
	m.mu.RUnlock()
	return ok
}
