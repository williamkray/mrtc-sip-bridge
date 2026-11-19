package calls

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
)

// The ghost's sticky rtc.member membership (MATRIXRTC2-SPEC §3 WP5): slots, the sticky join with its
// delayed leave, the keeper that restarts the delayed leave, the 40 min refresh and the leave. The call
// actor owns the state and does the I/O inline with bounded timeouts (like publishMembership); the only
// helper goroutine is the keeper, which the actor stops (and waits for) before the leave.

// matrixrtc.slot.action values.
const (
	slotActionNoop    = "noop"
	slotActionOpened  = "opened"
	slotActionRefused = "refused_encrypted"
	slotActionFailed  = "failed"
)

// matrixrtc.member.leave_via values.
const (
	leaveViaDelayed  = "delayed_send"
	leaveViaExplicit = "explicit"
)

// matrixrtc.plan.downgrade values.
const (
	downgradeSlot        = "slot_error"
	downgradeUnsupported = "sticky_unsupported"
	downgradeError       = "sticky_error"
)

// outcome values of mrtcsip.matrixrtc.delayed_leave.restarts.
const (
	restartOK    = "ok"
	restartGone  = "gone"
	restartError = "error"
)

// maxOwnerIdentities bounds the owner LiveKit identities remembered per call.
const maxOwnerIdentities = 16

var (
	errSlotEncrypted     = errors.New("calls: the room's call slot declares encryption")
	errStickyUnsupported = errors.New("calls: homeserver refused the sticky membership")

	optRestartOK    = metric.WithAttributeSet(attribute.NewSet(obs.AttrOutcome.String(restartOK)))
	optRestartGone  = metric.WithAttributeSet(attribute.NewSet(obs.AttrOutcome.String(restartGone)))
	optRestartError = metric.WithAttributeSet(attribute.NewSet(obs.AttrOutcome.String(restartError)))
)

// stickyJoinContent builds the current sticky join content (same member id on every send).
func (c *call) stickyJoinContent() map[string]any {
	var ts []matrixrtc.Transport
	if c.plan.auth == rtc.AuthMSC4195 && c.plan.url != "" {
		ts = []matrixrtc.Transport{
			{Type: matrixrtc.FocusTypeLiveKitStable, URL: c.plan.url},
			{Type: matrixrtc.FocusTypeLiveKit, URL: c.plan.url},
		}
	} else {
		ts = []matrixrtc.Transport{{Type: matrixrtc.FocusTypeLiveKit, ServiceURL: c.serviceURL}}
	}
	return matrixrtc.StickyJoinContent(matrixrtc.StickyJoinParams{
		UserID:     c.room.Ghost.UserID(),
		DeviceID:   c.deviceID,
		MemberID:   c.memberID,
		SlotID:     c.plan.slot,
		Transports: ts,
	})
}

// ---- slots ----

// slotRef is one (state event type, slot id) pair of ensureSlots.
type slotRef struct {
	typ mevent.Type
	sid string
}

// slotPairs lists the slot state events the plan needs open: every slot spelling under every slot event
// type (bridge-initiated: both spellings, both types = 4; a mirror plan: the owner's type and spelling).
func (p plan) slotPairs() []slotRef {
	types := p.slotTypes
	if len(types) == 0 {
		types = []mevent.Type{p.slotType}
	}
	var out []slotRef
	for _, sid := range p.slots {
		for _, t := range types {
			out = append(out, slotRef{t, sid})
		}
	}
	return out
}

// slotCheck is the result of reading one slot pair.
type slotCheck struct {
	ref    slotRef
	typ    mevent.Type // type to open it under (a mirror plan keeps an existing slot's type)
	action string      // slotActionNoop, or slotActionOpened when it must be opened
	err    error
}

// ensureSlots reads every slot pair the plan needs, then opens the missing or closed ones (one
// matrixrtc.slot.open span per pair). A slot that declares encryption is never touched and refuses the
// call (errSlotEncrypted) before anything is opened. Slots are never closed.
func (c *call) ensureSlots(ctx context.Context, g core.GhostRTCIntent) error {
	// One deadline for the whole step (up to 4 reads and 4 writes): slots are optional for bridge
	// plans and must not hold up the ring on a slow homeserver.
	ctx, cancel := context.WithTimeout(ctx, matrixOpTimeout)
	defer cancel()
	pairs := c.plan.slotPairs()
	checks := make([]slotCheck, 0, len(pairs))
	// Pass 1: read everything. A read error or an encrypted slot ends the step before any write.
	for _, ref := range pairs {
		chk := c.readSlot(ctx, g, ref)
		if chk.err != nil {
			c.slotSpan(ctx, ref, slotActionOf(chk), chk.err)
			return chk.err
		}
		checks = append(checks, chk)
	}
	// Pass 2: open what is missing.
	var firstErr error
	for _, chk := range checks {
		action, err := chk.action, error(nil)
		if action == slotActionOpened {
			octx, cancel := context.WithTimeout(ctx, matrixOpTimeout)
			_, err = c.room.Ghost.SendState(octx, c.room.RoomID, chk.typ, chk.ref.sid, matrixrtc.SlotOpenContent())
			cancel()
			if err != nil {
				action = slotActionFailed
			}
		}
		c.slotSpan(ctx, chk.ref, action, err)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if !c.plan.mirror {
				break
			}
		}
	}
	return firstErr
}

func slotActionOf(chk slotCheck) string {
	if errors.Is(chk.err, errSlotEncrypted) {
		return slotActionRefused
	}
	return slotActionFailed
}

// slotSpan records the outcome for one slot pair as a matrixrtc.slot.open child span (and the log line
// of a refusal or failure).
func (c *call) slotSpan(ctx context.Context, ref slotRef, action string, err error) {
	sctx, span := c.child(ctx, "matrixrtc.slot.open", trace.SpanKindClient,
		obs.AttrCallID.String(c.id), obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(ref.sid)))
	span.SetAttributes(obs.AttrMatrixRTCSlotAction.String(action))
	if err != nil {
		if errors.Is(err, errSlotEncrypted) {
			c.logFor(sctx).Warn().Str("slot_id", matrixrtc.SlotIDLabel(ref.sid)).Msg("The room's call slot declares encryption, refusing the call")
		} else {
			obs.Fail(span, err)
			c.logFor(sctx).Warn().Str("slot_id", matrixrtc.SlotIDLabel(ref.sid)).Str("error", scrubErr(err)).
				Msg("Failed to read or open the call slot")
		}
	}
	c.endSpan(span)
}

// readSlot reads one slot pair. A bridge plan reads exactly that (type, id); a mirror plan also accepts
// the slot under the other type (the owner's client may have written either name) and then opens a
// closed one under the type it found.
func (c *call) readSlot(ctx context.Context, g core.GhostRTCIntent, ref slotRef) slotCheck {
	chk := slotCheck{ref: ref, typ: ref.typ, action: slotActionOpened}
	types := []mevent.Type{ref.typ}
	if c.plan.mirror {
		other := matrixrtc.RTCSlot
		if ref.typ == matrixrtc.RTCSlot {
			other = matrixrtc.RTCSlotAlias
		}
		types = append(types, other)
	}
	var found map[string]any
	for _, st := range types {
		octx, cancel := context.WithTimeout(ctx, matrixOpTimeout)
		content, err := g.StateEvent(octx, c.room.RoomID, st, ref.sid)
		cancel()
		if err != nil {
			chk.err = err
			return chk
		}
		if content != nil {
			found, chk.typ = content, st
			break
		}
	}
	if found == nil {
		return chk
	}
	raw, err := json.Marshal(found)
	if err != nil {
		chk.err = err
		return chk
	}
	slot, ok := matrixrtc.ParseSlot(matrixrtc.Event{Kind: matrixrtc.KindRTCSlot, StateKey: ref.sid, Content: raw})
	switch {
	case ok && slot.Encrypted:
		chk.err = errSlotEncrypted
	case ok && slot.Open:
		chk.action = slotActionNoop
	}
	return chk
}

// ---- publish ----

// publishSticky schedules the delayed leave (when supported) and sends the sticky join
// (matrixrtc.member.publish). A refused sticky send records the login as sticky-less for a while and
// returns errStickyUnsupported (after cancelling the delayed leave).
func (c *call) publishSticky(ctx context.Context, g core.GhostRTCIntent) error {
	sctx, span := c.child(ctx, "matrixrtc.member.publish", trace.SpanKindClient,
		obs.AttrCallID.String(c.id),
		obs.AttrMatrixRoomID.String(c.room.RoomID.String()),
		obs.AttrMatrixRTCFormat.String("sticky"),
		obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(c.plan.slot)),
		obs.AttrMatrixRTCMemberType.String(c.plan.typ.Type))
	defer c.endSpan(span)
	room := c.room.RoomID
	leave := matrixrtc.StickyLeaveContent(c.plan.slot, c.memberID)

	var did core.DelayedEventID
	if c.plan.delayed {
		octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
		var err error
		did, err = g.ScheduleDelayed(octx, room, c.plan.typ, leave, matrixrtc.DelayedLeaveDelay, matrixrtc.MemberStickyDuration)
		cancel()
		if err != nil {
			did = ""
			c.logFor(sctx).Warn().Str("error", scrubErr(err)).
				Msg("Failed to schedule the delayed leave, the sticky membership will rely on its expiry")
		}
	}
	span.SetAttributes(obs.AttrMatrixRTCDelayedLeave.Bool(did != ""))

	join := c.stickyJoinContent()
	octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
	evID, err := g.SendSticky(octx, room, c.plan.typ, join, matrixrtc.MemberStickyDuration)
	cancel()
	if err != nil {
		obs.Fail(span, err)
		if did != "" {
			cctx, ccancel := context.WithTimeout(sctx, matrixOpTimeout)
			_ = g.UpdateDelayed(cctx, did, "cancel")
			ccancel()
		}
		if errors.Is(err, core.ErrUnsupported) {
			c.m.noteStickyUnsupported(c.login)
			return errStickyUnsupported
		}
		return err
	}
	c.stickyEventID = evID
	c.stickyPublished = true
	span.SetAttributes(obs.AttrMatrixEventID.String(evID.String()))
	c.logFor(sctx).Debug().Stringer("event_id", evID).Str("member_id", c.memberID).Bool("delayed_leave", did != "").
		Msg("Published the sticky call membership")
	if did != "" {
		c.startKeeper(g, did, leave, join)
	}
	c.stickyTimer = c.cfg.Clock.NewTimer(matrixrtc.MemberStickyRefresh)
	return nil
}

// dropSticky downgrades a plan that also has the legacy format to legacy only (the sticky part failed
// before anything was published) and records why.
func (c *call) dropSticky(reason string) {
	c.logFor(c.setupCtx).Warn().Str("reason", reason).Msg("Sticky membership unavailable, publishing the legacy membership only")
	c.setupSpan.SetAttributes(obs.AttrMatrixRTCPlanDowngrade.String(reason))
	c.plan.sticky, c.plan.ringSticky, c.plan.mirror = false, false, false
	c.plan.auth, c.plan.url, c.plan.fallback = rtc.AuthSFUGet, "", false
	c.plan.slot = matrixrtc.SlotIDRoomLegacy
	c.plan.slots = nil
	c.memberID = ""
}

// onStickyRefresh re-sends the sticky join (same member id) before its 1 h duration ends
// (call.membership.refresh, linked root to the setup span).
func (c *call) onStickyRefresh() {
	g, ok := c.rtcGhost()
	if c.state >= stEnding || !c.stickyPublished || !ok {
		return
	}
	t := c.now()
	ctx, span := obs.StartLinkedRoot(context.Background(), "call.membership.refresh", c.setupCtx,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(t),
		trace.WithAttributes(obs.AttrCallID.String(c.id), obs.AttrMatrixRTCFormat.String("sticky")))
	_, err := c.sendStickyJoin(ctx, g, c.stickyJoinContent())
	if err != nil {
		obs.Fail(span, err)
		c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("Failed to refresh the ghost sticky call membership")
		c.endSpan(span)
		c.stickyTimer = c.cfg.Clock.NewTimer(refreshRetryDelay)
		return
	}
	c.endSpan(span)
	c.addPast("call.membership.refresh", t, obs.AttrMatrixRTCFormat.String("sticky"))
	c.logFor(ctx).Info().Msg("Refreshed the ghost sticky call membership")
	c.stickyTimer = c.cfg.Clock.NewTimer(matrixrtc.MemberStickyRefresh)
}

// ---- token fallback ----

// isTokenError reports whether err is an rtc join failure that a switch from MSC4195 to sfu_get can fix:
// the token stage failed, or the join timed out before the LiveKit connect started (a stalled get_token).
func isTokenError(err error) bool {
	var je *rtc.JoinError
	if !errors.As(err, &je) {
		return false
	}
	return je.Stage == "token_error" || (je.Stage == "timeout" && je.BeforeConnect)
}

// fallbackToSFUGet switches the call from MSC4195 get_token to sfu_get after a token error and
// re-publishes the memberships that named the hashed identity (same member id): the sticky join with a
// livekit_service_url transport and, in a dual plan, the legacy content without membershipID/url, so
// every client expects "<ghost>:<device>". Publishing problems are logged; the join still goes on.
func (c *call) fallbackToSFUGet(joinErr error) {
	ctx := c.setupCtx
	c.plan.auth, c.plan.url, c.plan.fallback = rtc.AuthSFUGet, "", false
	c.m.noteTokenFailed(c.login) // later calls of this login go straight to sfu_get for a while
	c.setupSpan.SetAttributes(obs.AttrRTCTokenFallback.Bool(true))
	c.logFor(ctx).Warn().Str("error", scrubErr(joinErr)).
		Msg("MSC4195 get_token failed, falling back to sfu_get and re-publishing the membership")

	if g, ok := c.rtcGhost(); ok && c.plan.sticky && c.stickyPublished {
		sctx, span := c.child(ctx, "matrixrtc.member.publish", trace.SpanKindClient,
			obs.AttrCallID.String(c.id),
			obs.AttrMatrixRTCFormat.String("sticky"),
			obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(c.plan.slot)),
			obs.AttrMatrixRTCMemberType.String(c.plan.typ.Type),
			obs.AttrMatrixRTCDelayedLeave.Bool(c.keeper != nil),
			obs.AttrRTCTokenFallback.Bool(true))
		if _, err := c.sendStickyJoin(sctx, g, c.stickyJoinContent()); err != nil {
			obs.Fail(span, err)
			c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to re-publish the sticky membership after the token fallback")
		}
		c.endSpan(span)
	}
	if c.plan.legacy && c.memberEventID != "" {
		sctx, span := c.child(ctx, "matrixrtc.membership.publish", trace.SpanKindClient,
			obs.AttrCallID.String(c.id),
			obs.AttrMatrixRoomID.String(c.room.RoomID.String()),
			obs.AttrMatrixRTCDeviceID.String(c.deviceID),
			obs.AttrMatrixRTCExpiresMS.Int64(c.memberExpires.Milliseconds()),
			obs.AttrRTCTokenFallback.Bool(true))
		octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
		_, err := c.room.Ghost.SendState(octx, c.room.RoomID, matrixrtc.CallMember, c.memberStateKey(), c.joinContent())
		cancel()
		if err != nil {
			obs.Fail(span, err)
			c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to re-publish the legacy membership after the token fallback")
		}
		c.endSpan(span)
	}
}

// ---- owner identities ----

// learnOwnerIdentities remembers the LiveKit identities of a live owner membership and, when the
// LiveKit session exists, hands the new ones to it (Session.AddOwnerIdentities is thread-safe and does
// no I/O).
func (c *call) learnOwnerIdentities(mem matrixrtc.Membership) {
	var fresh []string
loop:
	for _, idn := range mem.BackendIdentities() {
		for _, have := range c.ownerIDs {
			if have == idn {
				continue loop
			}
		}
		if len(c.ownerIDs) >= maxOwnerIdentities {
			break
		}
		c.ownerIDs = append(c.ownerIDs, idn)
		fresh = append(fresh, idn)
	}
	if len(fresh) > 0 && c.session != nil {
		c.session.AddOwnerIdentities(fresh...)
	}
}

// ---- delayed leave keeper ----

// delayKeeper restarts the delayed leave every DelayedLeaveRestart on its own goroutine until stopped.
// It starts no spans (outcomes are a metric only, like REGISTER refreshes).
type delayKeeper struct {
	cancel context.CancelFunc
	done   chan struct{}

	sendMu sync.Mutex // serialises sticky join sends (sendStickyJoin)

	mu    sync.Mutex
	id    core.DelayedEventID
	leave map[string]any
	join  map[string]any
}

func (k *delayKeeper) delayID() core.DelayedEventID {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.id
}

func (k *delayKeeper) setJoin(join map[string]any) {
	k.mu.Lock()
	k.join = join
	k.mu.Unlock()
}

// currentJoin returns the content the keeper re-sends after a gone delayed leave.
func (k *delayKeeper) currentJoin() map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.join
}

// sendStickyJoin sends content as the ghost's sticky join. While a keeper runs, every join send (the
// refresh, the token-fallback re-publish and the keeper's own re-send after a gone delayed leave) is
// serialised by keeper.sendMu, held across the send (bounded by matrixOpTimeout): the keeper re-reads
// its content under that lock just before sending, and this method stores the new content under it
// before sending, so the newest content always lands last.
func (c *call) sendStickyJoin(ctx context.Context, g core.GhostRTCIntent, join map[string]any) (id.EventID, error) {
	k := c.keeper
	if k != nil {
		k.sendMu.Lock()
		defer k.sendMu.Unlock()
		k.setJoin(join)
	}
	octx, cancel := context.WithTimeout(ctx, matrixOpTimeout)
	defer cancel()
	return g.SendSticky(octx, c.room.RoomID, c.plan.typ, join, matrixrtc.MemberStickyDuration)
}

// resendJoin is the keeper's join re-send: the current content is read under sendMu right before the
// send (see sendStickyJoin).
func (k *delayKeeper) resendJoin(ctx context.Context, g core.GhostRTCIntent, room id.RoomID, typ mevent.Type) error {
	k.sendMu.Lock()
	defer k.sendMu.Unlock()
	octx, cancel := context.WithTimeout(ctx, matrixOpTimeout)
	defer cancel()
	_, err := g.SendSticky(octx, room, typ, k.currentJoin(), matrixrtc.MemberStickyDuration)
	return err
}

func (c *call) startKeeper(g core.GhostRTCIntent, did core.DelayedEventID, leave, join map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	k := &delayKeeper{cancel: cancel, done: make(chan struct{}), id: did, leave: leave, join: join}
	c.keeper = k
	room, typ := c.room.RoomID, c.plan.typ
	log := obs.LoggerSC(trace.SpanContextFromContext(c.setupCtx), &c.log) // stored setup span context, no live span
	t := c.cfg.Clock.NewTicker(matrixrtc.DelayedLeaveRestart)
	go func() {
		defer close(k.done)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C():
				if !c.keeperTick(ctx, g, k, room, typ, log) {
					return
				}
			}
		}
	}()
}

// keeperTick restarts the delayed leave once. It returns false when the keeper has nothing left to
// restart (the delayed leave is gone and could not be rescheduled).
//
// A restart that finds the delayed event gone (it fired, or the server forgot it) means the leave may
// already be on the timeline, so the keeper reschedules the delayed leave and re-sends the join (same
// member id; harmless if the member is still joined).
func (c *call) keeperTick(ctx context.Context, g core.GhostRTCIntent, k *delayKeeper, room id.RoomID,
	typ mevent.Type, log *zerolog.Logger) bool {
	m := obs.M()
	octx, cancel := context.WithTimeout(ctx, matrixOpTimeout)
	err := g.UpdateDelayed(octx, k.delayID(), "restart")
	cancel()
	if ctx.Err() != nil {
		return true // stopping: not an outcome
	}
	switch {
	case err == nil:
		m.DelayedLeaveRestarts.Add(context.Background(), 1, optRestartOK)
		return true
	case !errors.Is(err, core.ErrNotFound):
		m.DelayedLeaveRestarts.Add(context.Background(), 1, optRestartError)
		return true
	}
	m.DelayedLeaveRestarts.Add(context.Background(), 1, optRestartGone)
	k.mu.Lock()
	leave := k.leave
	k.mu.Unlock()
	sctx, scancel := context.WithTimeout(ctx, matrixOpTimeout)
	did, serr := g.ScheduleDelayed(sctx, room, typ, leave, matrixrtc.DelayedLeaveDelay, matrixrtc.MemberStickyDuration)
	scancel()
	if serr != nil {
		if ctx.Err() == nil {
			log.Warn().Str("error", scrubErr(serr)).Msg("The delayed leave was gone and could not be rescheduled")
		}
		k.mu.Lock()
		k.id = ""
		k.mu.Unlock()
		return false
	}
	k.mu.Lock()
	k.id = did
	k.mu.Unlock()
	jerr := k.resendJoin(ctx, g, room, typ)
	if jerr != nil && ctx.Err() == nil {
		log.Warn().Str("error", scrubErr(jerr)).Msg("The delayed leave was gone: rescheduled it but failed to re-send the join")
		return true
	}
	log.Warn().Msg("The delayed leave was gone: rescheduled it and re-sent the join")
	return true
}

// cancelKeeper asks the keeper to stop without waiting.
func (c *call) cancelKeeper() {
	if c.keeper != nil {
		c.keeper.cancel()
	}
}

// stopKeeper stops the keeper, waits for it (bounded) and returns its final delayed event id ("" when
// none or lost). Idempotent.
func (c *call) stopKeeper() core.DelayedEventID {
	k := c.keeper
	if k == nil {
		return ""
	}
	k.cancel()
	t := c.cfg.Clock.NewTimer(teardownStepTimeout)
	select {
	case <-k.done:
	case <-t.C():
		c.logFor(c.setupCtx).Warn().Msg("Timed out waiting for the delayed leave keeper to stop")
	}
	t.Stop()
	return k.delayID()
}

// ---- leave ----

// leaveMembership ends the ghost's membership: stop the keeper, send the sticky leave, then clear the
// legacy membership (each best effort). A no-op for calls that never published.
func (c *call) leaveMembership(ctx context.Context) {
	if c.room == nil {
		return
	}
	if c.stickyPublished {
		c.leaveSticky(ctx)
	} else {
		c.stopKeeper()
	}
	if c.memberEventID != "" {
		c.clearMembership(ctx)
	}
}

// leaveSticky sends the sticky leave: fires the delayed leave when there is one (delayed_send), else
// (or when that fails) sends the leave explicitly (matrixrtc.member.leave span, attribute leave_via).
func (c *call) leaveSticky(ctx context.Context) {
	did := c.stopKeeper()
	c.stickyPublished = false
	g, ok := c.rtcGhost()
	if !ok {
		return // nothing was published through a GhostRTCIntent: no span for a leave that cannot happen
	}
	sctx, span := c.child(ctx, "matrixrtc.member.leave", trace.SpanKindClient,
		obs.AttrCallID.String(c.id),
		obs.AttrMatrixRTCFormat.String("sticky"),
		obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(c.plan.slot)),
		obs.AttrMatrixRTCMemberType.String(c.plan.typ.Type))
	defer c.endSpan(span)
	via := leaveViaExplicit
	if did != "" {
		octx, cancel := context.WithTimeout(sctx, teardownStepTimeout)
		err := g.UpdateDelayed(octx, did, "send")
		cancel()
		if err == nil {
			via = leaveViaDelayed
		} else {
			c.logFor(sctx).Debug().Str("error", scrubErr(err)).Msg("Delayed leave send failed, leaving explicitly")
		}
	}
	if via == leaveViaExplicit {
		octx, cancel := context.WithTimeout(sctx, teardownStepTimeout)
		_, err := g.SendSticky(octx, c.room.RoomID, c.plan.typ, matrixrtc.StickyLeaveContent(c.plan.slot, c.memberID),
			matrixrtc.MemberStickyDuration)
		cancel()
		if err != nil {
			obs.Fail(span, err)
			c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to send the sticky call membership leave")
		}
	}
	span.SetAttributes(obs.AttrMatrixRTCLeaveVia.String(via))
}

// withdrawRing withdraws a sticky ring (matrixrtc.ring.withdraw), once, best effort.
func (c *call) withdrawRing(ctx context.Context) {
	g, ok := c.rtcGhost()
	if !c.ringSticky || c.ringWithdrawn || c.ringEventID == "" || !ok {
		return
	}
	c.ringWithdrawn = true
	sctx, span := c.child(ctx, "matrixrtc.ring.withdraw", trace.SpanKindClient,
		obs.AttrCallID.String(c.id), obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(c.ringSlot())))
	octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
	_, err := g.SendSticky(octx, c.room.RoomID, matrixrtc.RTCNotification, matrixrtc.RingWithdrawContent(c.ringSlot()),
		matrixrtc.NotificationStickyDuration(c.cfg.RingTimeout))
	cancel()
	if err != nil {
		obs.Fail(span, err)
		c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to withdraw the ring notification")
	}
	c.endSpan(span)
}
