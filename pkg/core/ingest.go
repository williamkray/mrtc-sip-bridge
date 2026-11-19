package core

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// RTCEventSink receives parsed MatrixRTC events. HandleRTCEvent is called synchronously from the
// appservice transaction path (EventProcessor ExecMode Sync by default, S3 caveats): it MUST NOT block
// or do network I/O; hand work to a goroutine instead. The sink may set `matrixrtc.routed_to` and
// `call.id` on `trace.SpanFromContext(ctx)`; the default is `none`.
type RTCEventSink interface {
	HandleRTCEvent(ctx context.Context, login LoginRef, evt matrixrtc.Event)
}

// LogSink is the Phase 1 sink. It logs one Info line with fields login (masked), kind, type, room_id,
// event_id, sender, state_key and message "MatrixRTC event ingested". It never logs Content.
// Log is a clean injected logger; under a span the line carries the trace correlation fields
// (obs.Logger). A nil Log (LogSink{}) logs through the logger on the ctx (zerolog.Ctx).
type LogSink struct {
	Log *zerolog.Logger
}

// HandleRTCEvent implements RTCEventSink by logging the event envelope (never its content).
func (s LogSink) HandleRTCEvent(ctx context.Context, login LoginRef, evt matrixrtc.Event) {
	l := zerolog.Ctx(ctx)
	if s.Log != nil {
		l = obs.Logger(ctx, s.Log)
	}
	l.Info().
		Str("login", string(login)).
		Stringer("kind", evt.Kind).
		Str("type", evt.Type).
		Stringer("room_id", evt.RoomID).
		Stringer("event_id", evt.EventID).
		Stringer("sender", evt.Sender).
		Str("state_key", evt.StateKey).
		Msg("MatrixRTC event ingested")
}

// Ingest is the MatrixRTC event-ingest entry point that hosts feed.
type Ingest struct {
	mu          sync.RWMutex
	sink        RTCEventSink
	localNumber func(LoginRef) string // nil: the standalone rule (LoginRef up to the first '@')
}

// NewIngest returns an Ingest forwarding to sink; a nil sink means LogSink{}.
func NewIngest(sink RTCEventSink) *Ingest {
	in := &Ingest{}
	in.SetSink(sink)
	return in
}

// SetSink replaces the sink (Phase 2 swaps in the call manager). nil means LogSink{}.
func (in *Ingest) SetSink(sink RTCEventSink) {
	if sink == nil {
		sink = LogSink{}
	}
	in.mu.Lock()
	in.sink = sink
	in.mu.Unlock()
}

// SetLocalNumber sets how the matrixrtc.ingest span gets call.local_e164 for a login. nil (default)
// keeps the standalone rule (LoginRef up to the first '@'). f must not block (no I/O).
func (in *Ingest) SetLocalNumber(f func(LoginRef) string) {
	in.mu.Lock()
	in.localNumber = f
	in.mu.Unlock()
}

// localE164 returns the call.local_e164 value of the matrixrtc.ingest span for login.
func (in *Ingest) localE164(login LoginRef) string {
	in.mu.RLock()
	f := in.localNumber
	in.mu.RUnlock()
	if f != nil {
		return f(login)
	}
	number, _, _ := strings.Cut(string(login), "@") // LoginRef is "<E.164>@<registrar host>" in the standalone host
	return number
}

// HandleMatrixRTCEvent parses evt with matrixrtc.FromMatrixEvent and forwards it to the sink.
// Returns false (and does nothing) if evt is nil, the kind is unknown, or login is "".
func (in *Ingest) HandleMatrixRTCEvent(ctx context.Context, login LoginRef, evt *event.Event) bool {
	return in.ingest(ctx, login, login != "", evt, time.Now())
}

// ingest counts the event and, when it is routed to a login and parses, forwards it to the sink inside
// a matrixrtc.ingest span. The span starts after the resolver accepted the event, backdated to t0 (handler
// entry), so dropped events create no span but are counted with routed=false. The span ends before
// return and never records event content.
func (in *Ingest) ingest(ctx context.Context, login LoginRef, routed bool, evt *event.Event, t0 time.Time) bool {
	kind := matrixrtc.KindUnknown
	if evt != nil {
		kind = matrixrtc.KindOf(evt.Type)
	}
	var parsed matrixrtc.Event
	handled := false
	if routed {
		parsed, handled = matrixrtc.FromMatrixEvent(evt)
	}
	obs.M().MatrixRTCEvents.Add(ctx, 1, metric.WithAttributes(
		obs.AttrMatrixRTCKind.String(kind.String()),
		obs.AttrRouted.Bool(handled)))
	if !handled {
		return false
	}
	var opts []trace.SpanStartOption
	if obs.Enabled() {
		number := in.localE164(login)
		opts = []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithTimestamp(t0),
			trace.WithAttributes(
				obs.AttrMatrixRoomID.String(parsed.RoomID.String()),
				obs.AttrMatrixEventID.String(parsed.EventID.String()),
				obs.AttrMatrixEventType.String(parsed.Type),
				obs.AttrMatrixRTCKind.String(parsed.Kind.String()),
				obs.AttrMatrixSender.String(parsed.Sender.String()),
				obs.AttrCallLocalE164.String(number),
				attribute.Int64("matrixrtc.resolve_ms", time.Since(t0).Milliseconds()),
			),
		}
	}
	if obs.Enabled() {
		opts = append(opts, trace.WithAttributes(memberAttrs(parsed, t0)...))
	}
	sctx, span := obs.Start(ctx, "matrixrtc.ingest", opts...)
	defer span.End()
	in.mu.RLock()
	sink := in.sink
	in.mu.RUnlock()
	span.SetAttributes(attribute.String("matrixrtc.routed_to", "none")) // default; the sink may overwrite it
	sink.HandleRTCEvent(sctx, login, parsed)
	return true
}

// memberAttrs returns the MatrixRTC 2.0 span attributes of a member or slot event: the membership format
// (legacy|sticky), the slot id as received and the device id of the parsed membership. Attributes are
// only added when the event parses and the value is known; never any content.
func memberAttrs(evt matrixrtc.Event, now time.Time) []attribute.KeyValue {
	switch evt.Kind {
	case matrixrtc.KindCallMember, matrixrtc.KindRTCMember:
		m, ok := matrixrtc.ParseMembership(evt, now)
		if !ok {
			return nil
		}
		var attrs []attribute.KeyValue
		switch m.Format {
		case matrixrtc.FormatLegacy:
			attrs = append(attrs, obs.AttrMatrixRTCFormat.String("legacy"))
		case matrixrtc.FormatSticky:
			attrs = append(attrs, obs.AttrMatrixRTCFormat.String("sticky"))
		}
		if m.SlotID != "" {
			attrs = append(attrs, obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(m.SlotID)))
		}
		if m.DeviceID != "" {
			attrs = append(attrs, obs.AttrMatrixRTCDeviceID.String(m.DeviceID))
		}
		return attrs
	case matrixrtc.KindRTCSlot:
		if evt.StateKey != "" {
			return []attribute.KeyValue{obs.AttrMatrixRTCSlotID.String(matrixrtc.SlotIDLabel(evt.StateKey))}
		}
	}
	return nil
}
