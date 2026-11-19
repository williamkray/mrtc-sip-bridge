package calls

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	mevent "maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
)

// Config.MembershipFormat values.
const (
	fmtAuto   = "auto"
	fmtLegacy = "legacy"
	fmtSticky = "sticky"
	fmtDual   = "dual"
)

// Config.LiveKitAuth values.
const (
	authAuto    = "auto"
	authSFUGet  = "sfu_get"
	authMSC4195 = "msc4195"
)

func normalizeFormat(s string) string {
	switch s {
	case fmtLegacy, fmtSticky, fmtDual:
		return s
	}
	return fmtAuto
}

func normalizeAuth(s string) string {
	switch s {
	case authSFUGet, authMSC4195:
		return s
	}
	return authAuto
}

// msc4195FailedTTL is how long a login skips MSC4195 get_token after an attempt failed before the
// LiveKit connect (and the call fell back to sfu_get).
const msc4195FailedTTL = 10 * time.Minute

// stickyUnsupportedTTL is how long a login stays legacy-only after a sticky send was refused
// (core.ErrUnsupported).
const stickyUnsupportedTTL = 10 * time.Minute

// matrixrtc.plan.reason values (closed enum): which row of MATRIXRTC2-SPEC §2 decided.
const (
	reasonConfigLegacy   = "config_legacy"
	reasonNoRTCIntent    = "no_rtc_intent"
	reasonNoSticky       = "no_sticky_support"
	reasonStickyRecently = "sticky_unsupported_recently"
	reasonOwnerLegacy    = "owner_legacy"
	reasonOwnerMirror    = "owner_sticky_mirror"
	reasonBridgeDialect  = "bridge_dialect"
	reasonBridgeStable   = "bridge_stable" // no dialect yet, auto: stable sticky names, legacy ring, sfu_get
	reasonConfigSticky   = "config_sticky"
	reasonConfigDual     = "config_dual"
	// reasonMSC4195Failed: MSC4195 would have been used but get_token failed for this login within
	// msc4195FailedTTL (auto only), so the call goes straight to sfu_get.
	reasonMSC4195Failed = "msc4195_failed_recently"
)

// matrixrtc.transports.status values.
const (
	transportsNotNeeded = "not_needed"
	transportsOK        = "ok"
	transportsNoURL     = "no_livekit_url"
	transportsError     = "error"
)

// plan is the ghost's publishing plan for one call (MATRIXRTC2-SPEC §2): which membership formats,
// which sticky dialect, how the LiveKit token is obtained and whether the ring is sticky.
type plan struct {
	legacy     bool          // publish the legacy call.member state event
	sticky     bool          // publish the sticky rtc.member event
	typ        mevent.Type   // sticky member event type (the dialect); zero unless sticky
	slot       string        // slot_id of the sticky membership and the ring; legacy plans: SlotIDRoomLegacy
	slots      []string      // slot spellings to open (ensureSlots); first = slot
	slotType   mevent.Type   // slot state event type matching typ
	slotTypes  []mevent.Type // slot state event types to open: bridge plans both, a mirror plan only slotType
	ringSlot   string        // slot_id of the ring; "" = slot
	auth       rtc.AuthMode
	url        string // LiveKit SFU url for MSC4195 ("" with AuthSFUGet)
	serviceURL string // lk-jwt-service url, for the livekit_service_url transport
	fallback   bool   // MSC4195 may fall back to sfu_get on a token error (auto)
	ringSticky bool   // the ring is the union shape and sent sticky (only when sticky)
	delayed    bool   // delayed events are supported (FeatureDelayed)
	mirror     bool   // sticky only, mirroring the owner's membership
	reason     string // reason* constant
}

// formatName is the matrixrtc.plan.format value.
func (p plan) formatName() string {
	switch {
	case p.sticky && p.legacy:
		return fmtDual
	case p.sticky:
		return fmtSticky
	default:
		return fmtLegacy
	}
}

// planInputs are all inputs of decidePlan, gathered by choosePlan (the only I/O).
type planInputs struct {
	ownerInit   bool // owner-initiated outbound call (the ghost is the callee)
	ownerFormat matrixrtc.Format
	ownerMem    matrixrtc.Membership
	dialect     Dialect
	hasDialect  bool
	format      string // Config.MembershipFormat (normalized)
	auth        string // Config.LiveKitAuth (normalized)
	rtcIntent   bool   // the ghost implements core.GhostRTCIntent
	stickyOK    bool   // FeatureSticky advertised
	stickyBlock bool   // a sticky send was refused for this login recently
	tokenBlock  bool   // MSC4195 get_token failed for this login recently (auto falls back at once)
	delayedOK   bool   // FeatureDelayed advertised
	// transportURL is the LiveKit url from /rtc/transports ("" when not fetched, failed or none).
	transportURL string
	serviceURL   string
}

// wantTransports reports whether decidePlan could use a transports url, so choosePlan only fetches
// /rtc/transports when it matters.
func wantTransports(in planInputs) bool {
	if in.format == fmtLegacy || !in.rtcIntent || !in.stickyOK || in.stickyBlock || in.auth == authSFUGet {
		return false
	}
	if in.tokenBlock && in.auth == authAuto {
		return false // MSC4195 is paused: no url needed
	}
	if in.ownerInit && in.ownerFormat == matrixrtc.FormatSticky && in.ownerMem.Key != "" {
		return false // mirror plan: the owner's own transport url is used, never /rtc/transports
	}
	if in.format == fmtAuto && (in.ownerInit || (!in.hasDialect && in.auth == authAuto)) {
		return false // legacy owner: legacy plan; no dialect yet: sfu_get (see decidePlan)
	}
	return true
}

// stickyMemberType maps a dialect type name to the sticky member event type; unknown names give the
// stable one.
func stickyMemberType(name string) mevent.Type {
	if name == matrixrtc.RTCMember.Type {
		return matrixrtc.RTCMember
	}
	return matrixrtc.RTCMemberAlias
}

// slotTypeFor returns the rtc.slot state type that goes with the member type typ.
func slotTypeFor(typ mevent.Type) mevent.Type {
	if typ == matrixrtc.RTCMember {
		return matrixrtc.RTCSlot
	}
	return matrixrtc.RTCSlotAlias
}

// otherMemberType returns the other sticky member event type.
func otherMemberType(t mevent.Type) mevent.Type {
	if t == matrixrtc.RTCMember {
		return matrixrtc.RTCMemberAlias
	}
	return matrixrtc.RTCMember
}

// otherSlotSpelling returns the other room slot id spelling.
func otherSlotSpelling(s string) string {
	if s == matrixrtc.SlotIDRoomLegacy {
		return matrixrtc.SlotIDRoom
	}
	return matrixrtc.SlotIDRoomLegacy
}

// decidePlan implements the MATRIXRTC2-SPEC §2 decision table. It is pure.
func decidePlan(in planInputs) plan {
	base := plan{legacy: true, slot: matrixrtc.SlotIDRoomLegacy, auth: rtc.AuthSFUGet, serviceURL: in.serviceURL, delayed: in.delayedOK}
	switch {
	case in.format == fmtLegacy:
		base.reason = reasonConfigLegacy
		return base
	case !in.rtcIntent:
		base.reason = reasonNoRTCIntent
		return base
	case !in.stickyOK:
		base.reason = reasonNoSticky
		return base
	case in.stickyBlock:
		base.reason = reasonStickyRecently
		return base
	}
	mirror := in.ownerInit && in.ownerFormat == matrixrtc.FormatSticky && in.ownerMem.Key != ""
	if in.format == fmtAuto && in.ownerInit && !mirror {
		base.reason = reasonOwnerLegacy // the owner uses (or we do not know) the legacy format: unchanged
		return base
	}

	p := plan{sticky: true, serviceURL: in.serviceURL, delayed: in.delayedOK, ringSticky: true, mirror: mirror}
	switch in.format {
	case fmtSticky:
		p.reason = reasonConfigSticky
	case fmtDual:
		p.legacy, p.reason = true, reasonConfigDual
	default: // auto: owner-initiated mirror is sticky only, bridge-initiated is dual
		p.legacy = !in.ownerInit
	}
	// Bridge-initiated, auto, and no client has shown the new format yet (no dialect): behave as before
	// for every client that does not know it. The legacy membership content stays byte-identical, the
	// ring keeps the legacy shape (slot m.call#ROOM, m.relates_to, not sticky), the token comes from
	// sfu_get and the sticky membership names a livekit_service_url transport (identity "<ghost>:<DEV>").
	// Only the extra sticky membership and the opened slots are new.
	legacyShape := in.format == fmtAuto && !mirror && !in.hasDialect
	url := ""
	switch {
	case mirror:
		p.typ, p.slot, url = stickyMemberType(in.ownerMem.TypeName), in.ownerMem.SlotID, in.ownerMem.PreferredLiveKitURL()
		p.slots = []string{p.slot}
		p.slotTypes = []mevent.Type{slotTypeFor(p.typ)}
		if p.reason == "" {
			p.reason = reasonOwnerMirror
		}
	default:
		p.typ, p.slot = matrixrtc.RTCMemberAlias, matrixrtc.SlotIDRoom
		if in.hasDialect {
			p.typ = stickyMemberType(in.dialect.TypeName)
			if matrixrtc.IsRoomSlot(in.dialect.SlotID) {
				p.slot = in.dialect.SlotID
			}
			if p.reason == "" {
				p.reason = reasonBridgeDialect
			}
		} else if p.reason == "" {
			p.reason = reasonBridgeStable
		}
		p.slots = []string{p.slot, otherSlotSpelling(p.slot)}
		p.slotTypes = []mevent.Type{slotTypeFor(p.typ), slotTypeFor(otherMemberType(p.typ))}
		url = in.transportURL
		if legacyShape {
			p.ringSticky, p.ringSlot = false, matrixrtc.SlotIDRoomLegacy
			if in.auth == authAuto {
				url = "" // an explicit livekit_auth: msc4195 is honoured
			}
		}
	}
	p.slotType = slotTypeFor(p.typ)

	p.auth = rtc.AuthSFUGet
	switch {
	case in.tokenBlock && in.auth == authAuto && !legacyShape:
		// MSC4195 is paused for this login (no transports lookup was made): straight to sfu_get.
		p.reason = reasonMSC4195Failed
	case url != "" && in.auth != authSFUGet:
		p.auth, p.url = rtc.AuthMSC4195, url
		p.fallback = in.auth == authAuto
	}
	return p
}

// rtcGhost returns the call ghost's MatrixRTC 2.0 capability, if it has one.
func (c *call) rtcGhost() (core.GhostRTCIntent, bool) {
	if c.room == nil || c.room.Ghost == nil {
		return nil, false
	}
	g, ok := c.room.Ghost.(core.GhostRTCIntent)
	return g, ok
}

// choosePlan gathers the plan inputs (capabilities and, when useful, transports: both cached by the
// ghost / resolver) and runs decidePlan. Every failure degrades towards the legacy plan. A call whose
// ghost has no GhostRTCIntent decides silently (legacy, nothing to observe); otherwise the decision is
// recorded in a matrixrtc.plan.choose child span and an Info log.
func (c *call) choosePlan(ctx context.Context) {
	in := planInputs{
		ownerInit:   c.isOutbound() && c.trigger == triggerOwnerMembership,
		ownerFormat: c.ownerFormat,
		ownerMem:    c.ownerMem,
		format:      c.cfg.MembershipFormat,
		auth:        c.cfg.LiveKitAuth,
		serviceURL:  c.serviceURL,
	}
	in.dialect, in.hasDialect = c.m.dialectFor(c.login)
	g, ok := c.rtcGhost()
	in.rtcIntent = ok
	if !ok {
		c.plan = decidePlan(in)
		return
	}
	sctx, span := c.child(ctx, "matrixrtc.plan.choose", trace.SpanKindInternal, obs.AttrCallID.String(c.id))
	transports := transportsNotNeeded
	if in.format != fmtLegacy {
		cctx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
		caps := g.Capabilities(cctx)
		cancel()
		in.stickyOK, in.delayedOK = caps[matrixrtc.FeatureSticky], caps[matrixrtc.FeatureDelayed]
		in.stickyBlock = in.stickyOK && c.m.stickyBlocked(c.login)
		in.tokenBlock = c.m.tokenBlocked(c.login)
		if wantTransports(in) {
			transports = transportsNoURL
			if c.cfg.Transports != nil {
				tctx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
				ts, err := c.cfg.Transports(tctx, g)
				cancel()
				if err != nil {
					transports = transportsError
					c.logFor(sctx).Debug().Str("error", scrubErr(err)).Msg("MatrixRTC transports not available")
				} else if u, _ := rtc.SelectLiveKit(ts); u != "" {
					in.transportURL, transports = u, transportsOK
				}
			}
		}
	}
	c.plan = decidePlan(in)

	attrs := []attribute.KeyValue{
		obs.AttrMatrixRTCPlanFormat.String(c.plan.formatName()),
		obs.AttrMatrixRTCPlanReason.String(c.plan.reason),
		obs.AttrMatrixRTCStickySupported.Bool(in.stickyOK && !in.stickyBlock),
		obs.AttrMatrixRTCDelayedSupported.Bool(in.delayedOK),
		obs.AttrMatrixRTCTransports.String(transports),
		obs.AttrRTCTokenEndpoint.String(tokenEndpointName(c.plan.auth)),
	}
	if c.plan.sticky {
		attrs = append(attrs, obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(c.plan.slot)), obs.AttrMatrixRTCMemberType.String(c.plan.typ.Type))
	}
	span.SetAttributes(attrs...)
	c.endSpan(span)
	c.setupSpan.SetAttributes(obs.AttrMatrixRTCPlanFormat.String(c.plan.formatName()),
		obs.AttrMatrixRTCPlanReason.String(c.plan.reason))
	c.logFor(ctx).Info().Str("format", c.plan.formatName()).Str("reason", c.plan.reason).
		Str("livekit_auth", tokenEndpointName(c.plan.auth)).Bool("delayed_leave", c.plan.sticky && c.plan.delayed).
		Msg("MatrixRTC membership plan")
}

// tokenEndpointName is the rtc.token.endpoint value of an auth mode.
func tokenEndpointName(a rtc.AuthMode) string {
	if a == rtc.AuthMSC4195 {
		return "get_token_cs"
	}
	return "sfu_get"
}

// stickyBlocked reports whether sticky sends are paused for login (a send returned ErrUnsupported
// within stickyUnsupportedTTL).
func (m *Manager) stickyBlocked(login core.LoginRef) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.stickyOff[login]
	if !ok {
		return false
	}
	if !m.cfg.Clock.Now().Before(until) {
		delete(m.stickyOff, login)
		return false
	}
	return true
}

// tokenBlocked reports whether MSC4195 get_token is paused for login (an attempt failed before the
// LiveKit connect within msc4195FailedTTL).
func (m *Manager) tokenBlocked(login core.LoginRef) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.tokenOff[login]
	if !ok {
		return false
	}
	if !m.cfg.Clock.Now().Before(until) {
		delete(m.tokenOff, login)
		return false
	}
	return true
}

// noteTokenFailed records that MSC4195 get_token failed for login.
func (m *Manager) noteTokenFailed(login core.LoginRef) {
	m.mu.Lock()
	m.tokenOff[login] = m.cfg.Clock.Now().Add(msc4195FailedTTL)
	m.mu.Unlock()
}

// noteStickyUnsupported records that the homeserver refused a sticky send for login.
func (m *Manager) noteStickyUnsupported(login core.LoginRef) {
	m.mu.Lock()
	m.stickyOff[login] = m.cfg.Clock.Now().Add(stickyUnsupportedTTL)
	m.mu.Unlock()
}
