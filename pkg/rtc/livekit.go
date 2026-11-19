package rtc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

const (
	eventBuffer    = 32
	defaultConnect = 10 * time.Second
	readBufSize    = 1500
)

// tokenTimeout bounds the whole MSC4195 get_token call (stable path, then unstable) so a stalled
// homeserver or auth service cannot eat the join budget (the actor's join timer is 10 s) and the
// sfu_get fallback still has time to run. A variable so tests can shorten it.
var tokenTimeout = 4 * time.Second

// roomConn is the seam over *lksdk.Room used by Join (faked in tests).
type roomConn interface {
	Name() string
	LocalIdentity() string
	Publish(track *lksdk.LocalTrack) error
	// ScanRemote calls fn for every remote participant already present; for those where fn returns true
	// (owner) the audio publications are subscribed (never inline in an SDK callback). May be called
	// repeatedly; subscribing is idempotent.
	ScanRemote(fn func(identity string) bool)
	// HasParticipant reports whether a remote participant with this identity is currently in the room.
	HasParticipant(identity string) bool
	Disconnect()
}

// dialFunc connects to LiveKit. cb is built by Join.
type dialFunc func(url, jwt string, cb *lksdk.RoomCallback, connectTimeout time.Duration) (roomConn, error)

type realRoom struct{ r *lksdk.Room }

func dialLiveKit(url, jwt string, cb *lksdk.RoomCallback, to time.Duration) (roomConn, error) {
	room, err := lksdk.ConnectToRoomWithToken(url, jwt, cb, lksdk.WithAutoSubscribe(false), lksdk.WithConnectTimeout(to))
	if err != nil {
		return nil, err
	}
	return &realRoom{r: room}, nil
}

func (r *realRoom) Name() string          { return r.r.Name() }
func (r *realRoom) LocalIdentity() string { return r.r.LocalParticipant.Identity() }
func (r *realRoom) HasParticipant(identity string) bool {
	return r.r.GetParticipantByIdentity(identity) != nil
}
func (r *realRoom) Disconnect() { r.r.Disconnect() }
func (r *realRoom) Publish(track *lksdk.LocalTrack) error {
	_, err := r.r.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "audio", Source: livekit.TrackSource_MICROPHONE, DisableDTX: true,
	})
	return err
}
func (r *realRoom) ScanRemote(fn func(identity string) bool) {
	for _, rp := range r.r.GetRemoteParticipants() {
		if fn(rp.Identity()) {
			subscribeAudio(rp)
		}
	}
}

func subscribeAudio(rp *lksdk.RemoteParticipant) {
	for _, pub := range rp.TrackPublications() {
		if pub.Kind() == lksdk.TrackKindAudio {
			if rpub, ok := pub.(*lksdk.RemoteTrackPublication); ok {
				go func() { _ = rpub.SetSubscribed(true) }()
			}
		}
	}
}

type joiner struct {
	hc             *http.Client
	connectTimeout time.Duration
	log            zerolog.Logger
	clk            clock.Clock
	dial           dialFunc
}

func newJoiner(cfg Config) Joiner {
	j := &joiner{hc: cfg.HTTPClient, connectTimeout: cfg.ConnectTimeout, log: cfg.Log, clk: cfg.Clock, dial: dialLiveKit}
	if j.hc == nil {
		j.hc = defaultHTTPClient()
	}
	if j.connectTimeout <= 0 {
		j.connectTimeout = defaultConnect
	}
	if j.clk == nil {
		j.clk = clock.Real()
	}
	return j
}

func fail(stage string, err error) error { return &JoinError{Stage: stage, Err: err} }

func (j *joiner) Join(ctx context.Context, p JoinParams) (sess Session, err error) {
	start := j.clk.Now()
	ctx, span := obs.StartChild(ctx, "rtc.join", trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(
		obs.AttrCallID.String(p.CallID),
		obs.AttrMatrixRoomID.String(string(p.RoomID)),
		obs.AttrMatrixGhostID.String(string(p.Ghost.UserID())),
		obs.AttrMatrixRTCDeviceID.String(p.DeviceID),
	))
	if p.Attempt > 0 {
		span.SetAttributes(obs.AttrRTCJoinAttempt.Int(p.Attempt))
	}
	dialStarted := false
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "publish_error"
			var je *JoinError
			if errors.As(err, &je) {
				outcome = je.Stage
				je.BeforeConnect = !dialStarted
			}
			obs.Fail(span, err)
		}
		if obs.MetricsEnabled() {
			obs.M().RTCJoinDuration.Record(ctx, j.clk.Now().Sub(start).Seconds(),
				metric.WithAttributes(obs.AttrOutcome.String(outcome)))
		}
		span.End()
	}()
	log := obs.Logger(ctx, &j.log).With().Str("call_id", p.CallID).Logger()
	timeoutOr := func(stage string, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return fail("timeout", cerr)
		}
		return fail(stage, err)
	}

	var lkURL, jwt string
	if p.Auth == AuthMSC4195 {
		// 1+2. Homeserver get_token as the ghost (MSC4195); no OpenID step.
		gr, ok := p.Ghost.(core.GhostRTCIntent)
		if !ok {
			return nil, fail("token_error", errors.New("ghost does not support MSC4195 get_token"))
		}
		if p.LiveKitURL == "" {
			return nil, fail("token_error", errors.New("no LiveKit url for MSC4195 get_token"))
		}
		lkURL = p.LiveKitURL
		jctx, s2 := obs.StartChild(ctx, "rtc.jwt_fetch", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(obs.AttrServerAddress.String(hostOf(lkURL)), obs.AttrRTCTokenEndpoint.String("get_token_cs")))
		var terr error
		tctx, tcancel := context.WithTimeout(jctx, tokenTimeout)
		jwt, terr = gr.LiveKitToken(tctx, core.LiveKitTokenRequest{URL: p.LiveKitURL, RoomID: p.RoomID, SlotID: p.SlotID, MemberID: p.MemberID})
		tcancel()
		if terr == nil && jwt == "" {
			terr = errors.New("get_token returned no jwt")
		}
		if terr != nil {
			obs.Fail(s2, terr)
			s2.End()
			return nil, timeoutOr("token_error", terr)
		}
		s2.End()
	} else {
		// 1. OpenID token as the ghost. Never logged or recorded.
		_, s1 := obs.StartChild(ctx, "rtc.openid_token", trace.WithSpanKind(trace.SpanKindClient))
		tok, err := p.Ghost.RequestOpenIDToken(ctx)
		if err != nil {
			obs.Fail(s1, err)
			s1.End()
			return nil, timeoutOr("openid_error", err)
		}
		s1.End()
		if err := ctx.Err(); err != nil {
			return nil, fail("timeout", err)
		}

		// 2. lk-jwt-service.
		jctx, s2 := obs.StartChild(ctx, "rtc.jwt_fetch", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(obs.AttrServerAddress.String(hostOf(p.ServiceURL)), obs.AttrRTCTokenEndpoint.String("sfu_get")))
		var ferr error
		lkURL, jwt, ferr = fetchToken(jctx, j.hc, p.ServiceURL, p.RoomID, p.DeviceID, tok)
		var se *tokenStatusError
		switch {
		case ferr == nil:
			s2.SetAttributes(obs.AttrHTTPResponseStatus.Int(200))
		case errors.As(ferr, &se):
			s2.SetAttributes(obs.AttrHTTPResponseStatus.Int(se.Status))
		}
		if ferr != nil {
			obs.Fail(s2, ferr)
			s2.End()
			return nil, timeoutOr("token_error", ferr)
		}
		s2.End()
	}
	if err := ctx.Err(); err != nil {
		return nil, fail("timeout", err)
	}

	// 3. Connect.
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	})
	if err != nil {
		return nil, fail("connect_error", err)
	}
	s := newSession(p, log, j.clk)
	s.leg = &rtcLeg{s: s, write: func(frame []byte, d time.Duration) error {
		return track.WriteSample(pionmedia.Sample{Data: frame, Duration: d}, nil)
	}}
	lkHost := hostOf(lkURL)
	s.server = lkHost
	_, s3 := obs.StartChild(ctx, "rtc.livekit.connect", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(obs.AttrLiveKitServer.String(lkHost)))
	dialStarted = true
	room, err := j.dial(lkURL, jwt, s.callbacks(), j.connectTimeout)
	if err != nil {
		obs.Fail(s3, err)
		s3.End()
		s.closeEvents()
		stage := "connect_error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, lksdk.ErrConnectionTimeout) {
			stage = "timeout"
		}
		return nil, timeoutOr(stage, err)
	}
	s.room = room
	s.roomPtr.Store(&roomHolder{room})
	s.identity = room.LocalIdentity()
	s.roomName = room.Name()
	s3.SetAttributes(obs.AttrLiveKitRoom.String(s.roomName), obs.AttrLiveKitIdentity.String(s.identity))
	s3.End()
	want := matrixrtc.LegacyLiveKitIdentity(p.Ghost.UserID(), p.DeviceID)
	if p.Auth == AuthMSC4195 {
		want = matrixrtc.LiveKitIdentity(p.Ghost.UserID(), p.MemberID)
	}
	if s.identity != want {
		log.Warn().Str("identity", s.identity).Str("want", want).
			Msg("LiveKit identity differs from membership-derived identity: owner clients will not render this participant")
	}
	if err := ctx.Err(); err != nil {
		s.abort()
		return nil, fail("timeout", err)
	}

	// 4. Publish.
	_, s4 := obs.StartChild(ctx, "rtc.track.publish", trace.WithSpanKind(trace.SpanKindInternal))
	if err := room.Publish(track); err != nil {
		obs.Fail(s4, err)
		s4.End()
		s.abort()
		return nil, timeoutOr("publish_error", err)
	}
	s4.End()

	// 5. Participants already present.
	s.rescan()
	return s, nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

// AddOwnerIdentities implements Session.
func (s *session) AddOwnerIdentities(ids ...string) {
	s.owners.add(ids...)
	if s.leaving.Load() {
		return
	}
	s.rescan()
}

// rescan emits EventOwnerJoined for owner participants already present that were not announced yet and
// has the room subscribe their audio. Non-owners are never subscribed. Called from Join and from
// AddOwnerIdentities (caller goroutines, never an SDK callback).
func (s *session) rescan() {
	h := s.roomPtr.Load()
	if h == nil {
		return
	}
	h.r.ScanRemote(func(identity string) bool {
		if !s.owners.match(identity) || !s.markAnnounced(identity) {
			return false
		}
		s.emit(Event{Kind: EventOwnerJoined, Identity: identity})
		return true
	})
}

// markAnnounced records that OwnerJoined was (or is being) emitted for identity; false if it already was.
func (s *session) markAnnounced(identity string) bool {
	s.annMu.Lock()
	defer s.annMu.Unlock()
	if _, ok := s.announced[identity]; ok {
		return false
	}
	s.announced[identity] = struct{}{}
	return true
}

func (s *session) clearAnnounced(identity string) {
	s.annMu.Lock()
	delete(s.announced, identity)
	s.annMu.Unlock()
}

// ownerTrack is one subscribed owner audio track.
type ownerTrack struct {
	identity string
	r        packetReader
}

type packetReader interface {
	Read(b []byte) (int, error)
}

type remoteReader struct{ t *webrtc.TrackRemote }

func (r remoteReader) Read(b []byte) (int, error) {
	n, _, err := r.t.Read(b)
	return n, err
}

type session struct {
	p        JoinParams
	log      zerolog.Logger
	clk      clock.Clock
	room     roomConn
	roomPtr  atomic.Pointer[roomHolder] // set after dial; read by SDK callbacks
	leg      *rtcLeg
	identity string
	roomName string
	server   string

	events   chan Event
	evMu     sync.RWMutex
	evClosed bool

	leaving  atomic.Bool
	leaveRun sync.Once
	left     chan struct{}

	receiver atomic.Pointer[receiverBox]

	owners    *ownerMatcher
	annMu     sync.Mutex
	announced map[string]struct{} // owner identities OwnerJoined was emitted for

	trMu    sync.Mutex
	tracks  []*ownerTrack
	current atomic.Pointer[ownerTrack]
}

type roomHolder struct{ r roomConn }

type receiverBox struct{ r media.PacketReceiver }

func newSession(p JoinParams, log zerolog.Logger, clk clock.Clock) *session {
	return &session{p: p, log: log, clk: clk, events: make(chan Event, eventBuffer), left: make(chan struct{}),
		owners: newOwnerMatcher(p.Owner, p.Owners), announced: map[string]struct{}{}}
}

func (s *session) Identity() string      { return s.identity }
func (s *session) LiveKitRoom() string   { return s.roomName }
func (s *session) ServerAddress() string { return s.server }
func (s *session) Audio() media.RTCLeg   { return s.leg }
func (s *session) Events() <-chan Event  { return s.events }

// emit never blocks; an event that does not fit (or arrives after close) is dropped.
func (s *session) emit(ev Event) {
	ev.At = s.clk.Now()
	s.evMu.RLock()
	defer s.evMu.RUnlock()
	if s.evClosed {
		return
	}
	select {
	case s.events <- ev:
	default:
		s.log.Warn().Int("kind", int(ev.Kind)).Msg("rtc event channel full, event dropped")
	}
}

func (s *session) closeEvents() {
	s.evMu.Lock()
	if !s.evClosed {
		s.evClosed = true
		close(s.events)
	}
	s.evMu.Unlock()
}

// abort tears down a half-joined session (Join failure path).
func (s *session) abort() {
	s.leaving.Store(true)
	s.room.Disconnect()
	s.closeEvents()
}

func (s *session) Leave(ctx context.Context) error {
	first := false
	s.leaveRun.Do(func() { first = true })
	if !first {
		select {
		case <-s.left:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer close(s.left)
	s.leaving.Store(true)
	s.receiver.Store(nil)
	done := make(chan struct{})
	go func() {
		s.room.Disconnect()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	s.closeEvents()
	return err
}

// callbacks builds the SDK callbacks; every one returns immediately.
func (s *session) callbacks() *lksdk.RoomCallback {
	return &lksdk.RoomCallback{
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			if !s.leaving.Load() {
				s.emit(Event{Kind: EventDisconnected, Reason: string(reason)})
			}
		},
		OnParticipantConnected: func(rp *lksdk.RemoteParticipant) {
			s.onParticipantConnected(rp.Identity())
		},
		OnParticipantDisconnected: func(rp *lksdk.RemoteParticipant) {
			s.onParticipantDisconnected(rp.Identity())
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackPublished: func(pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if pub.Kind() == lksdk.TrackKindAudio && s.owners.match(rp.Identity()) {
					go func() { _ = pub.SetSubscribed(true) }()
				}
			},
			OnTrackSubscribed: func(t *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				if t.Kind() != webrtc.RTPCodecTypeAudio || !s.owners.match(rp.Identity()) {
					return
				}
				s.addTrack(rp.Identity(), remoteReader{t})
			},
		},
	}
}

// onParticipantConnected handles a remote participant joining (SDK callback goroutine, no I/O). Only an
// owner identity that was not announced yet produces EventOwnerJoined (rescan may have announced it
// already); a departure clears the mark (onParticipantDisconnected), so a reconnect is announced again.
func (s *session) onParticipantConnected(identity string) {
	if s.owners.match(identity) && s.markAnnounced(identity) {
		s.emit(Event{Kind: EventOwnerJoined, Identity: identity})
	}
}

// onParticipantDisconnected handles a remote participant leaving (SDK callback goroutine).
func (s *session) onParticipantDisconnected(identity string) {
	if !s.owners.match(identity) {
		return
	}
	s.removeIdentity(identity)
	// lksdk runs the connect/disconnect callbacks in separate goroutines, so on an owner reconnect with
	// the same identity this one may run after the Connected one. If the participant is back in the
	// room, the departure is stale: do not report it.
	if h := s.roomPtr.Load(); h != nil && h.r.HasParticipant(identity) {
		return
	}
	s.clearAnnounced(identity)
	s.emit(Event{Kind: EventOwnerLeft, Identity: identity})
}

// addTrack makes r the current owner track and starts its reader goroutine.
func (s *session) addTrack(identity string, r packetReader) *ownerTrack {
	ot := &ownerTrack{identity: identity, r: r}
	s.trMu.Lock()
	s.tracks = append(s.tracks, ot)
	s.current.Store(ot)
	s.trMu.Unlock()
	go s.readLoop(ot)
	return ot
}

// removeTrack forgets ot; if it was current, the most recent remaining owner track takes over.
func (s *session) removeTrack(match func(*ownerTrack) bool) {
	s.trMu.Lock()
	defer s.trMu.Unlock()
	kept := s.tracks[:0]
	for _, t := range s.tracks {
		if !match(t) {
			kept = append(kept, t)
		}
	}
	for i := len(kept); i < len(s.tracks); i++ {
		s.tracks[i] = nil
	}
	s.tracks = kept
	if cur := s.current.Load(); cur != nil && match(cur) {
		var next *ownerTrack
		if n := len(kept); n > 0 {
			next = kept[n-1]
		}
		s.current.Store(next)
	}
}

func (s *session) removeIdentity(identity string) {
	s.removeTrack(func(t *ownerTrack) bool { return t.identity == identity })
}

// readLoop forwards one track's packets to the receiver while it is the current owner track. No
// allocation per packet.
func (s *session) readLoop(ot *ownerTrack) {
	defer s.removeTrack(func(t *ownerTrack) bool { return t == ot })
	buf := make([]byte, readBufSize)
	var pkt rtp.Packet
	var mp media.RTPPacket
	for {
		n, err := ot.r.Read(buf)
		if err != nil {
			return
		}
		if s.current.Load() != ot {
			continue
		}
		rb := s.receiver.Load()
		if rb == nil || rb.r == nil {
			continue
		}
		if pkt.Unmarshal(buf[:n]) != nil {
			continue
		}
		mp.SSRC, mp.Seq, mp.Timestamp = pkt.SSRC, pkt.SequenceNumber, pkt.Timestamp
		mp.PayloadType, mp.Marker, mp.Payload = pkt.PayloadType, pkt.Marker, pkt.Payload
		rb.r.ReceiveRTP(&mp)
	}
}

type rtcLeg struct {
	s     *session
	write func(frame []byte, d time.Duration) error
}

func (l *rtcLeg) WriteOpus(frame []byte, d time.Duration) error { return l.write(frame, d) }

func (l *rtcLeg) SetReceiver(r media.PacketReceiver) {
	if r == nil {
		l.s.receiver.Store(nil)
		return
	}
	l.s.receiver.Store(&receiverBox{r: r})
}
