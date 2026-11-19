package hostkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// ghostIntent implements core.GhostRTCIntent (docs/MATRIXRTC2-SPEC.md WP2). Requests go through the
// ghost's appservice client (g.raw.Client), so every URL carries the ?user_id= masquerade and the
// requests use the bridge's wrapped HTTP transport. The mautrix RTCTransports/RTCLivekitGetToken helpers
// are deliberately not used (spec §1.4).
var _ core.GhostRTCIntent = (*ghostIntent)(nil)

// hsError is a homeserver call failure that is safe to log: it carries the operation, HTTP status and
// errcode only. Never the response body, the URL (query strings hold user_id and parameters), tokens
// or JWTs.
type hsError struct {
	op          string
	status      int
	errCode     string
	msg         string // lower-cased server message, only used to classify a 400; never printed
	detail      string // scrubbed transport error text (status 0)
	cause       error  // kept only for context cancellation/deadline
	unsupported bool
}

func (e *hsError) Error() string {
	s := e.op + ": "
	switch {
	case e.status == 0:
		s += "request failed: " + e.detail
	case e.errCode != "":
		s += fmt.Sprintf("HTTP %d %s", e.status, e.errCode)
	default:
		s += fmt.Sprintf("HTTP %d", e.status)
	}
	if e.unsupported {
		return core.ErrUnsupported.Error() + ": " + s
	}
	return s
}

func (e *hsError) Unwrap() error { return e.cause }

// Is makes errors.Is(err, core.ErrUnsupported), errors.Is(err, core.ErrNotFound) (errcode M_NOT_FOUND)
// and errors.Is against mautrix.RespError sentinels (matched by errcode, e.g. mautrix.MNotFound) work.
func (e *hsError) Is(target error) bool {
	switch target {
	case core.ErrUnsupported:
		return e.unsupported
	case core.ErrNotFound:
		return e.errCode == "M_NOT_FOUND"
	}
	if r, ok := target.(mautrix.RespError); ok {
		return e.errCode != "" && r.ErrCode == e.errCode
	}
	return false
}

// missing reports that the endpoint itself is not there: an unrecognized request (M_UNRECOGNIZED), an
// unknown path (404 without an errcode) or a method not allowed. A 404 M_NOT_FOUND is an unknown object
// on a known endpoint (an expired delayed event, a missing room), never "unsupported".
func (e *hsError) missing() bool {
	switch {
	case e.errCode == "M_UNRECOGNIZED":
		return true
	case e.status == http.StatusNotFound:
		return e.errCode == ""
	}
	return e.status == http.StatusMethodNotAllowed
}

// paramRejected reports a 400 that rejects the sticky or delay parameter as unknown: the errcode must be
// M_UNRECOGNIZED, M_UNKNOWN or M_INVALID_PARAM and the message must name the parameter. A limit
// (M_LIMIT_EXCEEDED, M_FORBIDDEN, M_TOO_LARGE, or a message about a maximum, e.g. Synapse's M_UNKNOWN
// "requested delay exceeds the allowed maximum") is a refusal of this request, not missing support.
func (e *hsError) paramRejected() bool {
	if e.status != http.StatusBadRequest {
		return false
	}
	switch e.errCode {
	case "M_UNRECOGNIZED", "M_UNKNOWN", "M_INVALID_PARAM":
	default:
		return false
	}
	for _, k := range []string{"exceed", "maximum", "too many", "too long", "too large", "limit"} {
		if strings.Contains(e.msg, k) {
			return false
		}
	}
	for _, k := range []string{"sticky", "delay", "msc4354", "msc4140"} {
		if strings.Contains(e.msg, k) {
			return true
		}
	}
	return false
}

func asHS(err error) *hsError {
	var h *hsError
	if errors.As(err, &h) {
		return h
	}
	return nil
}

// markUnsupported flags err as ErrUnsupported when it is an hsError.
func markUnsupported(err error) error {
	if h := asHS(err); h != nil {
		h.unsupported = true
	}
	return err
}

// classify turns a mautrix error into an hsError.
func classify(op string, err error) *hsError {
	h := &hsError{op: op}
	var he mautrix.HTTPError
	switch {
	case errors.As(err, &he):
		if he.Response != nil {
			h.status = he.Response.StatusCode
		}
		if he.RespError != nil {
			h.errCode = he.RespError.ErrCode
			h.msg = strings.ToLower(he.RespError.Err)
			if h.status == 0 {
				h.status = he.RespError.StatusCode
			}
		}
		if h.status == 0 {
			h.detail = obs.Scrub(he.Message)
		}
	default:
		h.detail = obs.Scrub(err.Error())
	}
	if errors.Is(err, context.Canceled) {
		h.cause = context.Canceled
	} else if errors.Is(err, context.DeadlineExceeded) {
		h.cause = context.DeadlineExceeded
	}
	return h
}

// urlSegs splits a constant path ("/_matrix/client/v1/x") into URL segments for BuildURL.
func urlSegs(path string, extra ...any) mautrix.BaseURLPath {
	var out mautrix.BaseURLPath
	for _, s := range strings.Split(strings.Trim(path, "/"), "/") {
		out = append(out, s)
	}
	return append(out, extra...)
}

// do performs one request as the ghost. A nil error means success. Retries are disabled: the callers
// implement their own fallbacks and several requests are not idempotent.
func (g *ghostIntent) do(ctx context.Context, op, method, url string, body any, out any) ([]byte, error) {
	data, err := g.raw.Client.MakeFullRequest(ctx, mautrix.FullRequest{
		Method:           method,
		URL:              url,
		RequestJSON:      body,
		ResponseJSON:     out,
		MaxAttempts:      1,
		SensitiveContent: true,
	})
	if err != nil {
		return nil, classify(op, err)
	}
	return data, nil
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

// SendSticky implements core.GhostRTCIntent: PUT /rooms/{room}/send/{type}/{txn}?org.matrix.msc4354.sticky_duration_ms=N.
func (g *ghostIntent) SendSticky(ctx context.Context, roomID id.RoomID, t event.Type, content map[string]any, d time.Duration) (id.EventID, error) {
	var resp mautrix.RespSendEvent
	url := g.raw.Client.BuildURLWithQuery(
		mautrix.ClientURLPath{"v3", "rooms", roomID, "send", t.Type, g.raw.Client.TxnID()},
		map[string]string{matrixrtc.QueryStickyDuration: strconv.FormatInt(ms(d), 10)})
	if _, err := g.do(ctx, "send sticky event", http.MethodPut, url, content, &resp); err != nil {
		if h := asHS(err); h != nil && (h.missing() || h.paramRejected()) {
			h.unsupported = true
		}
		return "", err
	}
	return resp.EventID, nil
}

// ScheduleDelayed implements core.GhostRTCIntent. First the body endpoint
// PUT {PathDelayedEventPrefix}/rooms/{room}/delayed_event/{type}/{txn}; on 404/M_UNRECOGNIZED the
// deprecated PUT /rooms/{room}/send/{type}/{txn}?org.matrix.msc4140.delay=ms[&sticky] form.
func (g *ghostIntent) ScheduleDelayed(ctx context.Context, roomID id.RoomID, t event.Type, content map[string]any, delay, sticky time.Duration) (core.DelayedEventID, error) {
	cl := g.raw.Client
	txn := cl.TxnID()
	body := map[string]any{"delay_ms": ms(delay), "content": content}
	if sticky > 0 {
		body[matrixrtc.BodyStickyDuration] = ms(sticky)
	}
	var resp struct {
		DelayID string `json:"delay_id"`
	}
	url := cl.BuildURL(urlSegs(matrixrtc.PathDelayedEventPrefix, "rooms", roomID, "delayed_event", t.Type, txn))
	_, err := g.do(ctx, "schedule delayed event", http.MethodPut, url, body, &resp)
	if err == nil {
		if resp.DelayID == "" {
			return "", &hsError{op: "schedule delayed event", detail: "response has no delay_id", status: 0}
		}
		return core.DelayedEventID(resp.DelayID), nil
	}
	h := asHS(err)
	if h == nil || !(h.missing() || h.paramRejected()) {
		return "", err
	}

	// Deprecated query form.
	q := map[string]string{matrixrtc.QueryDelay: strconv.FormatInt(ms(delay), 10)}
	if sticky > 0 {
		q[matrixrtc.QueryStickyDuration] = strconv.FormatInt(ms(sticky), 10)
	}
	url = cl.BuildURLWithQuery(mautrix.ClientURLPath{"v3", "rooms", roomID, "send", t.Type, txn}, q)
	resp.DelayID = ""
	if _, err = g.do(ctx, "schedule delayed event (query form)", http.MethodPut, url, content, &resp); err != nil {
		if h := asHS(err); h != nil && (h.missing() || h.paramRejected()) {
			h.unsupported = true
		}
		return "", err
	}
	if resp.DelayID == "" {
		// A homeserver that ignores the delay parameter sends the event at once: not supported.
		return "", markUnsupported(&hsError{op: "schedule delayed event (query form)", detail: "response has no delay_id"})
	}
	return core.DelayedEventID(resp.DelayID), nil
}

// UpdateDelayed implements core.GhostRTCIntent: POST {prefix}/delayed_events/{id}/{action} with {};
// when the endpoint is not recognized, POST {prefix}/delayed_events/{id} with {"action":...}. An unknown
// delay id (404 M_NOT_FOUND) is returned as an error with errors.Is(err, mautrix.MNotFound) true and is
// not ErrUnsupported.
func (g *ghostIntent) UpdateDelayed(ctx context.Context, did core.DelayedEventID, action string) error {
	cl := g.raw.Client
	url := cl.BuildURL(urlSegs(matrixrtc.PathDelayedEventPrefix, "delayed_events", string(did), action))
	_, err := g.do(ctx, "update delayed event", http.MethodPost, url, map[string]any{}, nil)
	if err == nil {
		return nil
	}
	h := asHS(err)
	if h == nil || !h.missing() {
		return err
	}
	url = cl.BuildURL(urlSegs(matrixrtc.PathDelayedEventPrefix, "delayed_events", string(did)))
	if _, err = g.do(ctx, "update delayed event (legacy form)", http.MethodPost, url, map[string]any{"action": action}, nil); err != nil {
		if h := asHS(err); h != nil && h.missing() {
			h.unsupported = true
		}
		return err
	}
	return nil
}

// StateEvent implements core.GhostRTCIntent: GET /rooms/{room}/state/{type}/{key}; (nil, nil) on 404.
func (g *ghostIntent) StateEvent(ctx context.Context, roomID id.RoomID, t event.Type, stateKey string) (map[string]any, error) {
	url := g.raw.Client.BuildClientURL("v3", "rooms", roomID, "state", t.Type, stateKey)
	var out map[string]any
	if _, err := g.do(ctx, "get state event", http.MethodGet, url, nil, &out); err != nil {
		if h := asHS(err); h != nil && (h.status == http.StatusNotFound || h.errCode == "M_NOT_FOUND") {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// RTCTransports implements core.GhostRTCIntent: GET the stable path, then the unstable one when the
// stable one is not there. Returns the raw body.
func (g *ghostIntent) RTCTransports(ctx context.Context) ([]byte, error) {
	var last error
	for _, p := range []string{matrixrtc.PathRTCTransports, matrixrtc.PathRTCTransportsUnstable} {
		data, err := g.do(ctx, "get rtc transports", http.MethodGet, g.raw.Client.BuildURL(urlSegs(p)), nil, nil)
		if err == nil {
			return data, nil
		}
		h := asHS(err)
		if h == nil || !h.missing() {
			return nil, err
		}
		last = err
	}
	return nil, markUnsupported(last)
}

// LiveKitToken implements core.GhostRTCIntent: POST the stable get_token path, then the unstable one,
// body {url, room_id, slot_id, member_id}. Only the jwt is returned; it is never put into an error.
func (g *ghostIntent) LiveKitToken(ctx context.Context, req core.LiveKitTokenRequest) (string, error) {
	body := map[string]any{
		"url":       req.URL,
		"room_id":   req.RoomID,
		"slot_id":   req.SlotID,
		"member_id": req.MemberID,
	}
	var last error
	for _, p := range []string{matrixrtc.PathLiveKitGetToken, matrixrtc.PathLiveKitGetTokenUnstable} {
		var resp struct {
			JWT string `json:"jwt"`
		}
		_, err := g.do(ctx, "get livekit token", http.MethodPost, g.raw.Client.BuildURL(urlSegs(p)), body, &resp)
		if err == nil {
			if resp.JWT == "" {
				return "", &hsError{op: "get livekit token", detail: "response has no jwt"}
			}
			return resp.JWT, nil
		}
		h := asHS(err)
		if h == nil || !h.missing() {
			return "", err
		}
		last = err
	}
	return "", markUnsupported(last)
}

// Capabilities implements core.GhostRTCIntent: /versions unstable_features, cached per homeserver URL
// (shared by all ghosts of the bridge) for capsTTL; a failed fetch gives an empty map, cached for
// capsErrTTL.
func (g *ghostIntent) Capabilities(ctx context.Context) map[string]bool {
	key := ""
	if hs := g.raw.Client.HomeserverURL; hs != nil {
		key = hs.String()
	}
	return sharedCaps.get(ctx, key, func(ctx context.Context) (map[string]bool, error) {
		var resp struct {
			Unstable map[string]bool `json:"unstable_features"`
		}
		if _, err := g.do(ctx, "get versions", http.MethodGet, g.raw.Client.BuildClientURL("versions"), nil, &resp); err != nil {
			return nil, err
		}
		return resp.Unstable, nil
	})
}

var (
	capsTTL    = 10 * time.Minute
	capsErrTTL = 30 * time.Second
	capsNow    = time.Now
	sharedCaps = &capsCache{}
)

type capsEntry struct {
	mu      sync.Mutex // held during a fetch: concurrent callers wait for it instead of fetching again
	caps    map[string]bool
	expires time.Time
}

// capsCache caches the versions answer per homeserver URL.
type capsCache struct {
	mu sync.Mutex
	m  map[string]*capsEntry
}

func (c *capsCache) get(ctx context.Context, key string, fetch func(context.Context) (map[string]bool, error)) map[string]bool {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*capsEntry{}
	}
	e := c.m[key]
	if e == nil {
		e = &capsEntry{}
		c.m[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if capsNow().Before(e.expires) {
		return copyCaps(e.caps)
	}
	fctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	caps, err := fetch(fctx)
	if err != nil {
		if ctx.Err() != nil {
			return map[string]bool{} // do not cache a cancelled call
		}
		e.caps, e.expires = nil, capsNow().Add(capsErrTTL)
		return map[string]bool{}
	}
	e.caps, e.expires = caps, capsNow().Add(capsTTL)
	return copyCaps(caps)
}

func copyCaps(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
