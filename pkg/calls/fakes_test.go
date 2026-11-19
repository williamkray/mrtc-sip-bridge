package calls

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

const (
	testOwner      = id.UserID("@owner:example.org")
	testGhost      = id.UserID("@sip_15551234567:example.org")
	testRoom       = id.RoomID("!room:example.org")
	testLogin      = core.LoginRef("+15552220001@sip.example.com")
	testLocal      = "+15552220001"
	testServiceURL = "https://lk.example.org/livekit/jwt"
)

var testStart = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// opLog records host/ghost operations in order (shared across fakes).
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) add(s string) {
	l.mu.Lock()
	l.ops = append(l.ops, s)
	l.mu.Unlock()
}

func (l *opLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ops...)
}

// index returns the position of the first op equal to s, or -1.
func (l *opLog) index(s string) int {
	for i, o := range l.list() {
		if o == s {
			return i
		}
	}
	return -1
}

func (l *opLog) count(s string) int {
	n := 0
	for _, o := range l.list() {
		if o == s {
			n++
		}
	}
	return n
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) snapshot() *bytes.Buffer {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.NewBuffer(append([]byte(nil), b.buf.Bytes()...))
}

func (b *syncBuffer) String() string { return b.snapshot().String() }

// ---- SIP ----

type fakeSIPLeg struct {
	closed chan struct{}
	once   sync.Once
}

func newFakeSIPLeg() *fakeSIPLeg { return &fakeSIPLeg{closed: make(chan struct{})} }

func (l *fakeSIPLeg) ReadRTP(*media.RTPPacket) error {
	<-l.closed
	return io.EOF
}
func (l *fakeSIPLeg) WriteRTP([]byte, uint32, bool) error { return nil }
func (l *fakeSIPLeg) close()                              { l.once.Do(func() { close(l.closed) }) }

// closableLeg is a SIP media leg the fake SIP call closes when the dialog ends.
type closableLeg interface {
	media.SIPLeg
	close()
}

// fakeSIPCall implements sipua.InboundCall. Commands are recorded in order; Reject/Hangup emit
// EventEnded like sipua does; Answer auto-answers when autoAnswer is set.
type fakeSIPCall struct {
	clk        *clock.Fake
	info       sipua.InviteInfo
	ev         chan sipua.CallEvent
	leg        closableLeg
	autoAnswer bool

	mu       sync.Mutex
	cmds     []string
	answered bool
	ended    bool
}

func (f *fakeSIPCall) Info() sipua.InviteInfo { return f.info }

func (f *fakeSIPCall) Events() <-chan sipua.CallEvent { return f.ev }

func (f *fakeSIPCall) record(s string) {
	f.cmds = append(f.cmds, s)
}

func (f *fakeSIPCall) Ringing() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ringing")
}

func (f *fakeSIPCall) Answer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("answer")
	if f.autoAnswer && !f.ended && !f.answered {
		f.answerLocked()
	}
}

func (f *fakeSIPCall) Reject(code int, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("reject:" + strconv.Itoa(code))
	if !f.answered && !f.ended {
		f.endLocked(sipua.EndRejected, code, nil)
	}
}

func (f *fakeSIPCall) Hangup() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("hangup")
	if f.ended {
		return
	}
	if f.answered {
		f.endLocked(sipua.EndLocalHangup, 200, nil)
	} else {
		f.endLocked(sipua.EndRejected, 480, nil)
	}
}

func (f *fakeSIPCall) answerLocked() {
	f.answered = true
	now := f.clk.Now()
	f.ev <- sipua.CallEvent{Kind: sipua.EventAnswered, At: now, Media: f.leg, Answer: sipua.AnswerInfo{
		Codec: "PCMU", PtimeMS: 20, TelephoneEvent: true,
		LocalRTP: "192.0.2.1:20000", RemoteRTP: "203.0.113.5:4000",
		OKSentAt: now.Add(-50 * time.Millisecond), AckAt: now,
	}}
}

func (f *fakeSIPCall) endLocked(cause sipua.EndCause, code int, err error) {
	f.ended = true
	f.ev <- sipua.CallEvent{Kind: sipua.EventEnded, At: f.clk.Now(), Cause: cause, StatusCode: code, Err: err}
	close(f.ev)
	f.leg.close()
}

// answer emits EventAnswered (test-driven).
func (f *fakeSIPCall) answer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended && !f.answered {
		f.answerLocked()
	}
}

func (f *fakeSIPCall) cancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended {
		f.endLocked(sipua.EndCancelled, 487, nil)
	}
}

func (f *fakeSIPCall) bye() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended {
		f.endLocked(sipua.EndRemoteBye, 200, nil)
	}
}

// answerFailCancelled emits EventAnswerFailed followed by EventEnded{EndCancelled} (CANCEL during
// answer, as sipua does).
func (f *fakeSIPCall) answerFailCancelled() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ev <- sipua.CallEvent{Kind: sipua.EventAnswerFailed, At: f.clk.Now(), Err: errors.New("transaction cancelled")}
	f.endLocked(sipua.EndCancelled, 487, nil)
}

func (f *fakeSIPCall) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeSIPCall) count(cmd string) int {
	n := 0
	for _, c := range f.commands() {
		if c == cmd {
			n++
		}
	}
	return n
}

func (f *fakeSIPCall) has(prefix string) bool {
	for _, c := range f.commands() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// ---- RTC ----

type fakeRTCLeg struct {
	mu   sync.Mutex
	recv media.PacketReceiver
}

func (l *fakeRTCLeg) WriteOpus([]byte, time.Duration) error { return nil }
func (l *fakeRTCLeg) SetReceiver(r media.PacketReceiver) {
	l.mu.Lock()
	l.recv = r
	l.mu.Unlock()
}

type fakeSession struct {
	ev    chan rtc.Event
	audio media.RTCLeg

	mu     sync.Mutex
	leaves int
	closed bool
	owners []string

	// Owner matching like rtc.session: remote participants present in the LiveKit room, the owner's
	// legacy identity prefix and the exact identities accepted so far. matched lists the identities that
	// were announced (EventOwnerJoined) and whose audio would be subscribed; nothing else is.
	present []string
	prefix  string
	exact   map[string]bool
	matched []string
}

func newFakeSession(audio media.RTCLeg) *fakeSession {
	if audio == nil {
		audio = &fakeRTCLeg{}
	}
	return &fakeSession{ev: make(chan rtc.Event, 32), audio: audio}
}

func (s *fakeSession) Identity() string      { return string(testGhost) + ":DEV" }
func (s *fakeSession) LiveKitRoom() string   { return "lkroom" }
func (s *fakeSession) ServerAddress() string { return "livekit.example.org" }
func (s *fakeSession) AddOwnerIdentities(ids ...string) {
	s.mu.Lock()
	s.owners = append(s.owners, ids...)
	for _, i := range ids {
		if s.exact == nil {
			s.exact = map[string]bool{}
		}
		s.exact[i] = true
	}
	s.rescanLocked()
	s.mu.Unlock()
}

// setupOwners gives the session the owner matcher inputs of rtc.JoinParams and the participants that
// are already in the room, and announces those that match.
func (s *fakeSession) setupOwners(p rtc.JoinParams, present []string) {
	s.mu.Lock()
	s.present = present
	s.prefix = string(p.Owner) + ":"
	s.exact = map[string]bool{}
	for _, i := range p.Owners {
		s.exact[i] = true
	}
	s.rescanLocked()
	s.mu.Unlock()
}

func (s *fakeSession) rescanLocked() {
	for _, id := range s.present {
		if !s.exact[id] && !(s.prefix != ":" && strings.HasPrefix(id, s.prefix)) {
			continue
		}
		already := false
		for _, m := range s.matched {
			if m == id {
				already = true
			}
		}
		if already || s.closed {
			continue
		}
		s.matched = append(s.matched, id)
		select {
		case s.ev <- rtc.Event{Kind: rtc.EventOwnerJoined, Identity: id}:
		default:
		}
	}
}

// matchedOwners returns the identities announced as owner so far.
func (s *fakeSession) matchedOwners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.matched...)
}

// ownerIdentities returns every identity passed to AddOwnerIdentities, in order.
func (s *fakeSession) ownerIdentities() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.owners...)
}
func (s *fakeSession) Audio() media.RTCLeg { return s.audio }
func (s *fakeSession) Events() <-chan rtc.Event {
	return s.ev
}

func (s *fakeSession) Leave(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaves++
	if !s.closed {
		s.closed = true
		close(s.ev)
	}
	return nil
}

func (s *fakeSession) push(e rtc.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.ev <- e
	}
}

func (s *fakeSession) leaveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaves
}

// joinStep scripts one Join attempt: ok (zero value), err, block until ctx, or wait for gate.
type joinStep struct {
	err       error
	block     bool
	gate      chan struct{} // released by the test; respects ctx unless ignoreCtx
	ignoreCtx bool
}

type fakeJoiner struct {
	audio media.RTCLeg // nil = fakeRTCLeg per session
	ops   *opLog

	present []string // remote LiveKit participants already in the room (fakeSession owner matching)

	mu       sync.Mutex
	steps    []joinStep
	calls    []rtc.JoinParams
	sessions []*fakeSession
}

func (j *fakeJoiner) Join(ctx context.Context, p rtc.JoinParams) (rtc.Session, error) {
	if j.ops != nil {
		j.ops.add("join")
	}
	j.mu.Lock()
	n := len(j.calls)
	j.calls = append(j.calls, p)
	var step joinStep
	if n < len(j.steps) {
		step = j.steps[n]
	}
	j.mu.Unlock()
	if step.gate != nil {
		if step.ignoreCtx {
			<-step.gate
		} else {
			select {
			case <-step.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if step.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if step.err != nil {
		return nil, step.err
	}
	s := newFakeSession(j.audio)
	if len(j.present) > 0 {
		s.setupOwners(p, j.present)
	}
	j.mu.Lock()
	j.sessions = append(j.sessions, s)
	j.mu.Unlock()
	return s, nil
}

func (j *fakeJoiner) attempts() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.calls)
}

func (j *fakeJoiner) lastSession() *fakeSession {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.sessions) == 0 {
		return nil
	}
	return j.sessions[len(j.sessions)-1]
}

func (j *fakeJoiner) session(i int) *fakeSession {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i >= len(j.sessions) {
		return nil
	}
	return j.sessions[i]
}

// ---- Matrix host ----

type stateCall struct {
	typ     mevent.Type
	key     string
	content map[string]any
}

type msgCall struct {
	typ     mevent.Type
	content map[string]any
}

type fakeGhost struct {
	ops *opLog

	mu        sync.Mutex
	states    []stateCall
	msgs      []msgCall
	redacts   []id.EventID
	stateErr  error // returned for join content
	msgErr    error
	stateGate chan struct{} // blocks join SendState until closed
}

func (g *fakeGhost) UserID() id.UserID { return testGhost }

func (g *fakeGhost) SendState(ctx context.Context, _ id.RoomID, typ mevent.Type, key string, content map[string]any) (id.EventID, error) {
	g.mu.Lock()
	gate := g.stateGate
	g.mu.Unlock()
	if gate != nil && len(content) > 0 {
		<-gate
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.states = append(g.states, stateCall{typ, key, content})
	if len(content) == 0 {
		g.ops.add("clear")
		return id.EventID("$leave" + strconv.Itoa(len(g.states))), nil
	}
	g.ops.add("publish")
	if g.stateErr != nil {
		return "", g.stateErr
	}
	return id.EventID("$member" + strconv.Itoa(len(g.states))), nil
}

func (g *fakeGhost) SendMessage(_ context.Context, _ id.RoomID, typ mevent.Type, content map[string]any) (id.EventID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.msgs = append(g.msgs, msgCall{typ, content})
	if typ == matrixrtc.RTCDecline {
		g.ops.add("decline")
		return id.EventID("$decline" + strconv.Itoa(len(g.msgs))), nil
	}
	g.ops.add("ring")
	if g.msgErr != nil {
		return "", g.msgErr
	}
	return id.EventID("$ring" + strconv.Itoa(len(g.msgs))), nil
}

func (g *fakeGhost) Redact(_ context.Context, _ id.RoomID, evID id.EventID, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.redacts = append(g.redacts, evID)
	g.ops.add("redact")
	return nil
}

func (g *fakeGhost) RequestOpenIDToken(context.Context) (*core.OpenIDToken, error) {
	return &core.OpenIDToken{AccessToken: "SECRETTOKEN"}, nil
}

func (g *fakeGhost) stateCalls() []stateCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]stateCall(nil), g.states...)
}

func (g *fakeGhost) msgCalls() []msgCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]msgCall(nil), g.msgs...)
}

type fakeHost struct {
	ghost *fakeGhost
	ops   *opLog

	mu          sync.Mutex
	owner       id.UserID
	encrypted   bool
	created     bool   // ResolveCallRoom reports a new room
	remote      string // LookupCallRoom RemoteE164 ["+15552345678"]
	lookupErr   error  // LookupCallRoom error
	lookupGate  chan struct{}
	lookups     []id.RoomID
	resolveErr  error
	resolveGate chan struct{}
	readErr     error
	resolves    []string
	notices     []string
	reads       []id.EventID
	readHook    func() // called inside MarkReadAsOwner
	noticeGate  chan struct{}

	// ghostOverride, when set, is the ghost of every call room (e.g. a *fakeRTCGhost).
	ghostOverride core.GhostIntent
}

func (h *fakeHost) ghostIntent() core.GhostIntent {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ghostOverride != nil {
		return h.ghostOverride
	}
	return h.ghost
}

func (h *fakeHost) ResolveCallRoom(_ context.Context, _ core.LoginRef, e164 string) (*core.CallRoom, error) {
	h.mu.Lock()
	h.resolves = append(h.resolves, e164)
	gate, err, enc := h.resolveGate, h.resolveErr, h.encrypted
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	created := h.created
	h.mu.Unlock()
	return &core.CallRoom{RoomID: testRoom, RemoteE164: e164, Ghost: h.ghostIntent(), Encrypted: enc, Created: created}, nil
}

// LookupCallRoom knows testRoom and testRoom2 (same ghost); anything else is not a call room.
func (h *fakeHost) LookupCallRoom(_ context.Context, _ core.LoginRef, roomID id.RoomID) (*core.CallRoom, error) {
	h.mu.Lock()
	h.lookups = append(h.lookups, roomID)
	err, enc, remote, gate := h.lookupErr, h.encrypted, h.remote, h.lookupGate
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	h.ops.add("lookup")
	if err != nil {
		return nil, err
	}
	if roomID != testRoom && roomID != testRoom2 {
		return nil, core.ErrNotCallRoom
	}
	if remote == "" {
		remote = "+15552345678"
	}
	return &core.CallRoom{RoomID: roomID, RemoteE164: remote, Ghost: h.ghostIntent(), Encrypted: enc}, nil
}

func (h *fakeHost) lookupList() []id.RoomID {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]id.RoomID(nil), h.lookups...)
}

func (h *fakeHost) SendCallNotice(_ context.Context, _ *core.CallRoom, text string) error {
	h.mu.Lock()
	gate := h.noticeGate
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	h.mu.Lock()
	h.notices = append(h.notices, text)
	h.mu.Unlock()
	h.ops.add("notice")
	return nil
}

func (h *fakeHost) OwnerMXID(core.LoginRef) id.UserID {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.owner
}

func (h *fakeHost) MarkReadAsOwner(_ context.Context, _ core.LoginRef, _ id.RoomID, evID id.EventID) error {
	h.mu.Lock()
	h.reads = append(h.reads, evID)
	err, hook := h.readErr, h.readHook
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	h.ops.add("read")
	return err
}

func (h *fakeHost) noticeList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.notices...)
}

func (h *fakeHost) resolveList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.resolves...)
}

func (h *fakeHost) readList() []id.EventID {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]id.EventID(nil), h.reads...)
}

// ---- media ----

type fakeBridge struct {
	sip media.SIPLeg
	rtc media.RTCLeg
	cfg media.Config
	ops *opLog

	mu       sync.Mutex
	stats    media.Stats
	done     chan struct{}
	closed   bool
	err      error
	stops    int
	connects int
	dtmf     []string
	dtmfErr  error
}

func (b *fakeBridge) Stats() media.Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

func (b *fakeBridge) Connect() {
	b.mu.Lock()
	b.connects++
	b.mu.Unlock()
	if b.ops != nil {
		b.ops.add("connect")
	}
}

func (b *fakeBridge) SendDTMF(digits string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dtmfErr != nil {
		return b.dtmfErr
	}
	b.dtmf = append(b.dtmf, digits)
	return nil
}

func (b *fakeBridge) setDTMFErr(err error) {
	b.mu.Lock()
	b.dtmfErr = err
	b.mu.Unlock()
}

func (b *fakeBridge) connectCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects
}

func (b *fakeBridge) dtmfList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.dtmf...)
}

func (b *fakeBridge) Done() <-chan struct{} { return b.done }

func (b *fakeBridge) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *fakeBridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stops++
	if !b.closed {
		b.closed = true
		close(b.done)
	}
}

func (b *fakeBridge) finish(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
	if !b.closed {
		b.closed = true
		close(b.done)
	}
}

func (b *fakeBridge) setStats(s media.Stats) {
	b.mu.Lock()
	b.stats = s
	b.mu.Unlock()
}

func (b *fakeBridge) stopCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stops
}

type fakeMedia struct {
	ops *opLog

	mu      sync.Mutex
	bridges []*fakeBridge
	err     error
}

func (m *fakeMedia) start(cfg media.Config, sip media.SIPLeg, r media.RTCLeg) (media.Bridge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ops != nil {
		if cfg.Early {
			m.ops.add("media:early")
		} else {
			m.ops.add("media")
		}
	}
	if m.err != nil {
		return nil, m.err
	}
	b := &fakeBridge{sip: sip, rtc: r, cfg: cfg, ops: m.ops, done: make(chan struct{})}
	m.bridges = append(m.bridges, b)
	return b, nil
}

func (m *fakeMedia) bridge(i int) *fakeBridge {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.bridges) {
		return nil
	}
	return m.bridges[i]
}

// ---- harness ----

type harness struct {
	t      *testing.T
	env    *obstest.Env
	clk    *clock.Fake
	ops    *opLog
	ghost  *fakeGhost
	host   *fakeHost
	joiner *fakeJoiner
	media  *fakeMedia
	dialer *fakeDialer
	tones  *fakeTones
	logs   *syncBuffer
	m      *Manager
	cfg    Config
	ingest *core.Ingest

	serviceErr error
}

type harnessOpt func(*harness)

func withRingTimeout(d time.Duration) harnessOpt { return func(h *harness) { h.cfg.RingTimeout = d } }
func withJoinSteps(steps ...joinStep) harnessOpt {
	return func(h *harness) { h.joiner.steps = steps }
}
func withServiceErr(err error) harnessOpt { return func(h *harness) { h.serviceErr = err } }

// withOutbound enables outbound calls (fake Dial, LoginNumber = testLocal, prefixes ["+1"]) with the
// given trigger mode ("" = default ring).
func withOutbound(mode string) harnessOpt {
	return func(h *harness) {
		h.cfg.Dial = h.dialer.dial
		h.cfg.LoginNumber = func(core.LoginRef) string { return testLocal }
		h.cfg.DialAllowedPrefixes = []string{"+1"}
		h.cfg.OutboundTrigger = mode
	}
}

func withConfig(f func(*Config)) harnessOpt { return func(h *harness) { f(&h.cfg) } }

// newHarness must run inside a synctest bubble.
func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		env:    obstest.Setup(t),
		clk:    clock.NewFake(testStart),
		ops:    &opLog{},
		joiner: &fakeJoiner{},
		media:  &fakeMedia{},
		logs:   &syncBuffer{},
	}
	h.joiner.ops = h.ops
	h.media.ops = h.ops
	h.dialer = &fakeDialer{clk: h.clk, ops: h.ops}
	h.tones = &fakeTones{ops: h.ops}
	h.ghost = &fakeGhost{ops: h.ops}
	h.host = &fakeHost{ghost: h.ghost, ops: h.ops, owner: testOwner}
	h.cfg = Config{
		Host:   h.host,
		Joiner: h.joiner,
		ServiceURL: func(context.Context) (string, error) {
			if h.serviceErr != nil {
				return "", h.serviceErr
			}
			return testServiceURL, nil
		},
		RingTimeout:        30 * time.Second,
		DefaultCountryCode: "1",
		Clock:              h.clk,
		Log:                zerolog.New(h.logs),
		StartMedia:         h.media.start,
	}
	for _, o := range opts {
		o(h)
	}
	h.m = NewManager(h.cfg)
	h.m.startTone = h.tones.start
	h.ingest = core.NewIngest(h.m)
	t.Cleanup(func() {
		// Every test must leave no call behind (goroutine leaks are caught by synctest too).
		h.m.Stop(context.Background())
	})
	return h
}

func defaultInfo(clk *clock.Fake) sipua.InviteInfo {
	now := clk.Now()
	return sipua.InviteInfo{
		CallID:       "a84b4c76e66710@pbx.example.net",
		FromUser:     "+15552345678",
		ToUser:       "15552220001",
		RemoteAddr:   "198.51.100.7:5060",
		ReceivedAt:   now,
		TryingSentAt: now,
		Offer: sipua.Offer{HasSDP: true, PCMU: true, TelephoneEvent: true, MediaAddr: "203.0.113.5:4000",
			Codecs: []string{"PCMU/0", "telephone-event/101"}},
	}
}

func (h *harness) newSIPCall(mods ...func(*sipua.InviteInfo)) *fakeSIPCall {
	info := defaultInfo(h.clk)
	for _, m := range mods {
		m(&info)
	}
	return &fakeSIPCall{clk: h.clk, info: info, ev: make(chan sipua.CallEvent, 8), leg: newFakeSIPLeg(), autoAnswer: true}
}

// invite hands a new fake SIP call to the manager and waits until the actor is idle.
func (h *harness) invite(mods ...func(*sipua.InviteInfo)) *fakeSIPCall {
	sc := h.newSIPCall(mods...)
	h.m.HandleInvite(testLogin, testLocal, sc)
	synctest.Wait()
	return sc
}

// matrix feeds evt through a real core.Ingest (so the matrixrtc.ingest span exists) and waits.
func (h *harness) matrix(evt *mevent.Event) {
	h.ingest.HandleMatrixRTCEvent(context.Background(), testLogin, evt)
	synctest.Wait()
}

func (h *harness) advance(d time.Duration) {
	h.clk.Advance(d)
	synctest.Wait()
}

func (h *harness) ownerMember(device string, live bool) *mevent.Event {
	return memberEvent(testOwner, device, live, h.clk.Now(), testRoom)
}

func memberEvent(user id.UserID, device string, live bool, now time.Time, room id.RoomID) *mevent.Event {
	sk := matrixrtc.MemberStateKey(user, device)
	var raw []byte
	if live {
		raw = []byte(`{"application":"m.call","call_id":"","scope":"m.room","device_id":"` + device +
			`","created_ts":` + strconv.FormatInt(now.UnixMilli(), 10) +
			`,"expires":14400000,"focus_active":{"type":"livekit","focus_selection":"oldest_membership"},` +
			`"foci_preferred":[{"type":"livekit","livekit_service_url":"https://lk.example.org/livekit/jwt","livekit_alias":"` +
			string(room) + `"}],"m.call.intent":"audio"}`)
	} else {
		raw = []byte(`{}`)
	}
	return &mevent.Event{
		Type:      matrixrtc.CallMember,
		RoomID:    room,
		ID:        id.EventID("$m-" + device + strconv.FormatInt(now.UnixNano(), 10)),
		Sender:    user,
		StateKey:  &sk,
		Timestamp: now.UnixMilli(),
		Content:   mevent.Content{VeryRaw: raw},
	}
}

func declineEvent(sender id.UserID, target id.EventID, room id.RoomID) *mevent.Event {
	return &mevent.Event{
		Type:    matrixrtc.RTCDecline,
		RoomID:  room,
		ID:      "$decline",
		Sender:  sender,
		Content: mevent.Content{VeryRaw: []byte(`{"m.relates_to":{"rel_type":"m.reference","event_id":"` + string(target) + `"}}`)},
	}
}

// ringID returns the event id of the (first) ring.
func (h *harness) ringID() id.EventID { return "$ring1" }

func (h *harness) callCount() int {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return len(h.m.calls)
}

// assertEnded checks the final metrics of exactly one ended call with outcome (and status class when
// non-empty), call.active back to 0 and no call left in the manager.
func (h *harness) assertEnded(outcome, class string) {
	h.t.Helper()
	synctest.Wait()
	if n := h.callCount(); n != 0 {
		h.t.Errorf("manager still has %d calls", n)
	}
	want := []string{"call.outcome", outcome}
	if class != "" {
		want = append(want, "sip.response.status_class", class)
	}
	if got := h.env.Sum(h.t, "mrtcsip.call.ended", want...); got != 1 {
		h.t.Errorf("call.ended%v = %v, want 1 (all points: %+v)", want, got, h.env.MetricPoints(h.t, "mrtcsip.call.ended"))
	}
	if got := h.env.Sum(h.t, "mrtcsip.call.active"); got != 0 {
		h.t.Errorf("call.active = %v, want 0", got)
	}
}

// teardownAttrs returns hangup.by and end_reason of the single call.teardown span.
func (h *harness) teardownAttrs() (string, string) {
	h.t.Helper()
	spans := h.env.SpansByName("call.teardown")
	if len(spans) != 1 {
		h.t.Fatalf("got %d call.teardown spans, want 1", len(spans))
	}
	return obstest.Attr(spans[0], "call.hangup.by").AsString(), obstest.Attr(spans[0], "call.end_reason").AsString()
}

// setupAttr returns attribute key of the call.inbound span.
func (h *harness) setupAttr(key string) string {
	h.t.Helper()
	s := h.env.SpanByName("call.inbound")
	if s == nil {
		h.t.Fatal("no call.inbound span")
	}
	return obstest.Attr(s, key).Emit()
}

// answer drives a ringing call to Active: owner membership, auto-answer, and an owner LiveKit
// participant (so the grace timer is not armed).
func (h *harness) answer(sc *fakeSIPCall, device string) *fakeSession {
	h.t.Helper()
	h.matrix(h.ownerMember(device, true))
	s := h.joiner.lastSession()
	if s == nil {
		h.t.Fatal("no LiveKit session")
	}
	s.push(rtc.Event{Kind: rtc.EventOwnerJoined, Identity: string(testOwner) + ":" + device})
	synctest.Wait()
	if !sc.has("answer") {
		h.t.Fatalf("SIP answer not issued: %v", sc.commands())
	}
	if h.media.bridge(0) == nil {
		h.t.Fatal("media bridge not started")
	}
	return s
}

// ---- outbound ----

const testRoom2 = id.RoomID("!room2:example.org")

// fakeOutCall implements sipua.OutboundCall. Hangup before answer ends the call with EndLocalCancel and
// cancelStatus (487 by default; 200 = the CANCEL/200 race; -1 = never, the test ends it).
type fakeOutCall struct {
	clk          *clock.Fake
	info         sipua.DialInfo
	ev           chan sipua.CallEvent
	leg          closableLeg
	cancelStatus int

	mu       sync.Mutex
	hangups  int
	answered bool
	ended    bool
}

func (f *fakeOutCall) Info() sipua.DialInfo           { return f.info }
func (f *fakeOutCall) Events() <-chan sipua.CallEvent { return f.ev }

func (f *fakeOutCall) Hangup() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangups++
	switch {
	case f.ended:
	case f.answered:
		f.endLocked(sipua.EndLocalHangup, 200, "", nil)
	case f.cancelStatus >= 0:
		st := f.cancelStatus
		if st == 0 {
			st = 487
		}
		f.endLocked(sipua.EndLocalCancel, st, "", nil)
	}
}

func (f *fakeOutCall) send(ev sipua.CallEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended {
		ev.At = f.clk.Now()
		f.ev <- ev
	}
}

func (f *fakeOutCall) auth() { f.send(sipua.CallEvent{Kind: sipua.EventAuthChallenged}) }

func (f *fakeOutCall) provisional(status int, early bool) {
	ev := sipua.CallEvent{Kind: sipua.EventProvisional, StatusCode: status, EarlyMedia: early}
	if early {
		ev.Media = f.leg
	}
	f.send(ev)
}

func (f *fakeOutCall) answer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ended || f.answered {
		return
	}
	f.answered = true
	now := f.clk.Now()
	f.ev <- sipua.CallEvent{Kind: sipua.EventAnswered, At: now, Media: f.leg, Answer: sipua.AnswerInfo{
		Codec: "PCMU", PtimeMS: 20, TelephoneEvent: true, TelephoneEventPT: 101,
		LocalRTP: "192.0.2.1:20000", RemoteRTP: "203.0.113.9:4000",
		OKReceivedAt: now.Add(-30 * time.Millisecond), AckSentAt: now,
	}}
}

func (f *fakeOutCall) end(cause sipua.EndCause, status int, reason string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended {
		f.endLocked(cause, status, reason, err)
	}
}

func (f *fakeOutCall) reject(status int, reason string) {
	f.end(sipua.EndRemoteRejected, status, reason, nil)
}
func (f *fakeOutCall) bye() { f.end(sipua.EndRemoteBye, 200, "", nil) }

func (f *fakeOutCall) endLocked(cause sipua.EndCause, status int, reason string, err error) {
	f.ended = true
	f.ev <- sipua.CallEvent{Kind: sipua.EventEnded, At: f.clk.Now(), Cause: cause, StatusCode: status, Reason: reason, Err: err}
	close(f.ev)
	f.leg.close()
}

func (f *fakeOutCall) hangupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hangups
}

// fakeDialer is Config.Dial.
type fakeDialer struct {
	clk *clock.Fake
	ops *opLog

	mu           sync.Mutex
	reqs         []sipua.DialRequest
	calls        []*fakeOutCall
	err          error
	cancelStatus int
	newLeg       func() closableLeg // nil = fakeSIPLeg
}

func (d *fakeDialer) dial(_ core.LoginRef, req sipua.DialRequest) (sipua.OutboundCall, error) {
	d.ops.add("dial")
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reqs = append(d.reqs, req)
	if d.err != nil {
		return nil, d.err
	}
	n := len(d.calls) + 1
	var leg closableLeg = newFakeSIPLeg()
	if d.newLeg != nil {
		leg = d.newLeg()
	}
	c := &fakeOutCall{
		clk: d.clk,
		info: sipua.DialInfo{CallID: "out" + strconv.Itoa(n) + "@sip.example.com", Target: req.Target,
			RequestUser: strings.TrimPrefix(req.Target, "+"), StartedAt: d.clk.Now()},
		ev:           make(chan sipua.CallEvent, 16),
		leg:          leg,
		cancelStatus: d.cancelStatus,
	}
	d.calls = append(d.calls, c)
	return c, nil
}

func (d *fakeDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.reqs)
}

func (d *fakeDialer) last() *fakeOutCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		return nil
	}
	return d.calls[len(d.calls)-1]
}

func (d *fakeDialer) req(i int) sipua.DialRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reqs[i]
}

// fakeTones is Manager.startTone.
type fakeTones struct {
	ops *opLog

	mu      sync.Mutex
	started []*fakeTone
	err     error
}

type fakeTone struct {
	p    media.TonePattern
	rtc  media.RTCLeg
	ops  *opLog
	mu   sync.Mutex
	done bool
}

func toneName(p media.TonePattern) string {
	switch p {
	case media.ToneRingback:
		return "ringback"
	case media.ToneBusy:
		return "busy"
	case media.ToneReorder:
		return "reorder"
	default:
		return "unknown"
	}
}

func (t *fakeTones) start(_ media.Config, rtc media.RTCLeg, p media.TonePattern) (media.Tone, error) {
	t.ops.add("tone:" + toneName(p))
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return nil, t.err
	}
	tone := &fakeTone{p: p, rtc: rtc, ops: t.ops}
	t.started = append(t.started, tone)
	return tone, nil
}

func (t *fakeTone) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.done {
		t.done = true
		t.ops.add("tone-stop:" + toneName(t.p))
	}
}

func (t *fakeTone) stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

func (t *fakeTones) patterns() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, x := range t.started {
		out = append(out, toneName(x.p))
	}
	return out
}

// running returns the number of tones not stopped.
func (t *fakeTones) running() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, x := range t.started {
		if !x.stopped() {
			n++
		}
	}
	return n
}

// notifEvent is an Element Call style MSC4075 notification sent at ts.
func notifEvent(sender id.UserID, room id.RoomID, evID id.EventID, ts time.Time) *mevent.Event {
	ms := strconv.FormatInt(ts.UnixMilli(), 10)
	return &mevent.Event{
		Type:      matrixrtc.RTCNotification,
		RoomID:    room,
		ID:        evID,
		Sender:    sender,
		Timestamp: ts.UnixMilli(),
		Content: mevent.Content{VeryRaw: []byte(`{"notification_type":"ring","sender_ts":` + ms +
			`,"lifetime":30000,"m.mentions":{"user_ids":[],"room":true},` +
			`"m.relates_to":{"rel_type":"m.reference","event_id":"$ownermember"}}`)},
	}
}

// memberAt is an owner membership with explicit created_ts and origin timestamp.
func memberAt(user id.UserID, device string, room id.RoomID, created, ts time.Time) *mevent.Event {
	e := memberEvent(user, device, true, created, room)
	e.Timestamp = ts.UnixMilli()
	e.ID = id.EventID("$m-" + device + strconv.FormatInt(ts.UnixNano(), 10))
	return e
}

const testNotif = id.EventID("$notif1")

// ownerStart simulates the owner pressing call in testRoom: fresh live membership then notification.
func (h *harness) ownerStart(device string) {
	h.matrix(h.ownerMember(device, true))
	h.matrix(notifEvent(testOwner, testRoom, testNotif, h.clk.Now()))
}

// ownerJoinedLK reports the owner's LiveKit participant on the latest session (no grace timer).
func (h *harness) ownerJoinedLK(device string) {
	s := h.joiner.lastSession()
	if s == nil {
		h.t.Fatal("no LiveKit session")
	}
	s.push(rtc.Event{Kind: rtc.EventOwnerJoined, Identity: string(testOwner) + ":" + device})
	synctest.Wait()
}

// dialed returns the single outbound call, failing the test if Dial was not called.
func (h *harness) dialed() *fakeOutCall {
	h.t.Helper()
	oc := h.dialer.last()
	if oc == nil {
		h.t.Fatalf("Dial not called; ops %v", h.ops.list())
	}
	return oc
}

// declines returns the decline messages sent by the ghost.
func (h *harness) declines() []msgCall {
	var out []msgCall
	for _, m := range h.ghost.msgCalls() {
		if m.typ == matrixrtc.RTCDecline {
			out = append(out, m)
		}
	}
	return out
}

// outAttr returns attribute key of the call.outbound span.
func (h *harness) outAttr(key string) string {
	h.t.Helper()
	s := h.env.SpanByName("call.outbound")
	if s == nil {
		h.t.Fatal("no call.outbound span")
	}
	return obstest.Attr(s, key).Emit()
}

// ---- MatrixRTC 2.0 ghost ----

// stickyCall is one SendSticky call of a fakeRTCGhost.
type stickyCall struct {
	typ     mevent.Type
	content map[string]any
	d       time.Duration
}

// delayedCall is one ScheduleDelayed call.
type delayedCall struct {
	typ        mevent.Type
	content    map[string]any
	delay, dur time.Duration
	id         core.DelayedEventID
}

// updateCall is one UpdateDelayed call.
type updateCall struct {
	id     core.DelayedEventID
	action string
}

// fakeRTCGhost is a fakeGhost with the optional core.GhostRTCIntent capability. It records every call in
// order in the shared opLog ("slot:open:<id>", "delayed", "sticky:join", "sticky:leave", "sticky:ring",
// "sticky:withdraw", "sticky:decline", "delayed:restart", "delayed:send", "delayed:cancel") and has
// injectable errors.
type fakeRTCGhost struct {
	*fakeGhost

	rmu     sync.Mutex
	caps    map[string]bool
	stickys []stickyCall
	delays  []delayedCall
	updates []updateCall
	slots   map[string]map[string]any // preset slot state by slot id (every slot type)
	typed   map[string]map[string]any // slot state by type|id: set by SendState, wins over slots
	opened  []string                  // slot ids opened via SendState (type|id)
	reads   []string

	// Injectable errors (called under no lock; n = 0-based call index of that kind).
	stickyErr   func(n int, c stickyCall) error
	delayErr    func(n int) error
	updateErr   func(n int, c updateCall) error
	stateErr    error
	slotOpenErr error
}

// newFakeRTCGhost returns a ghost advertising sticky and delayed events.
func newFakeRTCGhost(g *fakeGhost) *fakeRTCGhost {
	return &fakeRTCGhost{fakeGhost: g, caps: map[string]bool{matrixrtc.FeatureSticky: true, matrixrtc.FeatureDelayed: true},
		slots: map[string]map[string]any{}, typed: map[string]map[string]any{}}
}

func (g *fakeRTCGhost) SendState(ctx context.Context, room id.RoomID, typ mevent.Type, key string, content map[string]any) (id.EventID, error) {
	if matrixrtc.IsSlotType(typ) {
		g.rmu.Lock()
		defer g.rmu.Unlock()
		if g.slotOpenErr != nil {
			return "", g.slotOpenErr
		}
		g.opened = append(g.opened, typ.Type+"|"+key)
		g.typed[typ.Type+"|"+key] = content
		g.ops.add("slot:open:" + key)
		return id.EventID("$slot" + key), nil
	}
	return g.fakeGhost.SendState(ctx, room, typ, key, content)
}

func (g *fakeRTCGhost) SendSticky(_ context.Context, _ id.RoomID, typ mevent.Type, content map[string]any, d time.Duration) (id.EventID, error) {
	c := stickyCall{typ, content, d}
	g.rmu.Lock()
	n := len(g.stickys)
	g.stickys = append(g.stickys, c)
	hook := g.stickyErr
	g.rmu.Unlock()
	kind := "sticky:join"
	switch {
	case typ == matrixrtc.RTCNotification:
		kind = "sticky:ring"
		if _, ok := content[matrixrtc.FieldNotificationType]; !ok {
			kind = "sticky:withdraw"
		}
	case typ == matrixrtc.RTCDecline:
		kind = "sticky:decline"
	default:
		if m, _ := content[matrixrtc.FieldMember].(map[string]any); m != nil && m[matrixrtc.FieldMembership] == matrixrtc.MembershipLeave {
			kind = "sticky:leave"
		}
	}
	g.ops.add(kind)
	if hook != nil {
		if err := hook(n, c); err != nil {
			return "", err
		}
	}
	return id.EventID("$sticky" + strconv.Itoa(n+1)), nil
}

func (g *fakeRTCGhost) ScheduleDelayed(_ context.Context, _ id.RoomID, typ mevent.Type, content map[string]any, delay, sticky time.Duration) (core.DelayedEventID, error) {
	g.rmu.Lock()
	n := len(g.delays)
	did := core.DelayedEventID("delay" + strconv.Itoa(n+1))
	g.delays = append(g.delays, delayedCall{typ, content, delay, sticky, did})
	hook := g.delayErr
	g.rmu.Unlock()
	g.ops.add("delayed")
	if hook != nil {
		if err := hook(n); err != nil {
			return "", err
		}
	}
	return did, nil
}

func (g *fakeRTCGhost) UpdateDelayed(_ context.Context, did core.DelayedEventID, action string) error {
	c := updateCall{did, action}
	g.rmu.Lock()
	n := len(g.updates)
	g.updates = append(g.updates, c)
	hook := g.updateErr
	g.rmu.Unlock()
	if action != "restart" {
		g.ops.add("delayed:" + action)
	}
	if hook != nil {
		return hook(n, c)
	}
	return nil
}

func (g *fakeRTCGhost) StateEvent(_ context.Context, _ id.RoomID, typ mevent.Type, key string) (map[string]any, error) {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	g.reads = append(g.reads, typ.Type+"|"+key)
	if g.stateErr != nil {
		return nil, g.stateErr
	}
	if c, ok := g.typed[typ.Type+"|"+key]; ok {
		return c, nil
	}
	return g.slots[key], nil
}

func (g *fakeRTCGhost) RTCTransports(context.Context) ([]byte, error) {
	return nil, core.ErrUnsupported
}

func (g *fakeRTCGhost) LiveKitToken(context.Context, core.LiveKitTokenRequest) (string, error) {
	return "", core.ErrUnsupported
}

func (g *fakeRTCGhost) Capabilities(context.Context) map[string]bool {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	out := make(map[string]bool, len(g.caps))
	for k, v := range g.caps {
		out[k] = v
	}
	return out
}

func (g *fakeRTCGhost) stickyList() []stickyCall {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	return append([]stickyCall(nil), g.stickys...)
}

func (g *fakeRTCGhost) delayList() []delayedCall {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	return append([]delayedCall(nil), g.delays...)
}

func (g *fakeRTCGhost) updateList() []updateCall {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	return append([]updateCall(nil), g.updates...)
}

func (g *fakeRTCGhost) openedList() []string {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	return append([]string(nil), g.opened...)
}

// withRTCGhost makes every call room's ghost a fakeRTCGhost; the pointer is stored in *out when non-nil.
func withRTCGhost(out **fakeRTCGhost, mods ...func(*fakeRTCGhost)) harnessOpt {
	return func(h *harness) {
		g := newFakeRTCGhost(h.ghost)
		for _, m := range mods {
			m(g)
		}
		h.host.ghostOverride = g
		if out != nil {
			*out = g
		}
	}
}
