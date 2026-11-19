package calls

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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

// call is one call (inbound or outbound). The fields in the first block are safe for concurrent use
// (set before the actor starts or immutable after being published); everything else is owned by the
// run goroutine.
type call struct {
	m         *Manager
	cfg       *Config
	login     core.LoginRef
	localE164 string
	in        sipua.InboundCall // inbound only
	info      sipua.InviteInfo  // inbound only
	id        string
	deviceID  string
	log       zerolog.Logger // clean logger with component, call_id, sip_call_id
	busy      bool           // set before run starts
	direction string         // dirInbound or dirOutbound
	dirAttr   attribute.KeyValue
	optDir    metric.MeasurementOption

	// Outbound, set before run starts.
	trigger    string          // triggerCommand or triggerOwnerMembership
	triggerCtx context.Context // carries only the trigger span context (cmd.call or matrixrtc.ingest)
	triggerAt  time.Time       // trigger time (call.outbound start)
	ownerNotif id.EventID      // the owner's rtc.notification (owner trigger, ring mode), "" otherwise
	// ownerFormat and ownerMem are the freshest live owner membership seen in the room (set at the owner
	// trigger, updated by owner memberships routed to the call); zero when unknown. Actor-owned once
	// the call runs. WP5 reads them to choose the ghost's dialect.
	ownerFormat matrixrtc.Format
	ownerMem    matrixrtc.Membership
	// ownerNotifSticky is the sticky duration of the owner's notification (owner trigger), 0 = not
	// sticky or unknown; the decline is then sent sticky too.
	ownerNotifSticky time.Duration
	cmdNumber        string // command path by number: the normalized E.164
	cmdRoom          bool   // command path by room (roomID set at creation)

	events   chan event
	quit     chan struct{} // closed by shutdown (evShutdown)
	quitOnce sync.Once
	done     chan struct{} // closed when the actor finished
	roomID   id.RoomID     // set once before setRoutable

	// ---- actor-owned ----
	state      state
	pub        atomic.Pointer[Snapshot] // published view of the call for Manager.Snapshot (snapshot.go)
	startedAt  time.Time                // INVITE receipt
	e164       string
	owner      id.UserID
	room       *core.CallRoom
	serviceURL string

	setupCtx   context.Context
	setupSpan  trace.Span
	setupEnded bool

	ringSpan      trace.Span
	ringOpen      bool
	ringSentAt    time.Time
	ringEndedAt   time.Time
	ringEventID   id.EventID
	ringRead      bool // the owner's read receipt for the ring was sent
	memberEventID id.EventID
	memberCreated time.Time
	memberExpires time.Duration

	// MatrixRTC 2.0 publishing (format.go, rtcmember.go), actor-owned.
	plan            plan
	memberID        string // sticky member.id of this call (never reused)
	stickyEventID   id.EventID
	stickyPublished bool
	keeper          *delayKeeper
	ringSticky      bool // the ring was sent sticky (withdrawn at the end)
	ringWithdrawn   bool
	ownerIDs        []string // LiveKit identities of the owner's live memberships (bounded)

	joinAttempt   int
	joinFallbacks int // join starts that were MSC4195 -> sfu_get switches (not counted against maxJoinAttempts)
	joinInFlight  bool
	joinCancel    context.CancelFunc
	session       rtc.Session
	sessEvents    <-chan rtc.Event
	lkOwners      map[string]struct{}
	joinResults   chan event // LiveKit join results (own channel: never dropped by a full event queue)

	answerIssued bool
	answerFailed bool
	answerCtx    context.Context
	answerSpan   trace.Span
	devices      map[string]struct{}

	sipEvents   <-chan sipua.CallEvent
	sipEnded    bool
	sipStatus   int
	sipReason   string // outbound: remote reason phrase of the final response (scrubbed before use)
	sipCause    sipua.EndCause
	finalIssued bool // we issued Reject or Hangup

	// ---- outbound, actor-owned ----
	out           sipua.OutboundCall
	dialInfo      sipua.DialInfo
	dialStartedAt time.Time
	inviteSpan    trace.Span
	inviteOpen    bool
	ringbackCtx   context.Context
	ringbackSpan  trace.Span
	ringbackOpen  bool
	ringbackAt    time.Time // first 18x
	ringbackEnd   time.Time // 200 OK received or failure
	progressSeen  map[int]bool
	tone          media.Tone
	dialTimer     clock.Timer

	ringTimer    clock.Timer
	keepalive    clock.Ticker
	answerTimer  clock.Timer
	graceTimer   clock.Timer
	joinTimer    clock.Timer
	statsTicker  clock.Ticker
	refreshTimer clock.Timer
	stickyTimer  clock.Timer // sticky membership refresh (MemberStickyRefresh)

	bridge     media.Bridge
	bridgeDone <-chan struct{}
	answer     sipua.AnswerInfo
	answered   bool
	answeredAt time.Time
	endedAt    time.Time

	outcome    string
	endReason  string
	hangupBy   string
	teardownSC trace.SpanContext

	lastStats     media.Stats
	jitterMax     time.Duration
	jitterSum     time.Duration
	jitterN       int
	degraded      bool
	past          []pastEvent
	setupRecorded bool
}

func newCall(m *Manager, login core.LoginRef, localE164 string, sc sipua.InboundCall) *call {
	info := sc.Info()
	cid := newCallID(m.cfg.Clock.Now())
	c := newActor(m, login, cid, dirInbound,
		m.cfg.Log.With().Str("component", "calls").Str("call_id", cid).Str("sip_call_id", info.CallID).Logger())
	c.localE164 = localE164
	c.in = sc
	c.info = info
	c.sipEvents = sc.Events()
	return c
}

// newActor returns a call with the fields shared by both directions.
func newActor(m *Manager, login core.LoginRef, cid, direction string, log zerolog.Logger) *call {
	c := &call{
		m:         m,
		cfg:       &m.cfg,
		login:     login,
		id:        cid,
		deviceID:  deviceIDFor(cid),
		log:       log,
		direction: direction,
		events:    make(chan event, callEventBuffer),
		// One result per attempt, so the join goroutine never blocks or drops.
		joinResults: make(chan event, maxJoinAttempts+2), // +1: the free MSC4195 fallback attempt
		quit:        make(chan struct{}),
		done:        make(chan struct{}),
		devices:     make(map[string]struct{}),
		lkOwners:    make(map[string]struct{}),
	}
	if direction == dirOutbound {
		c.dirAttr, c.optDir = attrOutbound, optOutbound
	} else {
		c.dirAttr, c.optDir = attrInbound, optInbound
	}
	return c
}

// isOutbound reports whether c is an outbound call.
func (c *call) isOutbound() bool { return c.direction == dirOutbound }

// shutdown asks the actor to end the call (evShutdown). Safe from any goroutine, idempotent.
func (c *call) shutdown() { c.quitOnce.Do(func() { close(c.quit) }) }

func (c *call) now() time.Time { return c.cfg.Clock.Now() }

// logFor returns the clean call logger enriched with the trace fields of ctx's span.
func (c *call) logFor(ctx context.Context) *zerolog.Logger { return obs.Logger(ctx, &c.log) }

// run is the actor goroutine.
func (c *call) run() {
	defer close(c.done)
	ok := false
	if c.isOutbound() {
		ok = c.setupOutbound()
	} else {
		ok = c.setup()
	}
	if !ok {
		return
	}
	c.loop()
}

func timerC(t clock.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C()
}

func tickerC(t clock.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C()
}

// loop is the actor's single select, until the call reaches stEnding (teardown runs inline).
func (c *call) loop() {
	quit := c.quit
	for c.state < stEnding {
		select {
		case ev := <-c.events:
			c.onEvent(ev)
		case <-quit:
			c.onEvent(event{kind: evShutdown})
		case ev := <-c.joinResults:
			c.onJoinResult(ev)
		case ev, ok := <-c.sipEvents:
			if !ok {
				c.sipEvents = nil
				continue
			}
			c.onSIP(ev)
		case ev, ok := <-c.sessEvents:
			if !ok {
				c.sessEvents = nil
				continue
			}
			c.onRTC(ev)
		case <-c.bridgeDone:
			c.bridgeDone = nil
			c.onBridgeDone()
		case <-timerC(c.ringTimer):
			c.ringTimer = nil
			c.onRingTimeout()
		case <-tickerC(c.keepalive):
			if c.state == stRinging && c.in != nil {
				c.in.Ringing()
			}
		case <-timerC(c.answerTimer):
			c.answerTimer = nil
			c.onAnswerTimeout()
		case <-timerC(c.dialTimer):
			c.dialTimer = nil
			c.onDialTimeout()
		case <-timerC(c.graceTimer):
			c.graceTimer = nil
			if c.state == stDialing || c.state == stEarly {
				c.logFor(c.setupCtx).Warn().Msg("Owner absent from LiveKit for the grace period while dialing")
				c.cancelOutbound(failSpec{outcome: outcomeCancelled, reason: reasonRTCLost, notice: noticeOutCancelled})
				continue
			}
			if c.state == stActive {
				devices := make([]string, 0, len(c.devices))
				for d := range c.devices {
					devices = append(devices, d)
				}
				owners := make([]string, 0, len(c.lkOwners))
				for o := range c.lkOwners {
					owners = append(owners, o)
				}
				c.logFor(c.setupCtx).Warn().Strs("livekit_owner_identities", owners).Strs("owner_membership_keys", devices).
					Msg("Owner absent from LiveKit for the grace period")
				c.teardown(byOwner, reasonRTCLost, false)
			}
		case <-timerC(c.joinTimer):
			c.joinTimer = nil
			if c.joinInFlight && c.joinCancel != nil {
				c.joinCancel() // the join goroutine reports the cancellation as an error result
			}
		case <-tickerC(c.statsTicker):
			c.onStatsTick()
		case <-timerC(c.refreshTimer):
			c.refreshTimer = nil
			c.onRefresh()
		case <-timerC(c.stickyTimer):
			c.stickyTimer = nil
			c.onStickyRefresh()
		}
	}
}

// onEvent dispatches an input from c.events (or the shutdown signal).
func (c *call) onEvent(ev event) {
	switch ev.kind {
	case evOwnerMembership:
		if c.isOutbound() {
			c.onOutMembership(ev)
		} else {
			c.onMembership(ev)
		}
	case evDecline:
		if c.isOutbound() {
			c.onOutDecline(ev)
		} else {
			c.onDecline(ev)
		}
	case evShutdown:
		if c.isOutbound() {
			c.onOutShutdown()
		} else {
			c.onShutdown()
		}
	case evHangupCmd:
		if c.isOutbound() {
			c.onOutHangupCmd()
		} else {
			c.onHangupCmd()
		}
	case evDTMF:
		c.onDTMF(ev)
	}
}

// onHangupCmd handles Manager.Hangup for an inbound call (PHASE3-SPEC §6.5, inbound rows).
func (c *call) onHangupCmd() {
	switch c.state {
	case stRinging:
		c.logFor(c.setupCtx).Info().Msg("Owner declined the call by command")
		c.endRing(trace.SpanContext{}, outcomeDeclined)
		c.reject(603, "Decline")
		c.fail(failSpec{outcome: outcomeDeclined, reason: reasonDecline, notice: noticeDeclined})
	case stAnswering:
		c.logFor(c.setupCtx).Info().Msg("Owner hung up by command before the call was connected")
		c.sipHangup()
		c.fail(failSpec{outcome: outcomeMissed, reason: reasonCancel, notice: noticeMissed})
	case stActive:
		c.logFor(c.setupCtx).Info().Msg("Owner hung up by command")
		c.teardown(byOwner, reasonBye, false)
	}
}

// onDTMF answers a Manager.SendDTMF request: only an active call with a media bridge sends digits.
// The digits are never logged.
func (c *call) onDTMF(ev event) {
	var err error
	if c.state != stActive || c.bridge == nil {
		err = ErrNotConnected
	} else {
		err = c.bridge.SendDTMF(ev.digits)
		if err == nil {
			c.logFor(c.setupCtx).Info().Int("digit_count", len(ev.digits)).Msg("DTMF queued")
		}
	}
	if ev.reply != nil {
		select {
		case ev.reply <- err:
		default:
		}
	}
}

func (c *call) onDecline(ev event) {
	if c.state != stRinging {
		return // answered elsewhere (Answering) or irrelevant
	}
	target, ok := matrixrtc.DeclineTarget(ev.rtc)
	if !ok || target != c.ringEventID || ev.rtc.Sender != c.owner {
		c.logFor(c.setupCtx).Debug().Stringer("sender", ev.rtc.Sender).Msg("Ignoring decline not from the owner for this ring")
		return
	}
	c.logFor(c.setupCtx).Info().Msg("Owner declined the call")
	c.endRing(ev.from, outcomeDeclined)
	c.reject(603, "Decline")
	c.fail(failSpec{outcome: outcomeDeclined, reason: reasonDecline, notice: noticeDeclined})
}

func (c *call) onShutdown() {
	switch c.state {
	case stRinging:
		c.endRing(trace.SpanContext{}, outcomeFailed)
		c.reject(480, "Temporarily Unavailable")
		c.fail(failSpec{outcome: outcomeFailed, reason: reasonBridgeStop, notice: noticeMissed})
	case stAnswering:
		c.sipHangup()
		c.fail(failSpec{outcome: outcomeFailed, reason: reasonBridgeStop, notice: noticeMissed})
	case stActive:
		c.teardown(byBridge, reasonBridgeStop, false)
	}
}

func (c *call) onRingTimeout() {
	if c.isOutbound() {
		c.onOutRingTimeout()
		return
	}
	if c.state != stRinging {
		return
	}
	c.logFor(c.setupCtx).Info().Msg("Ring timeout")
	c.endRing(trace.SpanContext{}, outcomeTimeout)
	c.reject(480, "Temporarily Unavailable")
	c.fail(failSpec{outcome: outcomeTimeout, reason: reasonRingTimeout, notice: noticeMissed})
}

func (c *call) onAnswerTimeout() {
	if c.state != stAnswering {
		return
	}
	c.logFor(c.setupCtx).Warn().Bool("rtc_ready", c.session != nil).Bool("answer_issued", c.answerIssued).
		Msg("Call was not connected in time after the owner answered")
	c.sipHangup()
	c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeNoAudio})
}

// onSIP handles an event from the SIP call.
func (c *call) onSIP(ev sipua.CallEvent) {
	if c.isOutbound() && c.state != stActive {
		c.onOutSIP(ev)
		return
	}
	switch ev.Kind {
	case sipua.EventEnded:
		c.recordSIPEnd(ev)
		switch c.state {
		case stRinging, stAnswering:
			if ev.Cause == sipua.EndCancelled {
				c.logFor(c.setupCtx).Info().Msg("Caller cancelled the call")
				c.spanEvent(c.setupSpan, "sip.cancel.received", ev.At)
				c.endRing(trace.SpanContext{}, outcomeCancelled)
				c.fail(failSpec{outcome: outcomeCancelled, reason: reasonCancel, notice: noticeMissed})
				return
			}
			c.logFor(c.setupCtx).Warn().Stringer("cause", ev.Cause).Int("status", ev.StatusCode).
				Str("error", scrubErr(ev.Err)).Msg("SIP call ended before it was connected")
			notice := noticeMissed
			if c.state == stAnswering {
				notice = noticeNoAudio
			}
			c.endRing(trace.SpanContext{}, outcomeFailed)
			c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: notice})
		case stActive:
			if ev.Cause == sipua.EndRemoteBye {
				c.teardown(byRemote, reasonBye, true)
			} else {
				c.teardown(byBridge, reasonError, false)
			}
		}
	case sipua.EventAnswered:
		if c.state != stAnswering {
			return
		}
		c.activate(ev)
		if c.state == stActive && len(c.devices) == 0 {
			c.logFor(c.setupCtx).Info().Msg("Owner left before the call was connected")
			c.teardown(byOwner, reasonBye, false)
		}
	case sipua.EventAnswerFailed:
		if c.state != stAnswering {
			return
		}
		c.logFor(c.setupCtx).Warn().Str("error", scrubErr(ev.Err)).Msg("SIP answer failed")
		c.answerFailed = true
		if !c.pollSIPEnded() {
			c.sipHangup() // the dialog is still up: end it (480, or BYE if 200 raced through)
		}
		c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeNoAudio})
	}
}

func (c *call) recordSIPEnd(ev sipua.CallEvent) {
	c.sipEnded = true
	c.sipStatus = ev.StatusCode
	c.sipCause = ev.Cause
	c.sipReason = ev.Reason
}

// onRTC handles an event from the LiveKit session.
func (c *call) onRTC(ev rtc.Event) {
	switch ev.Kind {
	case rtc.EventOwnerJoined:
		c.lkOwners[ev.Identity] = struct{}{}
		if c.graceTimer != nil {
			c.graceTimer.Stop()
			c.graceTimer = nil
		}
	case rtc.EventOwnerLeft:
		delete(c.lkOwners, ev.Identity)
		armed := c.state == stActive || c.state == stDialing || c.state == stEarly
		if armed && len(c.lkOwners) == 0 && c.graceTimer == nil {
			c.graceTimer = c.cfg.Clock.NewTimer(ownerRTCGrace)
		}
	case rtc.EventDisconnected:
		c.logFor(c.setupCtx).Warn().Str("reason", ev.Reason).Stringer("state", c.state).Msg("LiveKit session disconnected")
		if c.isOutbound() && c.state != stActive {
			c.onOutDisconnected()
			return
		}
		switch c.state {
		case stRinging, stAnswering:
			if c.state == stAnswering && c.answerIssued {
				// 200 OK may already be on its way with this session's audio: give up.
				c.sipHangup()
				c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeNoAudio})
				return
			}
			c.dropSession()
			if c.canRetryJoin() {
				c.startJoin()
				return
			}
			c.endRing(trace.SpanContext{}, outcomeFailed)
			c.reject(480, "Temporarily Unavailable")
			c.fail(failSpec{outcome: outcomeFailed, reason: reasonError, notice: noticeNoAudio})
		case stActive:
			c.teardown(byBridge, reasonRTCLost, false)
		}
	}
}

func (c *call) onBridgeDone() {
	if c.state == stDialing || c.state == stEarly {
		c.onEarlyBridgeDone()
		return
	}
	if c.state != stActive {
		return
	}
	reason := reasonError
	err := c.bridge.Err()
	if errors.Is(err, media.ErrRTPTimeout) {
		reason = reasonRTPTimeout
	}
	if errors.Is(err, io.EOF) {
		// A remote BYE makes diago close the media at about the same time sipua reports
		// EventEnded{EndRemoteBye}; give the SIP event a moment so the teardown is labelled correctly.
		c.waitSIPEndFor(byeRaceWindow)
		if c.sipEnded && c.sipCause == sipua.EndRemoteBye {
			c.teardown(byRemote, reasonBye, true)
			return
		}
	}
	c.logFor(c.setupCtx).Warn().Str("error", scrubErr(err)).Msg("Media bridge stopped")
	c.teardown(byBridge, reason, false)
}

// reject sends a final non-2xx once (inbound only; ignored after any final response or the end of the
// dialog).
func (c *call) reject(code int, reason string) {
	if c.in == nil || c.finalIssued || c.sipEnded {
		return
	}
	c.finalIssued = true
	c.in.Reject(code, reason)
}

// sipHangup issues sipua Hangup once before the dialog ended. Inbound: 480 before answer, BYE after.
// Outbound: CANCEL before a final response, BYE after answer.
func (c *call) sipHangup() {
	if c.finalIssued || c.sipEnded {
		return
	}
	switch {
	case c.in != nil:
		c.finalIssued = true
		c.in.Hangup()
	case c.out != nil:
		c.finalIssued = true
		c.out.Hangup()
	}
}

// canRetryJoin reports whether another (non-fallback) join attempt is allowed.
func (c *call) canRetryJoin() bool { return c.joinAttempt-c.joinFallbacks < maxJoinAttempts }

// ringSlot is the slot_id of the ring notification.
func (c *call) ringSlot() string {
	if c.plan.ringSlot != "" {
		return c.plan.ringSlot
	}
	return c.plan.slot
}

// stopTimers stops every state-machine timer and ticker.
func (c *call) stopTimers() {
	c.cancelKeeper() // the keeper is waited for (and its delayed leave fired) in leaveMembership
	for _, t := range []*clock.Timer{&c.ringTimer, &c.answerTimer, &c.graceTimer, &c.joinTimer, &c.refreshTimer, &c.stickyTimer, &c.dialTimer} {
		if *t != nil {
			(*t).Stop()
			*t = nil
		}
	}
	for _, t := range []*clock.Ticker{&c.keepalive, &c.statsTicker} {
		if *t != nil {
			(*t).Stop()
			*t = nil
		}
	}
}

// waitSIPEnd waits (bounded) for the SIP call's EventEnded; its StatusCode is the authoritative final
// status.
func (c *call) waitSIPEnd() { c.waitSIPEndFor(teardownStepTimeout) }

// waitSIPEndFor is waitSIPEnd with an explicit bound.
func (c *call) waitSIPEndFor(d time.Duration) {
	if c.sipEnded || c.sipEvents == nil {
		return
	}
	t := c.cfg.Clock.NewTimer(d)
	defer t.Stop()
	for !c.sipEnded {
		select {
		case ev, ok := <-c.sipEvents:
			if !ok {
				c.sipEvents = nil
				return
			}
			if ev.Kind == sipua.EventEnded {
				c.recordSIPEnd(ev)
			}
		case <-t.C():
			c.logFor(c.setupCtx).Debug().Dur("waited", d).Msg("Timed out waiting for the SIP call to end")
			return
		}
	}
}

func scrubErr(err error) string {
	if err == nil {
		return ""
	}
	return obs.Scrub(err.Error())
}
