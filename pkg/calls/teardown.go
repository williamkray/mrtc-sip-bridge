package calls

import (
	"context"
	"math"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// fail is the failure branch for unanswered calls (PHASE2-SPEC §8.6), inside call.inbound.
func (c *call) fail(f failSpec) {
	c.setState(stEnding)
	c.m.releaseActive(c)
	c.stopTimers()
	ctx := c.setupCtx

	// 1. SIP final response: issued by the transition (or 480 now), implicit for CANCEL (487 by sipgo).
	if !c.sipEnded && !c.finalIssued {
		c.finalIssued = true
		c.in.Reject(480, "Temporarily Unavailable")
	}
	c.waitSIPEnd()
	if c.answerFailed && c.sipCause == sipua.EndCancelled && f.outcome == outcomeFailed && f.reason == reasonError {
		// Caller hung up while we answered (edge case 3): the answer failure was the CANCEL.
		f = failSpec{outcome: outcomeCancelled, reason: reasonCancel, notice: noticeMissed}
		c.spanEvent(c.setupSpan, "sip.cancel.received", c.now())
	}
	if c.sipEnded && c.sipStatus != 0 {
		c.spanEvent(c.setupSpan, "sip.response.sent", c.now(), obs.AttrSIPStatusCode.Int(c.sipStatus))
	}
	c.outcome, c.endReason = f.outcome, f.reason

	// 2. In-flight join.
	c.cancelJoinAndWait()

	// 2b. Read receipt for the ring as the owner, after the SIP final response, so push-driven clients
	// drop their ring notification on every unanswered end (no-op if the answer path already sent it).
	c.markRingRead()

	// 3. Redact the ring (not on decline: the decline references it; D16).
	if c.ringEventID != "" && f.outcome != outcomeDeclined {
		c.redactRing(ctx)
	}
	// 3b. Withdraw a sticky ring.
	c.withdrawRing(ctx)
	// 4. Leave the ghost membership (sticky leave, then the legacy clear).
	c.leaveMembership(ctx)
	// 5. Leave LiveKit.
	if c.session != nil {
		c.leaveSession(ctx, c.session)
		c.session = nil
		c.sessEvents = nil
	}
	// 6. Notice.
	if c.room != nil && f.notice != "" {
		c.sendNotice(ctx, f.notice, f.outcome)
	}
	// 7. End call.ring and call.inbound.
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

	// 8. Metrics, call.session, manager.
	c.finish()
}

// teardown ends an answered call (PHASE2-SPEC §8.7) inside the call.teardown linked root.
func (c *call) teardown(by, reason string, remoteBye bool) {
	c.setState(stEnding)
	c.m.releaseActive(c)
	c.outcome, c.hangupBy, c.endReason = outcomeAnswered, by, reason
	c.endedAt = c.now()
	c.stopTimers()
	ctx, span := obs.StartLinkedRoot(context.Background(), "call.teardown", c.setupCtx,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(c.endedAt),
		trace.WithAttributes(
			obs.AttrCallID.String(c.id),
			obs.AttrCallHangupBy.String(by),
			obs.AttrCallEndReason.String(reason)))
	c.teardownSC = span.SpanContext()
	if remoteBye {
		c.spanEvent(span, "sip.bye.received", c.endedAt)
	}

	// 1. media.stop
	if c.bridge != nil {
		_, ms := c.child(ctx, "media.stop", trace.SpanKindInternal, obs.AttrCallID.String(c.id))
		c.bridge.Stop()
		st := c.bridge.Stats()
		c.flushStats(st, false)
		ms.SetAttributes(c.rtpSummary()...)
		c.endSpan(ms)
	}
	// 2. SIP BYE unless the call ended remotely (sipua ignores Hangup after the dialog ended).
	if !c.sipEnded {
		_, bs := c.child(ctx, "sip.bye", trace.SpanKindClient, obs.AttrCallID.String(c.id))
		c.sipHangup()
		c.waitSIPEnd()
		if c.sipEnded {
			bs.SetAttributes(obs.AttrSIPStatusCode.Int(c.sipStatus))
		}
		c.endSpan(bs)
	}
	// 3. rtc.leave
	if c.session != nil {
		c.leaveSession(ctx, c.session)
		c.session = nil
		c.sessEvents = nil
	}
	// 4. Withdraw a sticky ring, then leave the ghost membership (sticky leave, then the legacy clear).
	c.withdrawRing(ctx)
	c.leaveMembership(ctx)
	// 5. Notice with the talk time.
	if c.room != nil {
		text := answeredNotice(c.endedAt.Sub(c.answeredAt))
		if c.isOutbound() {
			text = outAnsweredNotice(c.endedAt.Sub(c.answeredAt))
		}
		c.sendNotice(ctx, text, outcomeAnswered)
	}
	c.endSpan(span)
	c.finish()
}

// redactRing redacts the ring notification (matrixrtc.ring.redact), best effort.
func (c *call) redactRing(ctx context.Context) {
	sctx, span := c.child(ctx, "matrixrtc.ring.redact", trace.SpanKindClient,
		obs.AttrCallID.String(c.id), obs.AttrMatrixEventID.String(c.ringEventID.String()))
	octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
	err := c.room.Ghost.Redact(octx, c.room.RoomID, c.ringEventID, "call ended")
	cancel()
	if err != nil {
		obs.Fail(span, err)
		c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to redact the ring notification")
	}
	c.endSpan(span)
}

// clearMembership sets the ghost membership to {} (matrixrtc.membership.clear), best effort.
func (c *call) clearMembership(ctx context.Context) {
	sctx, span := c.child(ctx, "matrixrtc.membership.clear", trace.SpanKindClient, obs.AttrCallID.String(c.id))
	octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
	_, err := c.room.Ghost.SendState(octx, c.room.RoomID, matrixrtc.CallMember, c.memberStateKey(), matrixrtc.LeaveContent())
	cancel()
	if err != nil {
		obs.Fail(span, err)
		c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to clear the ghost call membership")
	}
	c.endSpan(span)
}

// sendNotice posts a timeline notice (call.notice.send; the text is never recorded), best effort.
func (c *call) sendNotice(ctx context.Context, text, kind string) {
	sctx, span := c.child(ctx, "call.notice.send", trace.SpanKindInternal,
		obs.AttrCallID.String(c.id), obs.AttrCallOutcome.String(kind))
	octx, cancel := context.WithTimeout(sctx, matrixOpTimeout)
	err := c.cfg.Host.SendCallNotice(octx, c.room, text)
	cancel()
	if err != nil {
		obs.Fail(span, err)
		c.logFor(sctx).Warn().Str("error", scrubErr(err)).Msg("Failed to send the call notice")
	}
	c.endSpan(span)
}

// finish records the end-of-call metrics and call.session, logs the summary and unregisters the call.
func (c *call) finish() {
	c.setState(stEnded)
	duration := 0.0
	if c.answered {
		duration = c.endedAt.Sub(c.answeredAt).Seconds()
	}
	l := c.logFor(c.setupCtx).Info().
		Str("outcome", c.outcome).
		Str("end_reason", c.endReason).
		Int("sip_status", c.sipStatus).
		Float64("duration_s", duration).
		Str("direction", c.direction)
	if c.answered {
		st := c.lastStats
		l = l.Str("hangup_by", c.hangupBy).
			Uint64("rtp_rx", st.SIPRx).Uint64("rtp_tx", st.SIPTx).
			Uint64("rtp_lost", st.SIPLost).Uint64("rtp_late", st.SIPLate).
			Float64("jitter_max_ms", roundMS(c.jitterMax))
		if c.jitterN > 0 {
			l = l.Float64("jitter_avg_ms", roundMS(c.jitterSum/time.Duration(c.jitterN)))
		}
	}
	l.Msg("Call ended")
	c.recordEnd()
	c.emitSession()
	c.m.remove(c)
}

// roundMS converts d to milliseconds rounded to 0.01 ms.
func roundMS(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*100) / 100
}
