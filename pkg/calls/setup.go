package calls

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

var errNoServiceURL = errors.New("no LiveKit service URL configured or discovered")

// setup runs the inline setup step (PHASE2-SPEC §8.4). It returns false if the call already ended
// (the failure branch ran).
func (c *call) setup() bool {
	c.startedAt = c.info.ReceivedAt
	if c.startedAt.IsZero() {
		c.startedAt = c.now()
	}

	// 1. call.inbound (root, backdated to INVITE receipt, D10) and the call counters.
	attrs := []attribute.KeyValue{
		obs.AttrCallID.String(c.id),
		c.dirAttr,
		obs.AttrCallTrigger.String(triggerInvite),
		obs.AttrSIPCallID.String(c.info.CallID),
		obs.AttrCallLocalE164.String(c.localE164),
	}
	if host, port, err := net.SplitHostPort(c.info.RemoteAddr); err == nil {
		attrs = append(attrs, obs.AttrNetworkPeerAddress.String(host))
		if p, err := strconv.Atoi(port); err == nil {
			attrs = append(attrs, obs.AttrNetworkPeerPort.Int(p))
		}
	}
	c.setupCtx, c.setupSpan = obs.Start(context.Background(), "call.inbound",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(c.startedAt),
		trace.WithAttributes(attrs...))
	if !c.info.TryingSentAt.IsZero() {
		c.spanEvent(c.setupSpan, "sip.response.sent", c.info.TryingSentAt, obs.AttrSIPStatusCode.Int(100))
	}
	c.recordStarted()
	ctx := c.setupCtx

	// 2. Caller number: P-Asserted-Identity wins over From.
	raw := c.info.AssertedUser
	if raw == "" {
		raw = c.info.FromUser
	}
	e164, err := phone.Normalize(raw, c.cfg.DefaultCountryCode)
	if err != nil {
		c.setupSpan.SetAttributes(obs.AttrCallRemoteWith.Bool(true))
		c.logFor(ctx).Info().Bool("withheld", true).Str("to", c.info.ToUser).
			Msg("Rejected inbound call: caller number withheld or not a phone number")
		c.reject(480, "Temporarily Unavailable")
		c.fail(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: ""})
		return false
	}
	c.e164 = e164
	c.publish()
	c.setupSpan.SetAttributes(obs.AttrCallRemoteE164.String(e164))

	// 3. Busy: one active call per login (D19).
	if c.busy {
		c.logFor(ctx).Info().Str("from", e164).Msg("Rejected inbound call: line busy")
		c.reject(486, "Busy Here")
		c.resolveRoomBestEffort(ctx)
		c.fail(failSpec{outcome: outcomeBusy, reason: reasonRejected, notice: noticeBusy})
		return false
	}

	// 4. Offer must contain PCMU.
	if !c.info.Offer.HasSDP || !c.info.Offer.PCMU {
		c.logFor(ctx).Info().Str("from", e164).Bool("has_sdp", c.info.Offer.HasSDP).
			Strs("offered_codecs", c.info.Offer.Codecs).Msg("Rejected inbound call: no PCMU offered")
		c.reject(488, "Not Acceptable Here")
		c.resolveRoomBestEffort(ctx)
		c.fail(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeCodec})
		return false
	}

	// 5. Owner and room.
	c.owner = c.cfg.Host.OwnerMXID(c.login)
	if c.owner == "" {
		c.logFor(ctx).Error().Msg("Rejected inbound call: login has no owner")
		c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: ""})
		return false
	}
	c.setupSpan.SetAttributes(obs.AttrMatrixOwnerID.String(c.owner.String()))
	room, err := c.resolveRoom(ctx)
	if err != nil {
		c.logFor(ctx).Error().Str("from", e164).Str("error", scrubErr(err)).Msg("Failed to resolve the call room")
		c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: ""})
		return false
	}
	c.room = room
	c.roomID = room.RoomID
	c.publish()
	c.m.setRoutable(c)
	c.setupSpan.SetAttributes(
		obs.AttrMatrixRoomID.String(room.RoomID.String()),
		obs.AttrMatrixGhostID.String(room.Ghost.UserID().String()))
	c.logFor(ctx).Info().Str("from", e164).Str("to", c.info.ToUser).Stringer("room_id", room.RoomID).
		Msg("Inbound call")

	// 6. Encrypted-room guard (D18).
	if room.Encrypted {
		c.logFor(ctx).Error().Stringer("room_id", room.RoomID).Msg("Refusing to ring in an encrypted room")
		c.reject(480, "Temporarily Unavailable")
		c.fail(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeEncrypted})
		return false
	}

	// 7. LiveKit service URL (D13).
	if err := c.resolveServiceURL(ctx); err != nil {
		c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("No LiveKit service URL, sending the call to voicemail")
		c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeNoAudio})
		return false
	}

	// The slow steps are behind us: if the caller already hung up, do not ring.
	if c.pollSIPEnded() {
		if c.sipCause == sipua.EndCancelled {
			c.spanEvent(c.setupSpan, "sip.cancel.received", c.now())
			c.fail(failSpec{outcome: outcomeCancelled, reason: reasonCancel, notice: noticeMissed})
		} else {
			c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeMissed})
		}
		return false
	}

	// 8. 180 Ringing.
	if c.stopRequested() {
		return c.failOnStop()
	}
	c.in.Ringing()
	c.spanEvent(c.setupSpan, "sip.response.sent", c.now(), obs.AttrSIPStatusCode.Int(180))

	// 9. Ghost membership.
	if c.stopRequested() {
		return c.failOnStop()
	}
	if err := c.publishMembership(ctx); err != nil {
		if errors.Is(err, errSlotEncrypted) {
			c.reject(480, "Temporarily Unavailable")
			c.fail(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeEncrypted})
			return false
		}
		c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("Failed to publish the ghost call membership")
		c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeNoAudio})
		return false
	}

	// 10. Ring.
	if c.stopRequested() {
		return c.failOnStop()
	}
	if err := c.sendRing(ctx); err != nil {
		c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("Failed to send the ring notification")
		c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeNoAudio})
		return false
	}

	// 11. call.ring, timers, LiveKit pre-join.
	if c.stopRequested() {
		return c.failOnStop()
	}
	_, c.ringSpan = c.child(ctx, "call.ring", trace.SpanKindInternal,
		obs.AttrCallID.String(c.id), obs.AttrMatrixEventID.String(c.ringEventID.String()))
	c.ringOpen = true
	c.ringTimer = c.cfg.Clock.NewTimer(c.cfg.RingTimeout)
	if c.cfg.RingTimeout > ringKeepalive {
		c.keepalive = c.cfg.Clock.NewTicker(ringKeepalive)
	}
	c.setState(stRinging)
	c.logFor(ctx).Info().Stringer("ring_event_id", c.ringEventID).
		Int64("lifetime_ms", c.cfg.RingTimeout.Milliseconds()).Msg("Ringing owner")
	c.startJoin()
	return true
}

// stopRequested reports (without blocking) whether shutdown was requested during inline setup.
func (c *call) stopRequested() bool {
	select {
	case <-c.quit:
		return true
	default:
		return false
	}
}

// failOnStop is the setup failure branch for a shutdown request; it returns false (setup ended).
func (c *call) failOnStop() bool {
	c.logFor(c.setupCtx).Info().Msg("Bridge stopping during call setup")
	c.reject(480, "Temporarily Unavailable")
	c.fail(failSpec{outcome: outcomeFailed, reason: reasonBridgeStop, notice: noticeMissed})
	return false
}

// pollSIPEnded drains already-queued SIP events without blocking and reports whether the call ended.
func (c *call) pollSIPEnded() bool {
	for !c.sipEnded && c.sipEvents != nil {
		select {
		case ev, ok := <-c.sipEvents:
			if !ok {
				c.sipEvents = nil
				return c.sipEnded
			}
			if ev.Kind == sipua.EventEnded {
				c.recordSIPEnd(ev)
			}
		default:
			return false
		}
	}
	return c.sipEnded
}

// resolveRoom runs Host.ResolveCallRoom inside call.resolve_room.
func (c *call) resolveRoom(ctx context.Context) (*core.CallRoom, error) {
	sctx, span := c.child(ctx, "call.resolve_room", trace.SpanKindInternal,
		obs.AttrCallID.String(c.id), obs.AttrCallRemoteE164.String(c.e164))
	rctx, cancel := context.WithTimeout(sctx, resolveRoomTimeout)
	room, err := c.cfg.Host.ResolveCallRoom(rctx, c.login, c.e164)
	cancel()
	if err == nil && (room == nil || room.Ghost == nil) {
		err = errors.New("host returned no room or ghost")
	}
	if err != nil {
		obs.Fail(span, err)
		c.endSpan(span)
		return nil, err
	}
	span.SetAttributes(obs.AttrMatrixRoomID.String(room.RoomID.String()))
	c.endSpan(span)
	return room, nil
}

// resolveRoomBestEffort resolves the room for a notice on a refused call (never routable).
func (c *call) resolveRoomBestEffort(ctx context.Context) {
	room, err := c.resolveRoom(ctx)
	if err != nil {
		c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("Failed to resolve the call room for a notice")
		return
	}
	c.room = room
	c.setupSpan.SetAttributes(obs.AttrMatrixRoomID.String(room.RoomID.String()))
}

func (c *call) resolveServiceURL(ctx context.Context) error {
	if c.cfg.ServiceURL == nil {
		return errNoServiceURL
	}
	sctx, cancel := context.WithTimeout(ctx, matrixOpTimeout)
	u, err := c.cfg.ServiceURL(sctx)
	cancel()
	if err != nil {
		return err
	}
	if u == "" {
		return errNoServiceURL
	}
	c.serviceURL = u
	return nil
}

func (c *call) memberStateKey() string {
	return matrixrtc.MemberStateKey(c.room.Ghost.UserID(), c.deviceID)
}

func (c *call) joinContent() map[string]any {
	p := matrixrtc.JoinParams{
		UserID:     c.room.Ghost.UserID(),
		DeviceID:   c.deviceID,
		RoomID:     c.room.RoomID,
		ServiceURL: c.serviceURL,
		CreatedTS:  c.memberCreated,
		Expires:    c.memberExpires,
	}
	if c.plan.sticky && c.plan.auth == rtc.AuthMSC4195 && c.plan.url != "" {
		// Dual with MSC4195: both memberships name the same hashed identity (membershipID = member.id).
		p.MemberID, p.LiveKitURL = c.memberID, c.plan.url
	}
	return matrixrtc.JoinContent(p)
}

// publishMembership publishes the ghost's membership as planned (format.go): slots, the sticky
// rtc.member (matrixrtc.member.publish) and the legacy call.member (matrixrtc.membership.publish). A
// failing sticky part degrades to legacy when the plan has it. errSlotEncrypted means the room's slot
// declares encryption (the caller refuses the call like an encrypted room); under auto, a
// bridge-initiated call drops the sticky part instead and rings on the legacy path.
func (c *call) publishMembership(ctx context.Context) error {
	if c.ownerMem.Key != "" {
		c.learnOwnerIdentities(c.ownerMem)
	}
	c.choosePlan(ctx)
	if c.plan.sticky {
		c.memberID = matrixrtc.NewMemberID()
		g, _ := c.rtcGhost() // sticky plans only exist for ghosts with the capability
		err := c.ensureSlots(ctx, g)
		switch {
		case errors.Is(err, errSlotEncrypted) && (c.plan.mirror || !c.plan.legacy || c.cfg.MembershipFormat != fmtAuto):
			// The owner's client (or an explicit sticky/dual config) wants this slot: an encrypted slot
			// would give silent audio, so refuse like an encrypted room.
			return err
		case errors.Is(err, errSlotEncrypted):
			// Bridge-initiated under auto: the sticky part is optional; ring on the legacy path as before.
			c.dropSticky(downgradeSlot)
		case err != nil && c.plan.mirror:
			// Owner-initiated: the owner's client normally opened the slot, so a read or open problem
			// is not worth failing the call. Already logged and recorded by ensureSlots.
			c.setupSpan.SetAttributes(obs.AttrMatrixRTCPlanDowngrade.String(downgradeSlot))
		case err != nil:
			if !c.plan.legacy {
				return err
			}
			c.dropSticky(downgradeSlot)
		}
		if c.plan.sticky {
			if err := c.publishSticky(ctx, g); err != nil {
				if !c.plan.legacy && !c.plan.mirror {
					return err // explicit sticky-only config: nothing to fall back to
				}
				reason := downgradeError
				if errors.Is(err, errStickyUnsupported) {
					reason = downgradeUnsupported
				}
				c.dropSticky(reason)
				c.plan.legacy = true // a mirror plan had no legacy part: publish it now
			}
		}
	}
	if c.plan.legacy {
		return c.publishLegacy(ctx)
	}
	return nil
}

// publishLegacy sends the ghost's call.member (matrixrtc.membership.publish).
func (c *call) publishLegacy(ctx context.Context) error {
	c.memberCreated = c.now()
	c.memberExpires = matrixrtc.DefaultMembershipExpiry
	sctx, span := c.child(ctx, "matrixrtc.membership.publish", trace.SpanKindClient,
		obs.AttrCallID.String(c.id),
		obs.AttrMatrixRoomID.String(c.room.RoomID.String()),
		obs.AttrMatrixRTCDeviceID.String(c.deviceID),
		obs.AttrMatrixRTCExpiresMS.Int64(c.memberExpires.Milliseconds()))
	octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
	evID, err := c.room.Ghost.SendState(octx, c.room.RoomID, matrixrtc.CallMember, c.memberStateKey(), c.joinContent())
	cancel()
	if err != nil {
		obs.Fail(span, err)
		c.endSpan(span)
		return err
	}
	c.memberEventID = evID
	span.SetAttributes(obs.AttrMatrixEventID.String(evID.String()))
	c.endSpan(span)
	return nil
}

// sendRing sends the MSC4075 ring (matrixrtc.ring.send). With a sticky plan the ring is the union shape
// (slot_id, sticky key, no m.relates_to without a legacy membership) and sent sticky.
func (c *call) sendRing(ctx context.Context) error {
	sctx, span := c.child(ctx, "matrixrtc.ring.send", trace.SpanKindClient,
		obs.AttrCallID.String(c.id),
		obs.AttrMatrixRoomID.String(c.room.RoomID.String()),
		obs.AttrRingLifetimeMS.Int64(c.cfg.RingTimeout.Milliseconds()))
	g, hasRTC := c.rtcGhost()
	sticky := c.plan.sticky && c.plan.ringSticky && hasRTC
	build := func(sticky bool) map[string]any {
		return matrixrtc.RingContent(matrixrtc.RingParams{
			Owner:             c.owner,
			MembershipEventID: c.memberEventID,
			Lifetime:          c.cfg.RingTimeout,
			SenderTS:          c.now(),
			SlotID:            c.ringSlot(),
			Sticky:            sticky,
		})
	}
	if sticky {
		span.SetAttributes(obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(c.ringSlot())))
	}
	octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
	var evID id.EventID
	var err error
	if sticky {
		evID, err = g.SendSticky(octx, c.room.RoomID, matrixrtc.RTCNotification, build(true),
			matrixrtc.NotificationStickyDuration(c.cfg.RingTimeout))
		if errors.Is(err, core.ErrUnsupported) {
			c.m.noteStickyUnsupported(c.login)
			c.logFor(sctx).Warn().Msg("Sticky ring refused by the homeserver, sending a plain ring")
			sticky = false
			evID, err = c.room.Ghost.SendMessage(octx, c.room.RoomID, matrixrtc.RTCNotification, build(false))
		}
	} else {
		evID, err = c.room.Ghost.SendMessage(octx, c.room.RoomID, matrixrtc.RTCNotification, build(false))
	}
	cancel()
	if err != nil {
		obs.Fail(span, err)
		c.endSpan(span)
		return err
	}
	c.ringSticky = sticky
	c.ringEventID = evID
	c.ringSentAt = c.now()
	span.SetAttributes(obs.AttrMatrixEventID.String(evID.String()))
	c.endSpan(span)
	return nil
}

// endRing ends call.ring once, linking the ingest span that ended it (if any).
func (c *call) endRing(from trace.SpanContext, outcome string) {
	if !c.ringOpen {
		return
	}
	c.ringOpen = false
	if from.IsValid() {
		c.ringSpan.AddLink(trace.Link{SpanContext: from})
	}
	c.ringSpan.SetAttributes(obs.AttrCallOutcome.String(outcome))
	c.ringEndedAt = c.now()
	c.ringSpan.End(trace.WithTimestamp(c.ringEndedAt))
}

// startJoin starts the next LiveKit join attempt in its own goroutine. The goroutine gets only the
// carried setup span context (rtc.join is a child; the actor waits for it before ending call.inbound).
func (c *call) startJoin() {
	c.joinAttempt++
	attempt := c.joinAttempt
	ctx, cancel := context.WithCancel(obs.CarrySpan(context.Background(), c.setupCtx))
	c.joinCancel = cancel
	c.joinInFlight = true
	if c.joinTimer != nil {
		c.joinTimer.Stop()
	}
	c.joinTimer = c.cfg.Clock.NewTimer(joinTimeout)
	p := rtc.JoinParams{
		Ghost:      c.room.Ghost,
		RoomID:     c.room.RoomID,
		DeviceID:   c.deviceID,
		ServiceURL: c.serviceURL,
		Owner:      c.owner,
		CallID:     c.id,
		Attempt:    attempt,
		Owners:     append([]string(nil), c.ownerIDs...),
	}
	if c.plan.auth == rtc.AuthMSC4195 {
		p.Auth, p.SlotID, p.MemberID, p.LiveKitURL = rtc.AuthMSC4195, c.plan.slot, c.memberID, c.plan.url
	}
	joiner, results := c.cfg.Joiner, c.joinResults
	go func() {
		defer cancel()
		s, err := joiner.Join(ctx, p)
		if err == nil && s == nil {
			err = errors.New("rtc join returned no session")
		}
		if s != nil && (err != nil || ctx.Err() != nil) {
			// Cancelled (timeout or teardown) while joining: nobody wants this session.
			leaveDetached(s)
			s = nil
			if err == nil {
				err = ctx.Err()
			}
		}
		// Capacity maxJoinAttempts+2: at most maxJoinAttempts attempts plus one free MSC4195 fallback
		// attempt, so this never blocks.
		results <- event{session: s, err: err, attempt: attempt}
	}()
}

// leaveDetached leaves a session nobody owns (join goroutine only).
func leaveDetached(s rtc.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), teardownStepTimeout)
	_ = s.Leave(ctx)
	cancel()
}

// onJoinResult handles a LiveKit join result in Ringing/Answering.
func (c *call) onJoinResult(ev event) {
	if !c.joinInFlight || ev.attempt != c.joinAttempt {
		if ev.session != nil {
			c.leaveSession(c.setupCtx, ev.session)
		}
		return
	}
	c.joinInFlight = false
	if c.joinTimer != nil {
		c.joinTimer.Stop()
		c.joinTimer = nil
	}
	if c.state != stRinging && c.state != stAnswering && c.state != stJoining {
		if ev.session != nil {
			c.leaveSession(c.setupCtx, ev.session)
		}
		return
	}
	if ev.err == nil {
		c.session = ev.session
		c.sessEvents = ev.session.Events()
		if len(c.ownerIDs) > 0 {
			c.session.AddOwnerIdentities(c.ownerIDs...) // learned while the join was in flight
		}
		c.logFor(c.setupCtx).Info().Int("attempt", ev.attempt).Str("livekit_room", ev.session.LiveKitRoom()).
			Str("identity", ev.session.Identity()).Msg("Joined LiveKit")
		switch c.state {
		case stAnswering:
			c.doAnswer()
		case stJoining:
			c.dial()
		}
		return
	}
	c.logFor(c.setupCtx).Warn().Int("attempt", ev.attempt).Str("error", scrubErr(ev.err)).Msg("LiveKit join failed")
	if c.plan.auth == rtc.AuthMSC4195 && c.plan.fallback && isTokenError(ev.err) {
		// The switch to sfu_get is not a retry: it does not consume a join attempt (and happens once,
		// fallbackToSFUGet clears plan.fallback).
		c.joinFallbacks++
		c.fallbackToSFUGet(ev.err)
		c.startJoin()
		return
	}
	if c.canRetryJoin() {
		c.startJoin()
		return
	}
	if c.isOutbound() {
		c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonError, notice: failedNotice(failReasonNoAudio), decline: true})
		return
	}
	c.endRing(trace.SpanContext{}, outcomeFailed)
	c.reject(480, "Temporarily Unavailable")
	c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeNoAudio})
}

// cancelJoinAndWait cancels an in-flight join and waits for its result (PHASE2-SPEC §8.2 rule 7),
// leaving a late session immediately.
func (c *call) cancelJoinAndWait() {
	if !c.joinInFlight {
		return
	}
	c.joinCancel()
	if c.joinTimer != nil {
		c.joinTimer.Stop()
		c.joinTimer = nil
	}
	t := c.cfg.Clock.NewTimer(joinTimeout + time.Second)
	defer t.Stop()
	for c.joinInFlight {
		select {
		case ev := <-c.joinResults:
			if ev.session != nil {
				c.leaveSession(c.setupCtx, ev.session)
			}
			if ev.attempt == c.joinAttempt {
				c.joinInFlight = false
			}
		case <-t.C():
			c.logFor(c.setupCtx).Warn().Msg("Timed out waiting for the cancelled LiveKit join")
			c.joinInFlight = false
		}
	}
}

// leaveSession leaves s inside rtc.leave (child of parent).
func (c *call) leaveSession(parent context.Context, s rtc.Session) {
	sctx, span := c.child(parent, "rtc.leave", trace.SpanKindClient, obs.AttrCallID.String(c.id))
	lctx, cancel := context.WithTimeout(sctx, teardownStepTimeout)
	err := s.Leave(lctx)
	cancel()
	if err != nil {
		obs.Fail(span, err)
		c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to leave LiveKit")
	}
	c.endSpan(span)
}

// dropSession leaves and forgets the current session (it disconnected before answer).
func (c *call) dropSession() {
	if c.session == nil {
		return
	}
	c.leaveSession(c.setupCtx, c.session)
	c.session = nil
	c.sessEvents = nil
	clear(c.lkOwners)
}

// doAnswer issues the SIP answer once (the LiveKit session must be ready).
func (c *call) doAnswer() {
	if c.answerIssued || c.session == nil {
		return
	}
	c.answerIssued = true
	c.answerCtx, c.answerSpan = c.child(c.setupCtx, "call.answer", trace.SpanKindInternal,
		obs.AttrCallID.String(c.id),
		obs.AttrMediaCodec.String("PCMU"),
		obs.AttrMediaPtimeMS.Int(20),
		obs.AttrMediaTelephoneEvent.Bool(c.info.Offer.TelephoneEvent))
	c.in.Answer()
}

// activate runs on EventAnswered: start media, end call.answer and call.inbound (at ACK).
func (c *call) activate(ev sipua.CallEvent) {
	c.answer = ev.Answer
	c.answered = true
	if c.answerTimer != nil {
		c.answerTimer.Stop()
		c.answerTimer = nil
	}
	ack := ev.Answer.AckAt
	if ack.IsZero() {
		ack = c.now()
	}
	ok := ev.Answer.OKSentAt
	if ok.IsZero() {
		ok = ack
	}
	c.spanEvent(c.answerSpan, "sip.response.sent", ok, obs.AttrSIPStatusCode.Int(200))
	c.spanEvent(c.answerSpan, "sip.ack.received", ack)
	c.answeredAt = ack
	c.publish()

	var mattrs []attribute.KeyValue
	if host, _, err := net.SplitHostPort(ev.Answer.RemoteRTP); err == nil {
		mattrs = append(mattrs, obs.AttrNetworkPeerAddress.String(host))
	}
	mattrs = append(mattrs, obs.AttrCallID.String(c.id))
	_, mspan := c.child(c.answerCtx, "media.start", trace.SpanKindInternal, mattrs...)
	br, err := c.cfg.StartMedia(media.Config{Clock: c.cfg.Clock, Log: c.log, RTPTimeout: c.cfg.RTPTimeout},
		ev.Media, c.session.Audio())
	if err != nil {
		obs.Fail(mspan, err)
	}
	c.endSpan(mspan)
	c.endSpan(c.answerSpan)
	c.setupSpan.SetAttributes(obs.AttrCallOutcome.String(outcomeAnswered), obs.AttrSIPStatusCode.Int(200))
	c.setupSpan.End(trace.WithTimestamp(ack))
	c.setupEnded = true
	c.setState(stActive)
	c.recordSetupDuration(ok.Sub(c.startedAt), outcomeAnswered)
	c.setupRecorded = true

	if err != nil {
		c.logFor(c.setupCtx).Error().Str("error", scrubErr(err)).Msg("Failed to start the media bridge")
		c.teardown(byBridge, reasonError, false)
		return
	}
	c.bridge = br
	c.bridgeDone = br.Done()
	c.statsTicker = c.cfg.Clock.NewTicker(statsInterval)
	c.scheduleRefresh()
	if len(c.lkOwners) == 0 {
		c.graceTimer = c.cfg.Clock.NewTimer(ownerRTCGrace)
	}
	c.logFor(c.setupCtx).Info().Str("codec", ev.Answer.Codec).Str("remote_rtp", ev.Answer.RemoteRTP).
		Msg("Call connected")
}

// scheduleRefresh arms the ghost membership refresh at created_ts + expires - lead (D15).
func (c *call) scheduleRefresh() {
	if c.memberEventID == "" {
		return // no legacy membership (sticky-only plan): the sticky refresh timer covers it
	}
	due := c.memberCreated.Add(c.memberExpires - membershipRefreshLead)
	d := due.Sub(c.now())
	if d < 0 {
		d = 0
	}
	c.refreshTimer = c.cfg.Clock.NewTimer(d)
}

// onRefresh re-sends the ghost membership with the same created_ts and expires extended by
// DefaultMembershipExpiry (call.membership.refresh, linked root to the setup span).
func (c *call) onRefresh() {
	if c.state != stActive {
		return
	}
	newExpires := c.memberExpires + matrixrtc.DefaultMembershipExpiry
	t := c.now()
	ctx, span := obs.StartLinkedRoot(context.Background(), "call.membership.refresh", c.setupCtx,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(t),
		trace.WithAttributes(obs.AttrCallID.String(c.id), obs.AttrMatrixRTCExpiresMS.Int64(newExpires.Milliseconds())))
	old := c.memberExpires
	c.memberExpires = newExpires
	octx, cancel := context.WithTimeout(ctx, matrixOpTimeout)
	_, err := c.room.Ghost.SendState(octx, c.room.RoomID, matrixrtc.CallMember, c.memberStateKey(), c.joinContent())
	cancel()
	if err != nil {
		c.memberExpires = old
		obs.Fail(span, err)
		c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("Failed to refresh the ghost call membership")
		c.endSpan(span)
		c.refreshTimer = c.cfg.Clock.NewTimer(refreshRetryDelay)
		return
	}
	c.endSpan(span)
	c.addPast("call.membership.refresh", t, obs.AttrMatrixRTCExpiresMS.Int64(newExpires.Milliseconds()))
	c.logFor(ctx).Info().Int64("expires_ms", newExpires.Milliseconds()).Msg("Refreshed the ghost call membership")
	c.scheduleRefresh()
}
