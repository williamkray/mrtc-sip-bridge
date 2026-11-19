package core

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

func TestIngestSpan(t *testing.T) {
	env := obstest.Setup(t)
	var logBuf bytes.Buffer
	lg := zerolog.New(&logBuf)
	in := NewIngest(LogSink{Log: &lg})
	reg := &fakeRegistrar{}
	resolveOK := false
	RegisterMatrixRTCHandlers(reg, func(context.Context, *event.Event) (LoginRef, bool) {
		return "+15551234567@reg.example.com", resolveOK
	}, in)

	evt := knownEvent()
	evt.Content = event.Content{Raw: map[string]any{"secret": "hunter2-secret", "livekit_service_url": "https://lk.example.org/x"}}

	// Dropped by the resolver: no span, counted routed=false.
	reg.handlers[2](context.Background(), evt)
	if n := len(env.Spans()); n != 0 {
		t.Fatalf("dropped event created %d spans", n)
	}
	if got := env.Sum(t, "mrtcsip.matrixrtc.events", "routed", "false", "matrixrtc.kind", "rtc_notification"); got != 1 {
		t.Errorf("routed=false count = %v, want 1", got)
	}

	// Accepted: one CONSUMER root span with ids and no content.
	resolveOK = true
	reg.handlers[2](context.Background(), evt)
	spans := env.SpansByName("matrixrtc.ingest")
	if len(spans) != 1 {
		t.Fatalf("got %d matrixrtc.ingest spans", len(spans))
	}
	s := spans[0]
	if s.Parent().IsValid() {
		t.Error("matrixrtc.ingest must be a root without a host span")
	}
	if s.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("kind = %v", s.SpanKind())
	}
	if got := obstest.Attr(s, "matrix.room_id").AsString(); got != "!room:example.org" {
		t.Errorf("room_id = %q", got)
	}
	if got := obstest.Attr(s, "matrixrtc.kind").AsString(); got != "rtc_notification" {
		t.Errorf("kind attr = %q", got)
	}
	if got := obstest.Attr(s, "call.local.e164").AsString(); got != "+15551234567" {
		t.Errorf("call.local.e164 = %q", got)
	}
	if got := env.Sum(t, "mrtcsip.matrixrtc.events", "routed", "true"); got != 1 {
		t.Errorf("routed=true count = %v, want 1", got)
	}
	obstest.AssertNoSecrets(t, env.Rec, &logBuf, "hunter2-secret", "lk.example.org")
	obstest.AssertMetricAttrsAllowed(t, env.Reader, nil)

	// The sink line logged under the span carries the correlation fields exactly once.
	if n := bytes.Count(logBuf.Bytes(), []byte(`"trace_id"`)); n != 1 {
		t.Errorf("log line has %d trace_id fields, want 1: %s", n, logBuf.String())
	}
}

func TestIngestSpanChildOfHostSpan(t *testing.T) {
	env := obstest.Setup(t)
	in := NewIngest(&fakeSink{})
	ctx, host := obsStart(context.Background())
	if !in.HandleMatrixRTCEvent(ctx, "login1", knownEvent()) {
		t.Fatal("not handled")
	}
	host.End()
	s := env.SpanByName("matrixrtc.ingest")
	if s == nil || s.Parent().SpanID() != host.SpanContext().SpanID() {
		t.Fatalf("ingest span is not a child of the host span: %+v", s)
	}
	if len(env.Roots()) != 1 {
		t.Errorf("want only the host root, got %d roots", len(env.Roots()))
	}
	_ = matrixrtc.KindUnknown
}

func obsStart(ctx context.Context) (context.Context, trace.Span) {
	return obs.Start(ctx, "host.span")
}

type routingSink struct{}

func (routingSink) HandleRTCEvent(ctx context.Context, _ LoginRef, _ matrixrtc.Event) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("matrixrtc.routed_to", "call"))
}

func TestIngestSinkOverridesRoutedTo(t *testing.T) {
	env := obstest.Setup(t)
	in := NewIngest(routingSink{})
	if !in.HandleMatrixRTCEvent(context.Background(), "login1", knownEvent()) {
		t.Fatal("not handled")
	}
	s := env.SpanByName("matrixrtc.ingest")
	if s == nil {
		t.Fatal("no ingest span")
	}
	if got := obstest.Attr(s, "matrixrtc.routed_to").AsString(); got != "call" {
		t.Errorf("routed_to = %q, want call", got)
	}

	// Default stays "none" when the sink does not set it.
	in2 := NewIngest(&fakeSink{})
	in2.HandleMatrixRTCEvent(context.Background(), "login1", knownEvent())
	spans := env.SpansByName("matrixrtc.ingest")
	if got := obstest.Attr(spans[1], "matrixrtc.routed_to").AsString(); got != "none" {
		t.Errorf("default routed_to = %q, want none", got)
	}
}

// TestIngestSetLocalNumber checks that SetLocalNumber overrides call.local.e164 and that nil restores the
// standalone rule (LoginRef up to the first '@').
func TestIngestSetLocalNumber(t *testing.T) {
	env := obstest.Setup(t)
	in := NewIngest(&fakeSink{})
	var asked LoginRef
	in.SetLocalNumber(func(l LoginRef) string { asked = l; return "+15557654321" })
	if !in.HandleMatrixRTCEvent(context.Background(), "alice@example.org", knownEvent()) {
		t.Fatal("event not handled")
	}
	spans := env.SpansByName("matrixrtc.ingest")
	if len(spans) != 1 {
		t.Fatalf("got %d spans", len(spans))
	}
	if got := obstest.Attr(spans[0], "call.local.e164").AsString(); got != "+15557654321" || asked != "alice@example.org" {
		t.Errorf("call.local.e164 = %q (asked for %q)", got, asked)
	}

	in.SetLocalNumber(nil)
	if !in.HandleMatrixRTCEvent(context.Background(), "+15551234567@reg.example.com", knownEvent()) {
		t.Fatal("event not handled")
	}
	spans = env.SpansByName("matrixrtc.ingest")
	if len(spans) != 2 {
		t.Fatalf("got %d spans", len(spans))
	}
	if got := obstest.Attr(spans[1], "call.local.e164").AsString(); got != "+15551234567" {
		t.Errorf("nil SetLocalNumber: call.local.e164 = %q", got)
	}
}

func TestIngestSpanMembershipAttrs(t *testing.T) {
	env := obstest.Setup(t)
	in := NewIngest(&fakeSink{})
	ghost := id.UserID("@ghost:example.org")

	sticky := &event.Event{
		Type: matrixrtc.RTCMember, RoomID: "!room:example.org", ID: "$st", Sender: ghost,
		Content: event.Content{Raw: matrixrtc.StickyJoinContent(matrixrtc.StickyJoinParams{
			UserID: ghost, DeviceID: "DEV1", MemberID: "mem-1", SlotID: matrixrtc.SlotIDRoom})},
	}
	legacy := &event.Event{
		Type: matrixrtc.CallMember, RoomID: "!room:example.org", ID: "$lg", Sender: ghost,
		StateKey: ptr(matrixrtc.MemberStateKey(ghost, "DEV2")),
		Content: event.Content{Raw: matrixrtc.JoinContent(matrixrtc.JoinParams{
			UserID: ghost, DeviceID: "DEV2", RoomID: "!room:example.org", ServiceURL: "https://lk.example.org",
			CreatedTS: time.Now(), Expires: time.Hour})},
	}
	slot := &event.Event{
		Type: matrixrtc.RTCSlot, RoomID: "!room:example.org", ID: "$sl", Sender: "@owner:example.org",
		StateKey: ptr(matrixrtc.SlotIDRoom), Content: event.Content{Raw: matrixrtc.SlotOpenContent()},
	}
	for _, e := range []*event.Event{sticky, legacy, slot, knownEvent()} {
		if !in.HandleMatrixRTCEvent(context.Background(), "login1", e) {
			t.Fatalf("event %s not handled", e.ID)
		}
	}
	spans := env.SpansByName("matrixrtc.ingest")
	if len(spans) != 4 {
		t.Fatalf("%d spans", len(spans))
	}
	has := func(i int, key string) bool {
		for _, a := range spans[i].Attributes() {
			if string(a.Key) == key {
				return true
			}
		}
		return false
	}
	str := func(i int, key string) string { return obstest.Attr(spans[i], key).AsString() }

	if str(0, "matrixrtc.membership.format") != "sticky" || str(0, "matrixrtc.slot_id") != matrixrtc.SlotIDRoom ||
		str(0, "matrixrtc.membership.device_id") != "DEV1" {
		t.Errorf("sticky attrs wrong: %v", spans[0].Attributes())
	}
	if str(1, "matrixrtc.membership.format") != "legacy" || str(1, "matrixrtc.membership.device_id") != "DEV2" ||
		str(1, "matrixrtc.slot_id") != matrixrtc.SlotIDRoomLegacy {
		t.Errorf("legacy attrs wrong: %v", spans[1].Attributes())
	}
	if has(2, "matrixrtc.membership.format") || str(2, "matrixrtc.slot_id") != matrixrtc.SlotIDRoom {
		t.Errorf("slot attrs wrong: %v", spans[2].Attributes())
	}
	if has(3, "matrixrtc.membership.format") || has(3, "matrixrtc.slot_id") || has(3, "matrixrtc.membership.device_id") {
		t.Errorf("notification span has membership attrs: %v", spans[3].Attributes())
	}
	if str(2, "matrixrtc.kind") != "rtc_slot" || str(0, "matrixrtc.kind") != "rtc_member" {
		t.Errorf("kinds: %s %s", str(2, "matrixrtc.kind"), str(0, "matrixrtc.kind"))
	}
}

func ptr(s string) *string { return &s }

// The slot id of an event is sender-controlled: only the two room spellings are recorded, anything else
// is "other".
func TestMemberAttrsSlotIDIsClosedSet(t *testing.T) {
	slotAttr := func(evt matrixrtc.Event) string {
		for _, a := range memberAttrs(evt, time.Now()) {
			if string(a.Key) == "matrixrtc.slot_id" {
				return a.Value.AsString()
			}
		}
		return ""
	}
	for key, want := range map[string]string{matrixrtc.SlotIDRoom: matrixrtc.SlotIDRoom, "m.call#evil-1234": "other"} {
		if got := slotAttr(matrixrtc.Event{Kind: matrixrtc.KindRTCSlot, StateKey: key}); got != want {
			t.Errorf("slot event %q: attr %q, want %q", key, got, want)
		}
	}
	// A sticky membership with an unknown slot id still parses (not live); its attribute is "other".
	ghost := id.UserID("@ghost:example.org")
	raw := matrixrtc.StickyJoinContent(matrixrtc.StickyJoinParams{UserID: ghost, DeviceID: "D", MemberID: "m", SlotID: "m.call#evil-1234"})
	b, _ := json.Marshal(raw)
	evt := matrixrtc.Event{Kind: matrixrtc.KindRTCMember, Type: matrixrtc.RTCMember.Type, Sender: ghost, Content: b, Timestamp: time.Now()}
	if got := slotAttr(evt); got != "other" {
		t.Errorf("member slot attr %q", got)
	}
}
