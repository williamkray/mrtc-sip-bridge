package calls

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// Config configures a Manager.
type Config struct {
	Host               core.CallHost
	Joiner             rtc.Joiner
	ServiceURL         func(ctx context.Context) (string, error) // e.g. (*rtc.ServiceURLResolver).Get
	RingTimeout        time.Duration                             // ring lifetime and timer [30 s]
	RTPTimeout         time.Duration                             // passed to media.Config [60 s]
	DefaultCountryCode string                                    // for phone.Normalize ["1"]
	Clock              clock.Clock                               // [clock.Real()]
	Log                zerolog.Logger                            // clean logger
	// StartMedia starts the audio bridge; nil = media.Start. Tests inject fakes.
	StartMedia func(cfg media.Config, sip media.SIPLeg, rtc media.RTCLeg) (media.Bridge, error)

	// Dial places an outbound SIP call from login's account; nil disables outbound calls.
	Dial func(login core.LoginRef, req sipua.DialRequest) (sipua.OutboundCall, error)
	// LoginNumber returns login's own E.164 number (span attribute, self-dial guard); nil = unknown.
	LoginNumber func(login core.LoginRef) string
	// DialTimeout is INVITE sent -> 200 OK, then CANCEL [60 s].
	DialTimeout time.Duration
	// OutboundTrigger selects the owner-initiated trigger: "ring" (default), "membership" or "off".
	OutboundTrigger string
	// DialAllowedPrefixes lists the allowed E.164 prefixes; empty = any.
	DialAllowedPrefixes []string
	// DialNumberFormat is the Request-URI user format passed to sipua.
	DialNumberFormat sipua.NumberFormat
	// AllowSelfDial permits dialling the login's own number.
	AllowSelfDial bool

	// MembershipFormat selects the ghost's membership format: "auto" (default: mirror the owner's
	// dialect, see format.go), "legacy", "sticky" or "dual". Anything else is treated as "auto".
	MembershipFormat string
	// LiveKitAuth selects how the ghost gets its LiveKit token: "auto" (default: MSC4195 get_token
	// first, sfu_get on a token error), "sfu_get" or "msc4195". Anything else is treated as "auto".
	LiveKitAuth string
	// Transports returns the homeserver's MSC4143 transports (e.g. (*rtc.TransportResolver).Get); nil =
	// none known, so MSC4195 is only used for owner-mirrored calls.
	Transports func(ctx context.Context, g core.GhostRTCIntent) ([]matrixrtc.Transport, error)
}

// PlaceCallRequest is a command-initiated outbound call. Exactly one of Number and RoomID is set.
type PlaceCallRequest struct {
	Number string    // any form phone.Normalize accepts
	RoomID id.RoomID // an existing portal room of the login (the command was sent there)
}

// Errors returned by Manager.PlaceCall, Hangup and SendDTMF. SendDTMF also returns
// media.ErrDTMFUnsupported and media.ErrDTMFBusy as-is.
var (
	ErrOutboundDisabled = errors.New("calls: outbound calls are disabled")
	ErrInvalidNumber    = errors.New("calls: not a dialable phone number")
	ErrNotAllowed       = errors.New("calls: destination not allowed by dial_allowed_prefixes")
	ErrSelfDial         = errors.New("calls: that is this account's own number")
	ErrLineBusy         = errors.New("calls: another call is in progress")
	ErrNoActiveCall     = errors.New("calls: no active call")
	ErrNotConnected     = errors.New("calls: the call is not connected yet")
	ErrInvalidDigits    = errors.New("calls: digits must be 1-32 of 0-9 * # A-D")
)

// Manager owns all calls of all logins. Safe for concurrent use.
type Manager struct {
	cfg Config
	log zerolog.Logger

	mode      string // owner-trigger detector mode (modeRing, modeMembership, modeOff)
	startTone func(cfg media.Config, rtc media.RTCLeg, p media.TonePattern) (media.Tone, error)

	mu       sync.Mutex
	stopped  bool
	calls    map[*call]struct{}      // every live actor (busy ones included)
	active   map[core.LoginRef]*call // the one non-busy call per login
	routable map[core.LoginRef]*call // active calls whose room is resolved

	// Owner-trigger detector state (detect.go), bounded and pruned on insert.
	ownerLive    map[core.LoginRef]map[id.RoomID]map[string]memberSeen
	pendingNotif map[core.LoginRef]map[id.RoomID]notifSeen
	cooldown     map[id.RoomID]time.Time // no owner trigger in this room before the time

	dialect map[core.LoginRef]Dialect // owner client dialect per login (dialect.go)

	stickyOff map[core.LoginRef]time.Time // sticky sends refused (ErrUnsupported) until the time (format.go)
	tokenOff  map[core.LoginRef]time.Time // MSC4195 get_token failed until the time (format.go)
}

var _ core.RTCEventSink = (*Manager)(nil)

// NewManager returns a Manager with the defaults of Config applied.
func NewManager(cfg Config) *Manager {
	if cfg.RingTimeout <= 0 {
		cfg.RingTimeout = 30 * time.Second
	}
	if cfg.RTPTimeout <= 0 {
		cfg.RTPTimeout = 60 * time.Second
	}
	if cfg.DefaultCountryCode == "" {
		cfg.DefaultCountryCode = "1"
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.StartMedia == nil {
		cfg.StartMedia = media.Start
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	cfg.MembershipFormat, cfg.LiveKitAuth = normalizeFormat(cfg.MembershipFormat), normalizeAuth(cfg.LiveKitAuth)
	m := &Manager{
		cfg:          cfg,
		log:          cfg.Log.With().Str("component", "calls").Logger(),
		startTone:    media.StartTone,
		calls:        make(map[*call]struct{}),
		active:       make(map[core.LoginRef]*call),
		routable:     make(map[core.LoginRef]*call),
		ownerLive:    make(map[core.LoginRef]map[id.RoomID]map[string]memberSeen),
		pendingNotif: make(map[core.LoginRef]map[id.RoomID]notifSeen),
		cooldown:     make(map[id.RoomID]time.Time),
		dialect:      make(map[core.LoginRef]Dialect),
		stickyOff:    make(map[core.LoginRef]time.Time),
		tokenOff:     make(map[core.LoginRef]time.Time),
	}
	switch cfg.OutboundTrigger {
	case "", modeRing:
		m.mode = modeRing
	case modeMembership, modeOff:
		m.mode = cfg.OutboundTrigger
	default:
		m.log.Warn().Str("outbound_trigger", cfg.OutboundTrigger).Msg("Unknown outbound trigger mode, owner-initiated calls are off")
		m.mode = modeOff
	}
	return m
}

// HandleInvite takes ownership of a new inbound call for login. localE164 is the login's own number
// (span attribute only). Non-blocking: starts the call actor and returns (it runs on the diago handler
// goroutine).
func (m *Manager) HandleInvite(login core.LoginRef, localE164 string, sc sipua.InboundCall) {
	c := newCall(m, login, localE164, sc)
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		sc.Reject(480, "Temporarily Unavailable")
		c.log.Info().Msg("Rejected inbound call: call manager stopped")
		return
	}
	if m.active[login] != nil {
		c.busy = true
	} else {
		m.active[login] = c
	}
	m.calls[c] = struct{}{}
	m.mu.Unlock()
	go c.run()
}

// HandleRTCEvent routes a MatrixRTC event to the login's active call, or feeds the owner-trigger
// detector (detect.go). Non-blocking and without I/O (appservice path): a full call channel drops the
// event with a Warn log. Sets matrixrtc.routed_to (call|none) and call.id on the ingest span in ctx.
func (m *Manager) HandleRTCEvent(ctx context.Context, login core.LoginRef, evt matrixrtc.Event) {
	var kind eventKind
	switch evt.Kind {
	case matrixrtc.KindCallMember, matrixrtc.KindRTCMember:
		kind = evOwnerMembership
	case matrixrtc.KindRTCSlot:
		return // slot state is never routed and never a trigger (the ghost reads slots itself)
	case matrixrtc.KindRTCDecline:
		kind = evDecline
	case matrixrtc.KindRTCNotification:
		if n, ok := matrixrtc.ParseNotification(evt); ok && n.Withdrawn {
			m.withdrawNotif(login, evt)
			return
		}
	default:
		return
	}
	m.mu.Lock()
	c := m.routable[login]
	m.mu.Unlock()
	// roomID is written once before setRoutable (under m.mu), so reading it here is ordered.
	if c == nil || evt.RoomID != c.roomID {
		m.detect(ctx, login, evt)
		return
	}
	if kind == evOwnerMembership {
		// Keep the detector's view of the owner's devices current while a call owns the room.
		m.noteRoutedMembership(login, evt)
	}
	if kind == 0 {
		return // notifications in the active call's room are never a new trigger (edge case 5)
	}
	span := trace.SpanFromContext(ctx)
	select {
	case c.events <- event{kind: kind, rtc: evt, from: span.SpanContext()}:
		span.SetAttributes(obs.AttrMatrixRTCRoutedTo.String("call"), obs.AttrCallID.String(c.id))
	default:
		obs.Logger(ctx, &c.log).Warn().
			Stringer("kind", evt.Kind).
			Stringer("event_id", evt.EventID).
			Msg("Call event queue full, dropping MatrixRTC event")
	}
}

// EndLogin tears down the login's calls (hangup.by=bridge, end_reason=bridge_stop) and waits for them
// until ctx is done. Call before stopping the login's SIP UA so BYE/480 can still be sent.
func (m *Manager) EndLogin(ctx context.Context, login core.LoginRef) {
	m.mu.Lock()
	var list []*call
	for c := range m.calls {
		if c.login == login {
			list = append(list, c)
		}
	}
	delete(m.ownerLive, login)
	delete(m.pendingNotif, login)
	m.mu.Unlock()
	endCalls(ctx, list)
}

// Stop ends every call like EndLogin and makes HandleInvite reject new calls with 480. Idempotent.
func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	m.stopped = true
	list := make([]*call, 0, len(m.calls))
	for c := range m.calls {
		list = append(list, c)
	}
	m.mu.Unlock()
	endCalls(ctx, list)
}

// endCalls signals shutdown to every call, then waits for each until ctx is done.
func endCalls(ctx context.Context, list []*call) {
	for _, c := range list {
		c.shutdown()
	}
	for _, c := range list {
		select {
		case <-c.done:
		case <-ctx.Done():
			return
		}
	}
}

// setRoutable makes c receive Matrix events for its room (c.roomID must be set). Busy calls never
// become routable.
func (m *Manager) setRoutable(c *call) {
	m.mu.Lock()
	if m.active[c.login] == c {
		m.routable[c.login] = c
	}
	m.mu.Unlock()
}

// releaseActive frees the login's active/routable slots as soon as c starts ending, so a quick redial
// is accepted while the (slow, best-effort) cleanup is still running. c stays in m.calls until remove.
// It also starts the room's owner-trigger cooldown.
func (m *Manager) releaseActive(c *call) {
	now := m.cfg.Clock.Now()
	m.mu.Lock()
	m.setCooldownLocked(c.roomID, now)
	if m.active[c.login] == c {
		delete(m.active, c.login)
	}
	if m.routable[c.login] == c {
		delete(m.routable, c.login)
	}
	m.mu.Unlock()
}

// remove forgets c (end of its actor) and restarts the room's owner-trigger cooldown.
func (m *Manager) remove(c *call) {
	now := m.cfg.Clock.Now()
	m.mu.Lock()
	m.setCooldownLocked(c.roomID, now)
	delete(m.calls, c)
	if m.active[c.login] == c {
		delete(m.active, c.login)
	}
	if m.routable[c.login] == c {
		delete(m.routable, c.login)
	}
	m.mu.Unlock()
}
