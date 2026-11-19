package calls

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

var errNoSIPMedia = errors.New("answered call has no SIP media leg")

// newOutCall returns an outbound call actor (not yet registered or running). sc is the span context of
// the trigger span (cmd.call or matrixrtc.ingest); only the context is kept, never a live span.
func newOutCall(m *Manager, login core.LoginRef, trigger string, sc trace.SpanContext, now time.Time) *call {
	cid := newCallID(now)
	c := newActor(m, login, cid, dirOutbound,
		m.cfg.Log.With().Str("component", "calls").Str("call_id", cid).Logger())
	c.trigger = trigger
	c.triggerAt = now
	c.triggerCtx = context.Background()
	if sc.IsValid() {
		c.triggerCtx = trace.ContextWithSpanContext(context.Background(), sc)
	}
	return c
}

// setupOutbound runs the inline outbound setup (PHASE3-SPEC §6.4). It returns false if the call already
// ended (the failure branch ran).
func (c *call) setupOutbound() bool {
	c.startedAt = c.triggerAt
	if c.startedAt.IsZero() {
		c.startedAt = c.now()
	}
	if c.cfg.LoginNumber != nil {
		c.localE164 = c.cfg.LoginNumber(c.login)
	}

	// 1. call.outbound (linked root to the trigger span) and the call counters.
	attrs := []attribute.KeyValue{
		obs.AttrCallID.String(c.id),
		c.dirAttr,
		obs.AttrCallTrigger.String(c.trigger),
	}
	if c.localE164 != "" {
		attrs = append(attrs, obs.AttrCallLocalE164.String(c.localE164))
	}
	if c.cmdNumber != "" {
		attrs = append(attrs, obs.AttrCallRemoteE164.String(c.cmdNumber))
	}
	c.setupCtx, c.setupSpan = obs.StartLinkedRoot(context.Background(), "call.outbound", c.triggerCtx,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(c.startedAt),
		trace.WithAttributes(attrs...))
	c.recordStarted()
	ctx := c.setupCtx

	// 2. Owner.
	c.owner = c.cfg.Host.OwnerMXID(c.login)
	if c.owner == "" {
		c.logFor(ctx).Error().Msg("Outgoing call not placed: login has no owner")
		c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonError})
		return false
	}
	c.setupSpan.SetAttributes(obs.AttrMatrixOwnerID.String(c.owner.String()))

	// 3. Room.
	if c.trigger == triggerCommand && !c.cmdRoom {
		c.e164 = c.cmdNumber
		c.publish()
		room, err := c.resolveRoom(ctx)
		if err != nil {
			c.logFor(ctx).Error().Str("to", c.e164).Str("error", scrubErr(err)).Msg("Failed to resolve the call room")
			c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonError})
			return false
		}
		c.room = room
		c.roomID = room.RoomID
		c.publish()
		c.m.setRoutable(c)
	} else {
		room, err := c.lookupRoom(ctx)
		switch {
		case errors.Is(err, core.ErrNotCallRoom):
			c.logFor(ctx).Info().Stringer("room_id", c.roomID).Msg("Outgoing call not placed: not a call room")
			c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected})
			return false
		case err != nil:
			c.logFor(ctx).Error().Stringer("room_id", c.roomID).Str("error", scrubErr(err)).Msg("Failed to look up the call room")
			c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonError})
			return false
		}
		c.room = room
		c.e164 = room.RemoteE164
		c.publish()
	}
	c.setupSpan.SetAttributes(
		obs.AttrCallRemoteE164.String(c.e164),
		obs.AttrMatrixRoomID.String(c.room.RoomID.String()),
		obs.AttrMatrixGhostID.String(c.room.Ghost.UserID().String()))

	// Busy line (owner trigger while another call is active, D17).
	if c.busy {
		c.logFor(ctx).Info().Str("to", c.e164).Stringer("room_id", c.room.RoomID).
			Msg("Outgoing call not placed: another call is in progress")
		c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeOutBusyLine, decline: true})
		return false
	}

	// 4. Destination policy and the encrypted-room guard.
	if !phone.IsE164(c.e164) {
		c.logFor(ctx).Error().Str("to", c.e164).Msg("Outgoing call not placed: the room's number is not E.164")
		c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonError, notice: failedNotice(failReasonNotPlaced), decline: true})
		return false
	}
	if !validNANP(c.e164) {
		c.logFor(ctx).Info().Str("to", c.e164).Msg("Outgoing call not placed: not a valid NANP number")
		c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeOutNotAllowed, decline: true})
		return false
	}
	if !c.m.prefixAllowed(c.e164) {
		c.logFor(ctx).Info().Str("to", c.e164).Msg("Outgoing call not placed: destination not allowed by dial_allowed_prefixes")
		c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeOutNotAllowed, decline: true})
		return false
	}
	if c.m.isSelf(c.login, c.e164) {
		c.logFor(ctx).Info().Str("to", c.e164).Msg("Outgoing call not placed: own number")
		c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeOutSelf, decline: true})
		return false
	}
	if c.room.Encrypted {
		c.logFor(ctx).Error().Stringer("room_id", c.room.RoomID).Msg("Refusing to place a call in an encrypted room")
		c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeOutEncrypted, decline: true})
		return false
	}

	// 5. LiveKit service URL.
	if err := c.resolveServiceURL(ctx); err != nil {
		c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("No LiveKit service URL, not placing the call")
		c.failOutbound(c.noAudioFail())
		return false
	}
	if c.stopRequested() {
		return c.failOutOnStop()
	}

	if c.trigger == triggerCommand {
		// 6. Callback ring (D6), after the brand-new-room delay (D18).
		if c.room.Created && !c.sleep(newRoomRingDelay) {
			return c.failOutOnStop()
		}
		if err := c.publishMembership(ctx); err != nil {
			return c.failOutPublish(ctx, err)
		}
		if c.stopRequested() {
			return c.failOutOnStop()
		}
		if err := c.sendRing(ctx); err != nil {
			c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("Failed to send the ring notification")
			c.failOutbound(c.noAudioFail())
			return false
		}
		if c.stopRequested() {
			return c.failOutOnStop()
		}
		_, c.ringSpan = c.child(ctx, "call.ring", trace.SpanKindInternal,
			obs.AttrCallID.String(c.id), obs.AttrMatrixEventID.String(c.ringEventID.String()))
		c.ringOpen = true
		c.ringTimer = c.cfg.Clock.NewTimer(c.cfg.RingTimeout)
		c.setState(stRinging)
		c.logFor(ctx).Info().Str("to", c.e164).Stringer("room_id", c.room.RoomID).
			Stringer("ring_event_id", c.ringEventID).Msg("Ringing owner for an outgoing call")
		c.startJoin()
		return true
	}

	// 7. Owner-initiated: ghost membership, then join before dialling (D4, D5).
	if err := c.publishMembership(ctx); err != nil {
		return c.failOutPublish(ctx, err)
	}
	if c.stopRequested() {
		return c.failOutOnStop()
	}
	c.setState(stJoining)
	c.logFor(ctx).Info().Str("to", c.e164).Stringer("room_id", c.room.RoomID).Str("trigger", c.trigger).
		Msg("Owner started a call")
	c.startJoin()
	return true
}

// failOutPublish is the outbound failure for a membership publish error; it returns false. A slot that
// declares encryption is refused like an encrypted room.
func (c *call) failOutPublish(ctx context.Context, err error) bool {
	if errors.Is(err, errSlotEncrypted) {
		c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: noticeOutEncrypted, decline: true})
		return false
	}
	c.logFor(ctx).Warn().Str("error", scrubErr(err)).Msg("Failed to publish the ghost call membership")
	c.failOutbound(c.noAudioFail())
	return false
}

// noAudioFail is the failure for LiveKit/membership/service URL problems before dialling.
func (c *call) noAudioFail() failSpec {
	return failSpec{outcome: outcomeFailed, reason: reasonError, notice: failedNotice(failReasonNoAudio), decline: true}
}

// failOutOnStop is the outbound setup failure branch for a shutdown request; it returns false.
func (c *call) failOutOnStop() bool {
	c.logFor(c.setupCtx).Info().Msg("Bridge stopping during outgoing call setup")
	c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonBridgeStop, notice: noticeOutNotPlaced})
	return false
}

// sleep waits d on the call clock; false if shutdown was requested meanwhile.
func (c *call) sleep(d time.Duration) bool {
	t := c.cfg.Clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-c.quit:
		return false
	}
}

// lookupRoom runs Host.LookupCallRoom inside call.resolve_room.
func (c *call) lookupRoom(ctx context.Context) (*core.CallRoom, error) {
	sctx, span := c.child(ctx, "call.resolve_room", trace.SpanKindInternal,
		obs.AttrCallID.String(c.id), obs.AttrMatrixRoomID.String(c.roomID.String()))
	rctx, cancel := context.WithTimeout(sctx, resolveRoomTimeout)
	room, err := c.cfg.Host.LookupCallRoom(rctx, c.login, c.roomID)
	cancel()
	if err == nil && (room == nil || room.Ghost == nil) {
		err = errors.New("host returned no room or ghost")
	}
	if err != nil {
		if !errors.Is(err, core.ErrNotCallRoom) {
			obs.Fail(span, err)
		}
		c.endSpan(span)
		return nil, err
	}
	span.SetAttributes(obs.AttrCallRemoteE164.String(room.RemoteE164))
	c.endSpan(span)
	return room, nil
}

// dial places the INVITE (from Joining when the session is ready, or from Ringing on the owner's answer
// with the session ready).
func (c *call) dial() {
	if c.cfg.Dial == nil {
		c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected, notice: failedNotice(failReasonNotPlaced), decline: true})
		return
	}
	out, err := c.cfg.Dial(c.login, sipua.DialRequest{Target: c.e164, Format: c.cfg.DialNumberFormat,
		Timeout: c.cfg.DialTimeout + 30*time.Second})
	if err == nil && out == nil {
		err = errors.New("dial returned no call")
	}
	if err != nil {
		if errors.Is(err, sipua.ErrNotRegistered) {
			c.logFor(c.setupCtx).Warn().Str("to", c.e164).Msg("Outgoing call not placed: SIP account is not registered")
			c.failOutbound(failSpec{outcome: outcomeRejected, reason: reasonRejected,
				notice: failedNotice(failReasonNotRegistered), decline: true})
			return
		}
		c.logFor(c.setupCtx).Warn().Str("to", c.e164).Str("error", scrubErr(err)).Msg("Failed to place the call")
		c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonError, notice: failedNotice(failReasonNotPlaced), decline: true})
		return
	}
	c.out = out
	c.sipEvents = out.Events()
	c.dialInfo = out.Info()
	c.dialStartedAt = c.now()
	c.log = c.log.With().Str("sip_call_id", c.dialInfo.CallID).Logger()
	_, c.inviteSpan = c.child(c.setupCtx, "sip.invite", trace.SpanKindClient,
		obs.AttrCallID.String(c.id),
		obs.AttrSIPMethod.String("INVITE"),
		obs.AttrSIPCallID.String(c.dialInfo.CallID))
	c.inviteOpen = true
	c.setupSpan.SetAttributes(obs.AttrSIPCallID.String(c.dialInfo.CallID))
	c.dialTimer = c.cfg.Clock.NewTimer(c.cfg.DialTimeout)
	c.setState(stDialing)
	c.logFor(c.setupCtx).Info().Str("to", c.e164).Str("request_user", c.dialInfo.RequestUser).
		Str("sip_call_id", c.dialInfo.CallID).Msg("Dialing")
}

// eventTime returns ev.At, or now if the event carries no timestamp.
func (c *call) eventTime(ev sipua.CallEvent) time.Time {
	if ev.At.IsZero() {
		return c.now()
	}
	return ev.At
}

// onOutSIP handles a SIP event of an outbound call before it is Active.
func (c *call) onOutSIP(ev sipua.CallEvent) {
	switch ev.Kind {
	case sipua.EventAuthChallenged:
		c.spanEvent(c.inviteSpan, "sip.auth.challenge", c.eventTime(ev))
		if c.inviteOpen {
			c.inviteSpan.SetAttributes(obs.AttrSIPAuthChallenged.Bool(true))
		}
	case sipua.EventProvisional:
		c.onProvisional(ev)
	case sipua.EventAnswered:
		if c.state == stDialing || c.state == stEarly {
			c.connect(ev)
		}
	case sipua.EventEnded:
		c.recordSIPEnd(ev)
		if c.state != stDialing && c.state != stEarly {
			return
		}
		switch ev.Cause {
		case sipua.EndRemoteRejected:
			outcome := outcomeFor(ev.StatusCode)
			tone := media.ToneReorder
			if outcome == outcomeBusy {
				tone = media.ToneBusy
			}
			c.logFor(c.setupCtx).Info().Int("status", ev.StatusCode).Str("reason", obs.Scrub(ev.Reason)).
				Stringer("cause", ev.Cause).Msg("Outgoing call rejected by the remote side")
			c.failOutbound(failSpec{outcome: outcome, reason: reasonRemoteRej,
				notice: failedNotice(reasonText(ev.StatusCode)), tone: tone, decline: true})
		case sipua.EndFailed:
			c.logFor(c.setupCtx).Warn().Int("status", ev.StatusCode).Str("error", scrubErr(ev.Err)).
				Msg("Outgoing call failed: the provider did not respond")
			c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonError,
				notice: failedNotice(failReasonNoResponse), tone: media.ToneReorder, decline: true})
		default:
			c.logFor(c.setupCtx).Warn().Int("status", ev.StatusCode).Stringer("cause", ev.Cause).
				Str("error", scrubErr(ev.Err)).Msg("Outgoing call ended before it was answered")
			c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonError,
				notice: failedNotice(failReasonNotPlaced), tone: media.ToneReorder, decline: true})
		}
	}
}

// onProvisional handles a 1xx: span event, call progress log once per status, ringback / early media.
func (c *call) onProvisional(ev sipua.CallEvent) {
	s := ev.StatusCode
	c.spanEvent(c.inviteSpan, "sip.response.received", c.eventTime(ev), obs.AttrSIPStatusCode.Int(s))
	if c.progressSeen == nil {
		c.progressSeen = make(map[int]bool)
	}
	if !c.progressSeen[s] {
		c.progressSeen[s] = true
		c.logFor(c.setupCtx).Info().Int("status", s).Bool("early_media", ev.EarlyMedia).Msg("Call progress")
	}
	if s < 180 || s > 199 {
		return
	}
	early := ev.EarlyMedia && ev.Media != nil
	switch c.state {
	case stDialing:
		c.ringbackAt = c.eventTime(ev)
		c.ringbackCtx, c.ringbackSpan = obs.StartChild(c.setupCtx, "call.ringback",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithTimestamp(c.ringbackAt),
			trace.WithAttributes(obs.AttrCallID.String(c.id), obs.AttrSIPEarlyMedia.Bool(false)))
		c.ringbackOpen = true
		c.setState(stEarly)
		if early {
			c.startEarlyMedia(ev)
		} else {
			c.startRingback()
		}
	case stEarly:
		if early && c.bridge == nil {
			c.startEarlyMedia(ev)
		}
	}
}

// mediaConfig returns the media configuration of this call.
func (c *call) mediaConfig(early bool) media.Config {
	return media.Config{Clock: c.cfg.Clock, Log: c.log, RTPTimeout: c.cfg.RTPTimeout, Early: early}
}

// startRingback plays the generated ringback into LiveKit unless a tone or an early bridge is running.
func (c *call) startRingback() {
	if c.tone != nil || c.bridge != nil || c.session == nil {
		return
	}
	t, err := c.m.startTone(c.mediaConfig(false), c.session.Audio(), media.ToneRingback)
	if err != nil {
		c.logFor(c.setupCtx).Warn().Str("error", scrubErr(err)).Msg("Failed to start the ringback tone")
		return
	}
	c.tone = t
}

// stopTone stops the ringback/failure tone, if any.
func (c *call) stopTone() {
	if c.tone != nil {
		c.tone.Stop()
		c.tone = nil
	}
}

// startEarlyMedia starts the media bridge in early mode on the 183 leg (media.start under call.ringback).
func (c *call) startEarlyMedia(ev sipua.CallEvent) {
	if c.session == nil {
		return
	}
	c.stopTone()
	_, ms := c.child(c.ringbackCtx, "media.start", trace.SpanKindInternal, obs.AttrCallID.String(c.id))
	br, err := c.cfg.StartMedia(c.mediaConfig(true), ev.Media, c.session.Audio())
	if err != nil {
		obs.Fail(ms, err)
		c.endSpan(ms)
		c.logFor(c.setupCtx).Warn().Str("error", scrubErr(err)).Msg("Failed to start the early media bridge")
		c.startRingback()
		return
	}
	c.endSpan(ms)
	c.bridge = br
	c.bridgeDone = br.Done()
	if c.ringbackOpen {
		c.ringbackSpan.SetAttributes(obs.AttrSIPEarlyMedia.Bool(true))
	}
	c.logFor(c.setupCtx).Info().Msg("Relaying early media")
}

// onEarlyBridgeDone: the early-media bridge stopped by itself before answer; fall back to ringback.
func (c *call) onEarlyBridgeDone() {
	if c.bridge == nil {
		return
	}
	c.logFor(c.setupCtx).Warn().Str("error", scrubErr(c.bridge.Err())).Msg("Early media bridge stopped, playing ringback")
	c.bridge.Stop()
	c.bridge = nil
	c.startRingback()
}

// endRingback ends call.ringback once with outcome at t.
func (c *call) endRingback(outcome string, t time.Time) {
	if !c.ringbackOpen {
		return
	}
	c.ringbackOpen = false
	if t.Before(c.ringbackAt) {
		t = c.ringbackAt
	}
	c.ringbackEnd = t
	c.ringbackSpan.SetAttributes(obs.AttrCallOutcome.String(outcome))
	c.ringbackSpan.End(trace.WithTimestamp(t))
}

// connect runs on EventAnswered (PHASE3-SPEC §6.5 connect).
func (c *call) connect(ev sipua.CallEvent) {
	okAt := ev.Answer.OKReceivedAt
	if okAt.IsZero() {
		okAt = c.now()
	}
	ackAt := ev.Answer.AckSentAt
	if ackAt.IsZero() {
		ackAt = c.now()
	}
	c.endRingback(outcomeAnswered, okAt)
	c.spanEvent(c.inviteSpan, "sip.response.received", okAt, obs.AttrSIPStatusCode.Int(200))
	if c.inviteOpen {
		c.inviteOpen = false
		end := okAt
		if end.Before(c.dialStartedAt) {
			end = c.dialStartedAt
		}
		c.inviteSpan.SetAttributes(obs.AttrSIPStatusCode.Int(200))
		c.inviteSpan.End(trace.WithTimestamp(end))
	}
	c.spanEvent(c.setupSpan, "sip.ack.sent", ackAt)
	c.stopTone()
	if c.dialTimer != nil {
		c.dialTimer.Stop()
		c.dialTimer = nil
	}
	c.answer = ev.Answer

	var mediaErr error
	if c.bridge != nil {
		c.bridge.Connect()
	} else {
		var mattrs []attribute.KeyValue
		if host, _, err := net.SplitHostPort(ev.Answer.RemoteRTP); err == nil {
			mattrs = append(mattrs, obs.AttrNetworkPeerAddress.String(host))
		}
		mattrs = append(mattrs, obs.AttrCallID.String(c.id))
		_, ms := c.child(c.setupCtx, "media.start", trace.SpanKindInternal, mattrs...)
		switch {
		case ev.Media == nil || c.session == nil:
			mediaErr = errNoSIPMedia
		default:
			br, err := c.cfg.StartMedia(c.mediaConfig(false), ev.Media, c.session.Audio())
			if err != nil {
				mediaErr = err
			} else {
				c.bridge = br
				c.bridgeDone = br.Done()
			}
		}
		if mediaErr != nil {
			obs.Fail(ms, mediaErr)
		}
		c.endSpan(ms)
	}

	c.setupSpan.SetAttributes(obs.AttrCallOutcome.String(outcomeAnswered), obs.AttrSIPStatusCode.Int(200))
	c.setupSpan.End(trace.WithTimestamp(ackAt))
	c.setupEnded = true
	c.answered = true
	c.answeredAt = ackAt
	c.publish()
	c.setState(stActive)
	c.recordSetupDuration(max(okAt.Sub(c.dialStartedAt), 0), outcomeAnswered)
	c.setupRecorded = true

	if mediaErr != nil {
		c.logFor(c.setupCtx).Error().Str("error", scrubErr(mediaErr)).Msg("Failed to start the media bridge")
		c.teardown(byBridge, reasonError, false)
		return
	}
	c.statsTicker = c.cfg.Clock.NewTicker(statsInterval)
	c.scheduleRefresh()
	if len(c.lkOwners) == 0 && c.graceTimer == nil {
		c.graceTimer = c.cfg.Clock.NewTimer(ownerRTCGrace)
	}
	c.logFor(c.setupCtx).Info().Str("codec", ev.Answer.Codec).Str("remote_rtp", ev.Answer.RemoteRTP).
		Msg("Call connected")
}

// onOutMembership handles an owner call.member event routed to an outbound call.
func (c *call) onOutMembership(ev event) {
	if c.state == stActive {
		c.onMembership(ev)
		return
	}
	mem, ok := matrixrtc.ParseMembership(ev.rtc, c.now())
	if !ok || mem.UserID != c.owner || ev.rtc.Sender != c.owner {
		return
	}
	key := mem.Key
	c.noteOwnerMem(mem)
	switch c.state {
	case stRinging:
		if !ev.rtc.Timestamp.IsZero() && ev.rtc.Timestamp.Before(c.ringSentAt.Add(-staleMembershipSlack)) {
			c.logFor(c.setupCtx).Debug().Str("device", mem.DeviceID).Msg("Ignoring owner membership older than the ring")
			return
		}
		if !mem.Live {
			return
		}
		c.endRing(ev.from, outcomeAnswered)
		c.devices[key] = struct{}{}
		if c.ringTimer != nil {
			c.ringTimer.Stop()
			c.ringTimer = nil
		}
		c.logFor(c.setupCtx).Info().Str("device", mem.DeviceID).Msg("Owner answered the callback ring")
		c.markRingRead()
		if c.session != nil {
			c.dial()
		} else {
			c.setState(stJoining)
		}
	case stJoining, stDialing, stEarly:
		if mem.Live {
			c.devices[key] = struct{}{}
			return
		}
		if !c.removeDevice(key) || len(c.devices) > 0 {
			return
		}
		c.logFor(c.setupCtx).Info().Str("device", mem.DeviceID).Stringer("state", c.state).
			Msg("Owner left before the call was answered")
		c.cancelOutbound(failSpec{outcome: outcomeCancelled, reason: reasonCancel, notice: noticeOutCancelled})
	}
}

// onOutDecline: the owner declined the callback ring (command path).
func (c *call) onOutDecline(ev event) {
	if c.state != stRinging {
		return
	}
	target, ok := matrixrtc.DeclineTarget(ev.rtc)
	if !ok || target != c.ringEventID || ev.rtc.Sender != c.owner {
		c.logFor(c.setupCtx).Debug().Stringer("sender", ev.rtc.Sender).Msg("Ignoring decline not from the owner for this ring")
		return
	}
	c.logFor(c.setupCtx).Info().Msg("Owner declined the callback ring")
	c.endRing(ev.from, outcomeDeclined)
	c.failOutbound(failSpec{outcome: outcomeDeclined, reason: reasonDecline, notice: noticeOutNotPlaced})
}

// onOutShutdown handles EndLogin/Stop for an outbound call.
func (c *call) onOutShutdown() {
	switch c.state {
	case stRinging, stJoining:
		c.failOutbound(failSpec{outcome: outcomeFailed, reason: reasonBridgeStop, notice: noticeOutNotPlaced})
	case stDialing, stEarly:
		c.cancelOutbound(failSpec{outcome: outcomeFailed, reason: reasonBridgeStop, notice: failedNotice(failReasonBridgeStop)})
	case stActive:
		c.teardown(byBridge, reasonBridgeStop, false)
	}
}

// onOutHangupCmd handles Manager.Hangup for an outbound call.
func (c *call) onOutHangupCmd() {
	switch c.state {
	case stRinging, stJoining, stDialing, stEarly:
		c.logFor(c.setupCtx).Info().Stringer("state", c.state).Msg("Owner cancelled the outgoing call by command")
		c.cancelOutbound(failSpec{outcome: outcomeCancelled, reason: reasonCancel, notice: noticeOutCancelled})
	case stActive:
		c.logFor(c.setupCtx).Info().Msg("Owner hung up by command")
		c.teardown(byOwner, reasonBye, false)
	}
}

// onOutRingTimeout: the callback ring was not answered.
func (c *call) onOutRingTimeout() {
	if c.state != stRinging {
		return
	}
	c.logFor(c.setupCtx).Info().Msg("Callback ring timeout")
	c.endRing(trace.SpanContext{}, outcomeTimeout)
	c.failOutbound(failSpec{outcome: outcomeTimeout, reason: reasonRingTimeout, notice: noticeOutNoAnswer})
}

// onDialTimeout: no 200 OK within DialTimeout.
func (c *call) onDialTimeout() {
	if c.state != stDialing && c.state != stEarly {
		return
	}
	c.logFor(c.setupCtx).Info().Dur("dial_timeout", c.cfg.DialTimeout).Msg("Outgoing call not answered in time")
	c.cancelOutbound(failSpec{outcome: outcomeTimeout, reason: reasonRingTimeout,
		notice: failedNotice(failReasonNoAnswer), tone: media.ToneReorder, decline: true})
}

// onOutDisconnected: our LiveKit session dropped before the call was answered.
func (c *call) onOutDisconnected() {
	switch c.state {
	case stRinging, stJoining:
		c.dropSession()
		if c.canRetryJoin() {
			c.startJoin()
			return
		}
		c.failOutbound(c.noAudioFail())
	case stDialing, stEarly:
		c.cancelOutbound(failSpec{outcome: outcomeFailed, reason: reasonRTCLost,
			notice: failedNotice(failReasonNoAudio), decline: true})
	}
}

// cancelOutbound hangs up the SIP call (CANCEL or BYE) if one exists, then runs the failure branch.
func (c *call) cancelOutbound(f failSpec) {
	c.sipHangup()
	c.failOutbound(f)
}

// failOutbound is the outbound failure branch (PHASE3-SPEC §6.7), inside call.outbound.
func (c *call) failOutbound(f failSpec) {
	c.setState(stEnding)
	c.m.releaseActive(c)
	c.stopTimers()
	ctx := c.setupCtx

	// 2. SIP: make sure the dialog ends, then close sip.invite and call.ringback.
	if c.out != nil {
		if !c.sipEnded {
			c.sipHangup()
			c.waitSIPEnd()
		}
		c.endInvite()
	}
	c.endRingback(f.outcome, c.now())
	c.outcome, c.endReason = f.outcome, f.reason

	// 3. In-flight join.
	c.cancelJoinAndWait()

	// 4. Command path: read receipt and redaction of the callback ring (not on decline).
	if c.ringEventID != "" {
		c.markRingRead()
		if f.outcome != outcomeDeclined {
			c.redactRing(ctx)
		}
		c.withdrawRing(ctx)
	}

	// 5. Early bridge and ringback tone.
	if c.bridge != nil {
		c.bridge.Stop()
		c.bridge = nil
		c.bridgeDone = nil
	}
	c.stopTone()

	// 6. Failure tone.
	if f.tone != 0 && c.session != nil {
		c.playFailureTone(f.tone)
	}

	// 7. Decline the owner's notification.
	if f.decline && c.ownerNotif != "" && c.room != nil {
		c.sendDecline(ctx)
	}

	// 8. Ghost membership (sticky leave, then the legacy clear) and LiveKit.
	c.leaveMembership(ctx)
	if c.session != nil {
		c.leaveSession(ctx, c.session)
		c.session = nil
		c.sessEvents = nil
	}

	// 9. Notice.
	if c.room != nil && f.notice != "" {
		c.sendNotice(ctx, f.notice, f.outcome)
	}

	// 10. End call.ring and call.outbound.
	c.endRing(trace.SpanContext{}, f.outcome)
	c.endedAt = c.now()
	c.setupSpan.SetAttributes(obs.AttrCallOutcome.String(f.outcome), obs.AttrCallEndReason.String(f.reason))
	if c.sipStatus != 0 {
		c.setupSpan.SetAttributes(obs.AttrSIPStatusCode.Int(c.sipStatus))
	}
	if f.outcome == outcomeFailed {
		c.setupSpan.SetStatus(codes.Error, f.reason)
	}
	c.setupSpan.End(trace.WithTimestamp(c.endedAt))
	c.setupEnded = true
	c.logFor(ctx).Info().Str("outcome", f.outcome).Str("end_reason", f.reason).Int("sip_status", c.sipStatus).
		Msg("Outgoing call not connected")
	c.finish()
}

// endInvite ends sip.invite with the received final status (or Error if none).
func (c *call) endInvite() {
	if !c.inviteOpen {
		return
	}
	c.inviteOpen = false
	t := c.now()
	if c.sipEnded && c.sipStatus >= 200 {
		c.spanEvent(c.inviteSpan, "sip.response.received", t, obs.AttrSIPStatusCode.Int(c.sipStatus))
		c.inviteSpan.SetAttributes(obs.AttrSIPStatusCode.Int(c.sipStatus))
		if r := strings.TrimSpace(c.sipReason); r != "" {
			c.inviteSpan.SetAttributes(obs.AttrSIPReason.String(obs.Scrub(r)))
		}
	} else {
		c.inviteSpan.SetStatus(codes.Error, "no final response")
	}
	c.inviteSpan.End(trace.WithTimestamp(t))
}

// playFailureTone plays p for failureToneLength (ends early on shutdown).
func (c *call) playFailureTone(p media.TonePattern) {
	t, err := c.m.startTone(c.mediaConfig(false), c.session.Audio(), p)
	if err != nil {
		c.logFor(c.setupCtx).Warn().Str("error", scrubErr(err)).Msg("Failed to start the failure tone")
		return
	}
	timer := c.cfg.Clock.NewTimer(failureToneLength)
	select {
	case <-timer.C():
	case <-c.quit:
	}
	timer.Stop()
	t.Stop()
}

// sendDecline sends the MSC4310 decline of the owner's notification (matrixrtc.decline.send), best
// effort. A decline of a sticky notification is sent sticky with the notification's duration.
func (c *call) sendDecline(ctx context.Context) {
	sctx, span := c.child(ctx, "matrixrtc.decline.send", trace.SpanKindClient,
		obs.AttrCallID.String(c.id), obs.AttrMatrixEventID.String(c.ownerNotif.String()))
	octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
	var err error
	content := matrixrtc.DeclineContent(c.ownerNotif)
	if g, ok := c.rtcGhost(); ok && c.ownerNotifSticky > 0 {
		_, err = g.SendSticky(octx, c.room.RoomID, matrixrtc.RTCDecline, content, c.ownerNotifSticky)
		if errors.Is(err, core.ErrUnsupported) {
			_, err = c.room.Ghost.SendMessage(octx, c.room.RoomID, matrixrtc.RTCDecline, content)
		}
	} else {
		_, err = c.room.Ghost.SendMessage(octx, c.room.RoomID, matrixrtc.RTCDecline, content)
	}
	cancel()
	if err != nil {
		obs.Fail(span, err)
		c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to decline the owner's call notification")
	}
	c.endSpan(span)
}
