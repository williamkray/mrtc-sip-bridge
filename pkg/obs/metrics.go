package obs

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Instruments is the registry of TELEMETRY-SPEC §4.3 instruments created by Use (the T6 set;
// Phase 2/3 instruments are added by those phases). Every field is non-nil: with no MeterProvider they
// are no-op instruments, so callers never check MetricsEnabled before recording.
type Instruments struct {
	// Meter is exposed so hosts can register callbacks (Meter.RegisterCallback with Registrations).
	Meter metric.Meter

	RegisterAttempts   metric.Int64Counter         // mrtcsip.sip.register.attempts
	RegisterDuration   metric.Float64Histogram     // mrtcsip.sip.register.duration
	Registrations      metric.Int64ObservableGauge // mrtcsip.sip.registrations (callback registered by the caller)
	MatrixRTCEvents    metric.Int64Counter         // mrtcsip.matrixrtc.events
	Logins             metric.Int64Counter         // mrtcsip.bridge.logins
	HTTPClientDuration metric.Float64Histogram     // http.client.request.duration

	// Phase 2 (PHASE2-SPEC 4.4, TELEMETRY-SPEC 4.3).
	CallStarted       metric.Int64Counter       // mrtcsip.call.started        {call}
	CallEnded         metric.Int64Counter       // mrtcsip.call.ended          {call}
	CallActive        metric.Int64UpDownCounter // mrtcsip.call.active         {call}
	CallSetupDuration metric.Float64Histogram   // mrtcsip.call.setup.duration s
	CallRingDuration  metric.Float64Histogram   // mrtcsip.call.ring.duration  s
	CallDuration      metric.Float64Histogram   // mrtcsip.call.duration       s
	RTCJoinDuration   metric.Float64Histogram   // mrtcsip.rtc.join.duration   s
	RTPPackets        metric.Int64Counter       // mrtcsip.media.rtp.packets   {packet} (SIP leg)
	RTPLost           metric.Int64Counter       // mrtcsip.media.rtp.lost      {packet} (SIP leg)
	RTPLate           metric.Int64Counter       // mrtcsip.media.rtp.late      {packet} (SIP leg)
	RTPJitter         metric.Float64Histogram   // mrtcsip.media.rtp.jitter    s (SIP leg)
	MediaConcealed    metric.Int64Counter       // mrtcsip.media.concealed     {frame}
	DTMFSent          metric.Int64Counter       // mrtcsip.dtmf.sent           {digit}, attribute dtmf.method=rfc4733

	// MatrixRTC 2.0 (docs/MATRIXRTC2-SPEC.md WP5).
	DelayedLeaveRestarts metric.Int64Counter // mrtcsip.matrixrtc.delayed_leave.restarts {restart}, outcome=ok|gone|error
}

// M returns the current instrument registry (never nil).
func M() *Instruments { return cur.Load().inst }

// newInstruments builds the registry from mp (nil means no-op instruments).
func newInstruments(mp metric.MeterProvider) *Instruments {
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	m := mp.Meter(scopeName)
	in := &Instruments{Meter: m}
	var err error
	handle := func(e error) {
		if e != nil {
			otel.Handle(e)
		}
	}
	in.RegisterAttempts, err = m.Int64Counter("mrtcsip.sip.register.attempts",
		metric.WithUnit("{attempt}"), metric.WithDescription("SIP REGISTER transaction results"))
	handle(err)
	in.RegisterDuration, err = m.Float64Histogram("mrtcsip.sip.register.duration",
		metric.WithUnit("s"), metric.WithDescription("SIP REGISTER attempt duration"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	handle(err)
	in.Registrations, err = m.Int64ObservableGauge("mrtcsip.sip.registrations",
		metric.WithUnit("{registration}"), metric.WithDescription("Active SIP user agents by state"))
	handle(err)
	in.MatrixRTCEvents, err = m.Int64Counter("mrtcsip.matrixrtc.events",
		metric.WithUnit("{event}"), metric.WithDescription("MatrixRTC events handled by ingest"))
	handle(err)
	in.Logins, err = m.Int64Counter("mrtcsip.bridge.logins",
		metric.WithUnit("{login}"), metric.WithDescription("Login submit results"))
	handle(err)
	in.HTTPClientDuration, err = m.Float64Histogram("http.client.request.duration",
		metric.WithUnit("s"), metric.WithDescription("Outbound HTTP request duration"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10))
	handle(err)

	setupBuckets := metric.WithExplicitBucketBoundaries(0.25, 0.5, 1, 2, 3, 5, 10, 20, 30, 60)
	in.CallStarted, err = m.Int64Counter("mrtcsip.call.started",
		metric.WithUnit("{call}"), metric.WithDescription("Calls started"))
	handle(err)
	in.CallEnded, err = m.Int64Counter("mrtcsip.call.ended",
		metric.WithUnit("{call}"), metric.WithDescription("Calls ended by outcome"))
	handle(err)
	in.CallActive, err = m.Int64UpDownCounter("mrtcsip.call.active",
		metric.WithUnit("{call}"), metric.WithDescription("Calls currently in progress"))
	handle(err)
	in.CallSetupDuration, err = m.Float64Histogram("mrtcsip.call.setup.duration",
		metric.WithUnit("s"), metric.WithDescription("Time from INVITE to call answered or ended"), setupBuckets)
	handle(err)
	in.CallRingDuration, err = m.Float64Histogram("mrtcsip.call.ring.duration",
		metric.WithUnit("s"), metric.WithDescription("Time a call rang before it ended or was answered"), setupBuckets)
	handle(err)
	in.CallDuration, err = m.Float64Histogram("mrtcsip.call.duration",
		metric.WithUnit("s"), metric.WithDescription("Answered call duration"),
		metric.WithExplicitBucketBoundaries(10, 30, 60, 120, 300, 600, 1800, 3600, 7200))
	handle(err)
	in.RTCJoinDuration, err = m.Float64Histogram("mrtcsip.rtc.join.duration",
		metric.WithUnit("s"), metric.WithDescription("LiveKit join duration (OpenID to track published)"),
		metric.WithExplicitBucketBoundaries(0.1, 0.25, 0.5, 1, 2, 3, 5, 10))
	handle(err)
	in.RTPPackets, err = m.Int64Counter("mrtcsip.media.rtp.packets",
		metric.WithUnit("{packet}"), metric.WithDescription("SIP RTP packets"))
	handle(err)
	in.RTPLost, err = m.Int64Counter("mrtcsip.media.rtp.lost",
		metric.WithUnit("{packet}"), metric.WithDescription("SIP RTP packets lost"))
	handle(err)
	in.RTPLate, err = m.Int64Counter("mrtcsip.media.rtp.late",
		metric.WithUnit("{packet}"), metric.WithDescription("SIP RTP packets discarded as late"))
	handle(err)
	in.RTPJitter, err = m.Float64Histogram("mrtcsip.media.rtp.jitter",
		metric.WithUnit("s"), metric.WithDescription("SIP RTP interarrival jitter"),
		metric.WithExplicitBucketBoundaries(0.001, 0.0025, 0.005, 0.01, 0.02, 0.04, 0.08, 0.16))
	handle(err)
	in.MediaConcealed, err = m.Int64Counter("mrtcsip.media.concealed",
		metric.WithUnit("{frame}"), metric.WithDescription("Frames sent as concealment or silence"))
	handle(err)
	in.DTMFSent, err = m.Int64Counter("mrtcsip.dtmf.sent",
		metric.WithUnit("{digit}"), metric.WithDescription("DTMF digits sent to the SIP side (RFC 4733)"))
	handle(err)
	in.DelayedLeaveRestarts, err = m.Int64Counter("mrtcsip.matrixrtc.delayed_leave.restarts",
		metric.WithUnit("{restart}"), metric.WithDescription("MSC4140 delayed leave restarts for ghost sticky memberships"))
	handle(err)
	return in
}
