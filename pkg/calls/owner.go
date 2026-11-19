package calls

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// onMembership handles a call.member event routed to this call. Only the owner's own memberships count
// (D14): ParseMembership already requires the state-key user to be the sender.
func (c *call) onMembership(ev event) {
	mem, ok := matrixrtc.ParseMembership(ev.rtc, c.now())
	if !ok || mem.UserID != c.owner || ev.rtc.Sender != c.owner {
		return
	}
	// Devices are keyed by Membership.Key (legacy: the state key; sticky: "sticky\x00"+sticky key): join
	// and leave both carry it, while device_id exists only on join. mem.DeviceID is for logs.
	key := mem.Key
	c.noteOwnerMem(mem)
	switch c.state {
	case stRinging:
		if !ev.rtc.Timestamp.IsZero() && ev.rtc.Timestamp.Before(c.ringSentAt.Add(-staleMembershipSlack)) {
			// Late appservice backlog (e.g. after a restart): not an answer to this ring.
			c.logFor(c.setupCtx).Debug().Str("device", mem.DeviceID).Msg("Ignoring owner membership older than the ring")
			return
		}
		if mem.Live {
			c.ownerAnswered(ev, mem)
		}
	case stAnswering:
		if mem.Live {
			c.devices[key] = struct{}{}
			return
		}
		if !c.removeDevice(key) || len(c.devices) > 0 || c.answerIssued {
			// With the answer issued, EventAnswered with no device left tears the call down.
			return
		}
		c.logFor(c.setupCtx).Info().Str("device", mem.DeviceID).Msg("Owner left before the call was connected")
		c.reject(480, "Temporarily Unavailable")
		c.fail(failSpec{outcome: outcomeMissed, reason: reasonBye, notice: noticeMissed})
	case stActive:
		if mem.Live {
			c.devices[key] = struct{}{}
			return
		}
		if !c.removeDevice(key) {
			return
		}
		if len(c.devices) == 0 {
			c.logFor(c.setupCtx).Info().Str("device", mem.DeviceID).Msg("Owner left the call")
			c.teardown(byOwner, reasonBye, false)
			return
		}
		c.ownerDeviceLeft(mem.DeviceID)
	}
}

// noteOwnerMem records a live owner membership as the freshest one (ownerMem/ownerFormat).
func (c *call) noteOwnerMem(mem matrixrtc.Membership) {
	if !mem.Live {
		return
	}
	c.learnOwnerIdentities(mem)
	if c.ownerMem.Key == "" || !mem.Created.Before(c.ownerMem.Created) {
		c.ownerMem = mem
		c.ownerFormat = mem.Format
	}
}

func (c *call) removeDevice(device string) bool {
	if _, ok := c.devices[device]; !ok {
		return false
	}
	delete(c.devices, device)
	return true
}

// ownerAnswered is the Ringing -> Answering transition on the owner's first live membership.
func (c *call) ownerAnswered(ev event, mem matrixrtc.Membership) {
	c.endRing(ev.from, outcomeAnswered)
	c.devices[mem.Key] = struct{}{}
	c.logFor(c.setupCtx).Info().Str("device", mem.DeviceID).Msg("Owner answered")

	if c.ringTimer != nil {
		c.ringTimer.Stop()
		c.ringTimer = nil
	}
	if c.keepalive != nil {
		c.keepalive.Stop()
		c.keepalive = nil
	}
	c.answerTimer = c.cfg.Clock.NewTimer(answerTimeout)
	c.setState(stAnswering)
	if c.session != nil {
		c.doAnswer() // only queues a sipua command: issue it before the slow HTTP calls below
	}

	// Read receipt for the ring as the owner so push-driven clients stop ringing (D16). Runs after
	// doAnswer so the 200 OK is not delayed by two inline HTTP requests.
	c.markRingRead()
}

// markRingRead sends the owner's read receipt for the ring (once), best effort: errors are logged and
// never fail the call or its teardown.
func (c *call) markRingRead() {
	if c.ringRead || c.ringEventID == "" || c.room == nil {
		return
	}
	c.ringRead = true
	rctx, cancel := context.WithTimeout(c.setupCtx, matrixOpTimeout)
	err := c.cfg.Host.MarkReadAsOwner(rctx, c.login, c.room.RoomID, c.ringEventID)
	cancel()
	switch {
	case errors.Is(err, core.ErrNoDoublePuppet):
		c.logFor(c.setupCtx).Debug().Msg("Owner has no double puppet, not marking the ring read")
	case err != nil:
		c.logFor(c.setupCtx).Warn().Str("error", scrubErr(err)).Msg("Failed to mark the ring read as the owner")
	}
}

// ownerDeviceLeft records one of several owner devices leaving an active call (call.owner.left linked
// root plus a buffered call.session event).
func (c *call) ownerDeviceLeft(device string) {
	t := c.now()
	ctx, span := obs.StartLinkedRoot(context.Background(), "call.owner.left", c.setupCtx,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(t),
		trace.WithAttributes(obs.AttrCallID.String(c.id), obs.AttrMatrixRTCDeviceID.String(device)))
	c.endSpan(span)
	c.addPast("call.owner.left", t, obs.AttrMatrixRTCDeviceID.String(device))
	c.logFor(ctx).Info().Str("device", device).Int("devices_left", len(c.devices)).
		Msg("Owner device left, call continues on another device")
}
