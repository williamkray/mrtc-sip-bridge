package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type ctxKey struct{}

var (
	testTraceID = trace.TraceID{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 2}
	testSpanID  = trace.SpanID{0, 0, 0, 0, 0, 0, 1, 0}
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resetForTest() { Use(Providers{}) }

func useRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	Use(Providers{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))})
	t.Cleanup(resetForTest)
	return rec
}

func useReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	rd := sdkmetric.NewManualReader()
	Use(Providers{Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))})
	t.Cleanup(resetForTest)
	return rd
}

func collect(t *testing.T, rd *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func TestDisabledIdentity(t *testing.T) {
	resetForTest()
	if Enabled() || MetricsEnabled() {
		t.Fatal("must be disabled without Use")
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, 1)
	sc := sampledSC(testTraceID, testSpanID)
	checks := map[string]func() (context.Context, trace.Span){
		"Start":           func() (context.Context, trace.Span) { return Start(ctx, "x") },
		"StartChild":      func() (context.Context, trace.Span) { return StartChild(ctx, "x") },
		"StartLinkedRoot": func() (context.Context, trace.Span) { return StartLinkedRoot(ctx, "x", ctx) },
	}
	for name, f := range checks {
		c, s := f()
		if c != ctx || s.IsRecording() {
			t.Errorf("%s not identity/no-op", name)
		}
	}
	if CarrySpan(ctx, ctx) != ctx || WithParent(ctx, sc) != ctx {
		t.Error("ctx helpers not identity")
	}
	Fail(nil, errors.New("x")) // must not panic
	base := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	w := WrapHTTPTransport(base, nil)
	req, _ := http.NewRequest("GET", "http://x/", nil)
	if _, err := w.RoundTrip(req); err != nil || req.Header.Get("traceparent") != "" {
		t.Error("transport should pass through")
	}
	// instruments are usable no-ops
	in := M()
	if in == nil || in.RegisterAttempts == nil || in.Registrations == nil || in.HTTPClientDuration == nil {
		t.Fatal("registry must be fully populated when off")
	}
	in.RegisterAttempts.Add(ctx, 1)
	in.HTTPClientDuration.Record(ctx, 1)
	// Phase 2 instruments are non-nil and usable as no-ops.
	in.CallStarted.Add(ctx, 1)
	in.CallEnded.Add(ctx, 1)
	in.CallActive.Add(ctx, 1)
	in.CallSetupDuration.Record(ctx, 1)
	in.CallRingDuration.Record(ctx, 1)
	in.CallDuration.Record(ctx, 1)
	in.RTCJoinDuration.Record(ctx, 1)
	in.RTPPackets.Add(ctx, 1)
	in.RTPLost.Add(ctx, 1)
	in.RTPLate.Add(ctx, 1)
	in.RTPJitter.Record(ctx, 1)
	in.MediaConcealed.Add(ctx, 1)
	in.DTMFSent.Add(ctx, 1, metric.WithAttributes(AttrDTMFMethod.String("rfc4733")))
}

func TestUseNilHalves(t *testing.T) {
	useRecorder(t)
	if !Enabled() || MetricsEnabled() {
		t.Fatal("tracer-only: tracing on, metrics off")
	}
	rd := useReader(t)
	if Enabled() || !MetricsEnabled() {
		t.Fatal("meter-only: tracing off, metrics on")
	}
	M().Logins.Add(context.Background(), 1)
	if _, ok := collect(t, rd)["mrtcsip.bridge.logins"]; !ok {
		t.Fatal("logins counter missing")
	}
}

func TestInstrumentNames(t *testing.T) {
	rd := useReader(t)
	ctx := context.Background()
	in := M()
	in.RegisterAttempts.Add(ctx, 1)
	in.RegisterDuration.Record(ctx, 0.1)
	in.MatrixRTCEvents.Add(ctx, 1)
	in.Logins.Add(ctx, 1)
	in.HTTPClientDuration.Record(ctx, 0.1)
	if _, err := in.Meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(in.Registrations, 2, metric.WithAttributes(AttrState.String("registered")))
		return nil
	}, in.Registrations); err != nil {
		t.Fatal(err)
	}
	got := collect(t, rd)
	for _, n := range []string{
		"mrtcsip.sip.register.attempts", "mrtcsip.sip.register.duration", "mrtcsip.sip.registrations",
		"mrtcsip.matrixrtc.events", "mrtcsip.bridge.logins", "http.client.request.duration",
	} {
		if _, ok := got[n]; !ok {
			t.Errorf("instrument %s missing", n)
		}
	}
}

func TestPhase2InstrumentNames(t *testing.T) {
	rd := useReader(t)
	ctx := context.Background()
	in := M()
	in.CallStarted.Add(ctx, 1)
	in.CallEnded.Add(ctx, 1)
	in.CallActive.Add(ctx, 1)
	in.CallSetupDuration.Record(ctx, 1)
	in.CallRingDuration.Record(ctx, 1)
	in.CallDuration.Record(ctx, 1)
	in.RTCJoinDuration.Record(ctx, 1)
	in.RTPPackets.Add(ctx, 1)
	in.RTPLost.Add(ctx, 1)
	in.RTPLate.Add(ctx, 1)
	in.RTPJitter.Record(ctx, 0.01)
	in.MediaConcealed.Add(ctx, 1)
	in.DTMFSent.Add(ctx, 1, metric.WithAttributes(AttrDTMFMethod.String("rfc4733")))
	in.DelayedLeaveRestarts.Add(ctx, 1, metric.WithAttributes(AttrOutcome.String("ok")))
	got := collect(t, rd)
	for _, n := range []string{
		"mrtcsip.call.started", "mrtcsip.call.ended", "mrtcsip.call.active", "mrtcsip.call.setup.duration",
		"mrtcsip.call.ring.duration", "mrtcsip.call.duration", "mrtcsip.rtc.join.duration",
		"mrtcsip.media.rtp.packets", "mrtcsip.media.rtp.lost", "mrtcsip.media.rtp.late",
		"mrtcsip.media.rtp.jitter", "mrtcsip.media.concealed", "mrtcsip.dtmf.sent",
		"mrtcsip.matrixrtc.delayed_leave.restarts",
	} {
		if _, ok := got[n]; !ok {
			t.Errorf("instrument %s missing", n)
		}
	}
}

func TestStartChild(t *testing.T) {
	rec := useRecorder(t)
	_, s := StartChild(context.Background(), "orphan")
	s.End()
	if len(rec.Ended()) != 0 {
		t.Fatal("orphan child must not record")
	}
	ctx, root := Start(context.Background(), "root")
	_, ch := StartChild(ctx, "child")
	ch.End()
	root.End()
	ended := rec.Ended()
	if len(ended) != 2 || ended[0].SpanContext().TraceID() != ended[1].SpanContext().TraceID() ||
		ended[0].Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatalf("bad child: %v", ended)
	}
}

func TestLinkedRootAndCarry(t *testing.T) {
	rec := useRecorder(t)
	origin, o := Start(context.WithValue(context.Background(), ctxKey{}, 1), "origin")
	base := context.Background()
	lctx, l := StartLinkedRoot(base, "linked", origin)
	l.End()
	if l.SpanContext().TraceID() == o.SpanContext().TraceID() {
		t.Error("linked root must be new trace")
	}
	c := CarrySpan(base, origin)
	if c.Value(ctxKey{}) != nil {
		t.Error("CarrySpan leaked ctx values")
	}
	_, cs := StartChild(c, "carried")
	cs.End()
	o.End()
	_ = lctx
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "linked":
			if len(s.Links()) != 1 || s.Links()[0].SpanContext.SpanID() != o.SpanContext().SpanID() {
				t.Error("missing link")
			}
		case "carried":
			if s.SpanContext().TraceID() != o.SpanContext().TraceID() {
				t.Error("carried span in wrong trace")
			}
		}
	}
	// no valid origin: no link
	_, l2 := StartLinkedRoot(base, "nolink", base)
	l2.End()
	for _, s := range rec.Ended() {
		if s.Name() == "nolink" && len(s.Links()) != 0 {
			t.Error("unexpected link")
		}
	}
}

func sampledSC(tid trace.TraceID, sid trace.SpanID) trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
}

func logKV(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", buf.String(), err)
	}
	return m
}

func keysOnce(t *testing.T, out string) {
	t.Helper()
	for _, k := range []string{"trace_id", "span_id", "dd.trace_id", "dd.span_id"} {
		if n := strings.Count(out, `"`+k+`"`); n != 1 {
			t.Errorf("key %s appears %d times: %s", k, n, out)
		}
	}
}

func TestLoggerDisabled(t *testing.T) {
	resetForTest()
	sc := sampledSC(testTraceID, testSpanID)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	var buf bytes.Buffer
	base := zerolog.New(&buf)
	if Logger(ctx, &base) != &base || LoggerSC(sc, &base) != &base {
		t.Fatal("disabled must return base")
	}
	if n := testing.AllocsPerRun(100, func() {
		_ = Logger(ctx, &base)
		_ = LoggerSC(sc, &base)
	}); n != 0 {
		t.Fatalf("allocs = %v", n)
	}
	lctx := base.WithContext(context.Background())
	nctx, _ := Start(lctx, "x")
	if zerolog.Ctx(nctx) != zerolog.Ctx(lctx) {
		t.Fatal("Start must not touch ctx logger when disabled")
	}
}

func TestLoggerFields(t *testing.T) {
	useRecorder(t)
	var buf bytes.Buffer
	base := zerolog.New(&buf)
	sc := sampledSC(testTraceID, testSpanID)
	LoggerSC(sc, &base).Info().Msg("x")
	keysOnce(t, buf.String())
	m := logKV(t, &buf)
	if m["trace_id"] != sc.TraceID().String() || m["span_id"] != sc.SpanID().String() ||
		m["dd.trace_id"] != "258" || m["dd.span_id"] != "256" {
		t.Fatalf("bad fields: %v", m)
	}
	buf.Reset()
	var ff trace.TraceID
	var fs trace.SpanID
	for i := range ff {
		ff[i] = 0xff
	}
	for i := range fs {
		fs[i] = 0xff
	}
	Logger(trace.ContextWithSpanContext(context.Background(), sampledSC(ff, fs)), &base).Info().Msg("x")
	m = logKV(t, &buf)
	if m["dd.trace_id"] != "18446744073709551615" || m["dd.span_id"] != "18446744073709551615" {
		t.Fatalf("high-bit: %v", m)
	}
	if base.GetLevel() == zerolog.Disabled {
		t.Fatal("base mutated")
	}
}

func TestLoggerUnsampledOrInvalid(t *testing.T) {
	useRecorder(t)
	var buf bytes.Buffer
	base := zerolog.New(&buf)
	unsampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: testTraceID, SpanID: testSpanID})
	if Logger(context.Background(), &base) != &base ||
		LoggerSC(trace.SpanContext{}, &base) != &base ||
		LoggerSC(unsampled, &base) != &base ||
		Logger(trace.ContextWithSpanContext(context.Background(), unsampled), &base) != &base {
		t.Fatal("expected base pointer")
	}
}

func TestStartAnnotatesRootsOnly(t *testing.T) {
	useRecorder(t)
	var buf bytes.Buffer
	ctx := zerolog.New(&buf).WithContext(context.Background())
	rctx, root := Start(ctx, "root")
	zerolog.Ctx(rctx).Info().Msg("hi")
	keysOnce(t, buf.String())
	m := logKV(t, &buf)
	if m["span_id"] != root.SpanContext().SpanID().String() || m["trace_id"] != root.SpanContext().TraceID().String() {
		t.Fatalf("bad ids: %v", m)
	}
	cctx, child := Start(rctx, "child")
	if zerolog.Ctx(cctx) != zerolog.Ctx(rctx) {
		t.Fatal("child must not re-annotate")
	}
	child.End()
	root.End()
}

func TestLinkedRootReannotates(t *testing.T) {
	useRecorder(t)
	var buf bytes.Buffer
	ctx := zerolog.New(&buf).WithContext(context.Background())
	rctx, root := Start(ctx, "root")
	lctx, l := StartLinkedRoot(rctx, "l", rctx)
	buf.Reset()
	zerolog.Ctx(lctx).Info().Msg("hi")
	keysOnce(t, buf.String())
	m := logKV(t, &buf)
	if m["trace_id"] != l.SpanContext().TraceID().String() || m["trace_id"] == root.SpanContext().TraceID().String() ||
		m["span_id"] != l.SpanContext().SpanID().String() {
		t.Fatalf("bad ids: %v", m)
	}
	l.End()
	root.End()
}

func TestStartSkipsDefaultAndDisabled(t *testing.T) {
	useRecorder(t)
	bg := context.Background()
	nctx, s := Start(bg, "r")
	if zerolog.Ctx(nctx) != zerolog.Ctx(bg) {
		t.Fatal("background root must not annotate")
	}
	s.End()
	var buf bytes.Buffer
	l := zerolog.New(&buf)
	old := zerolog.DefaultContextLogger
	zerolog.DefaultContextLogger = &l
	t.Cleanup(func() { zerolog.DefaultContextLogger = old })
	nctx, s = Start(bg, "r")
	if zerolog.Ctx(nctx) != &l {
		t.Fatal("default context logger must not be annotated")
	}
	s.End()
	zerolog.Ctx(nctx).Info().Msg("x")
	if strings.Contains(buf.String(), "trace_id") {
		t.Fatal("default logger polluted")
	}
}

func TestDerivedLoggerNotReannotated(t *testing.T) {
	useRecorder(t)
	var buf bytes.Buffer
	ctx := zerolog.New(&buf).WithContext(context.Background())
	rctx, root := Start(ctx, "root")
	dctx := zerolog.Ctx(rctx).With().Str("k", "v").Logger().WithContext(rctx)
	lctx, l := StartLinkedRoot(dctx, "l", dctx)
	buf.Reset()
	zerolog.Ctx(lctx).Info().Msg("hi")
	keysOnce(t, buf.String())
	l.End()
	root.End()
}

func TestLoggerOnAnnotatedCtxLogger(t *testing.T) {
	useRecorder(t)
	var buf bytes.Buffer
	ctx := zerolog.New(&buf).WithContext(context.Background())
	rctx, root := Start(ctx, "root")
	lg := Logger(rctx, zerolog.Ctx(rctx))
	if lg != zerolog.Ctx(rctx) {
		t.Fatal("expected same pointer")
	}
	lg.Info().Msg("x")
	keysOnce(t, buf.String())
	// a clean base gets enriched exactly once
	var b2 bytes.Buffer
	clean := zerolog.New(&b2)
	Logger(rctx, &clean).Info().Msg("x")
	keysOnce(t, b2.String())
	root.End()
}

func TestFail(t *testing.T) {
	rec := useRecorder(t)
	_, s := Start(context.Background(), "f")
	Fail(s, &urlErr{})
	Fail(s, nil)
	s.End()
	got := rec.Ended()[0]
	if got.Status().Code != 1 && got.Status().Code.String() != "Error" {
		t.Errorf("status: %v", got.Status())
	}
	if strings.Contains(got.Status().Description, "/secret") || !strings.Contains(got.Status().Description, "https://host.example") {
		t.Errorf("url not scrubbed: %q", got.Status().Description)
	}
	var typ string
	for _, kv := range got.Attributes() {
		if kv.Key == attribute.Key("error.type") {
			typ = kv.Value.AsString()
		}
	}
	if typ == "" {
		t.Error("error.type missing")
	}
	if ev := got.Events(); len(ev) != 1 || strings.Contains(ev[0].Attributes[0].Value.Emit(), "/secret") {
		t.Errorf("event not scrubbed: %v", ev)
	}
}

func TestScrub(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig-nature_1"
	for _, tc := range []struct{ in, want string }{
		{"download mxc://hs.example/AbCdSecret failed", "download mxc://hs.example failed"},
		{"GET https://u:p@host.example:8448/a/b?access_token=zzz x", "GET https://host.example:8448 x"},
		{"dial wss://lk.example/rtc?access_token=" + jwt + " refused", "dial wss://lk.example refused"},
		{"bad sip:alice:hunter2@sip.example.net;transport=udp", "bad sip:alice:***@sip.example.net;transport=udp"},
		{"bad sips:alice:hunter2@sip.example.net", "bad sip:alice:***@sip.example.net"},
		{"header Authorization: Bearer abc.def-123 end", "header Authorization: Bearer *** end"},
		{"header bearer   XyZ", "header Bearer ***"},
		{"token " + jwt + " leaked", "token <jwt> leaked"},
		{"query password=hunter2&x=1 and Access_Token=abc secret=s token=t", "query password=***&x=1 and Access_Token=*** secret=*** token=***"},
		{"nothing to see, +15551234567 and @a:hs ok", "nothing to see, +15551234567 and @a:hs ok"},
	} {
		if got := Scrub(tc.in); got != tc.want {
			t.Errorf("Scrub(%q)\n got  %q\n want %q", tc.in, got, tc.want)
		}
	}
}

func TestFailScrubsSecrets(t *testing.T) {
	rec := useRecorder(t)
	_, s := Start(context.Background(), "f")
	Fail(s, errors.New("auth failed sip:bob:pw123@reg.example Bearer tok123 password=pw123"))
	s.End()
	got := rec.Ended()[0]
	all := got.Status().Description
	for _, ev := range got.Events() {
		for _, kv := range ev.Attributes {
			all += kv.Value.Emit()
		}
	}
	for _, secret := range []string{"pw123", "tok123"} {
		if strings.Contains(all, secret) {
			t.Errorf("secret %q leaked: %q", secret, all)
		}
	}
}

func TestHTTPAttrs(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://hs.example/_matrix/x?token=secret", nil)
	attrs := HTTPAttrs(req)
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range attrs {
		m[kv.Key] = kv.Value
		if strings.Contains(kv.Value.Emit(), "secret") {
			t.Errorf("leaked: %v", kv)
		}
	}
	if m["server.address"].AsString() != "hs.example" || m["server.port"].AsInt64() != 443 ||
		m["http.request.method"].AsString() != "POST" || len(attrs) != 3 {
		t.Errorf("attrs: %v", attrs)
	}
}

func tmplFn(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.ContainsAny(p, "!@:$0123456789") {
			parts[i] = "*"
		}
	}
	return strings.Join(parts, "/")
}

func TestTransport(t *testing.T) {
	rec := useRecorder(t)
	var gotTP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTP = r.Header.Get("traceparent")
		w.WriteHeader(404)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()
	var baseBody io.ReadCloser
	rt := WrapHTTPTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if resp != nil {
			baseBody = resp.Body
		}
		return resp, err
	}), tmplFn)
	// no parent: child-only, no span and no header
	req, _ := http.NewRequest("GET", srv.URL+"/rooms/!a:b/send?q=secret", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotTP != "" || len(rec.Ended()) != 0 {
		t.Fatal("untraced request must be untouched")
	}
	// with parent
	ctx, root := Start(context.Background(), "root")
	req, _ = http.NewRequestWithContext(ctx, "GET", srv.URL+"/rooms/!a:b/send?q=secret", nil)
	req.Header.Set("X-Orig", "1")
	resp, err = rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	root.End()
	if resp.Body != baseBody {
		t.Error("response body was wrapped")
	}
	if req.Header.Get("traceparent") != "" || req.Header.Get("X-Orig") != "1" {
		t.Error("caller request mutated")
	}
	if !strings.Contains(gotTP, root.SpanContext().TraceID().String()) {
		t.Errorf("traceparent %q", gotTP)
	}
	var found bool
	for _, s := range rec.Ended() {
		if s.Name() == "GET /rooms/*/send" {
			found = true
			if s.SpanKind() != trace.SpanKindClient {
				t.Error("not a client span")
			}
			for _, kv := range s.Attributes() {
				if strings.Contains(kv.Value.Emit(), "secret") {
					t.Error("query leaked")
				}
			}
		}
	}
	if !found {
		t.Errorf("transport span missing: %v", rec.Ended())
	}
}

func TestTransportMetric(t *testing.T) {
	rd := useReader(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	rt := WrapHTTPTransport(nil, tmplFn)
	// metrics are recorded even without a traced ctx
	req, _ := http.NewRequest("GET", srv.URL+"/rooms/!a:b/send?q=secret", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if req.Header.Get("traceparent") != "" {
		t.Error("untraced request got traceparent")
	}
	m, ok := collect(t, rd)["http.client.request.duration"]
	if !ok {
		t.Fatal("duration histogram missing")
	}
	h := m.Data.(metricdata.Histogram[float64])
	if len(h.DataPoints) != 1 || h.DataPoints[0].Count != 1 {
		t.Fatalf("points: %+v", h.DataPoints)
	}
	got := map[string]string{}
	for _, kv := range h.DataPoints[0].Attributes.ToSlice() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["http.response.status_code"] != "503" || got["error.type"] != "503" ||
		got["url.template"] != "/rooms/*/send" || got["http.request.method"] != "GET" {
		t.Errorf("attrs: %v", got)
	}
	for _, v := range got {
		if strings.Contains(v, "secret") {
			t.Errorf("leaked: %v", got)
		}
	}
}

type urlErr struct{}

func (*urlErr) Error() string {
	return "PUT https://host.example/upload/secret?x=1 failed for user@example.com/res"
}

// TestAnnotateLogger checks that the exported AnnotateLogger has the root-annotation semantics of Start:
// fields once, rebuilt over an already annotated ctx, and a no-op for invalid/unsampled span contexts,
// disabled loggers and loggers derived from an annotated one.
func TestAnnotateLogger(t *testing.T) {
	var buf bytes.Buffer
	ctx := zerolog.New(&buf).WithContext(context.Background())
	sc := sampledSC(testTraceID, testSpanID)
	actx := AnnotateLogger(ctx, sc)
	zerolog.Ctx(actx).Info().Msg("hi")
	keysOnce(t, buf.String())
	if m := logKV(t, &buf); m["trace_id"] != testTraceID.String() || m["span_id"] != testSpanID.String() {
		t.Fatalf("bad ids: %v", m)
	}

	// A second annotation over the annotated ctx rebuilds from the clean logger (keys still once).
	other := sampledSC(trace.TraceID{9}, trace.SpanID{9})
	buf.Reset()
	zerolog.Ctx(AnnotateLogger(actx, other)).Info().Msg("again")
	keysOnce(t, buf.String())
	if m := logKV(t, &buf); m["trace_id"] != other.TraceID().String() {
		t.Fatalf("not rebuilt: %v", m)
	}

	// Derived from an annotated logger: unchanged.
	dctx := zerolog.Ctx(actx).With().Str("k", "v").Logger().WithContext(actx)
	if got := AnnotateLogger(dctx, other); got != dctx {
		t.Fatal("derived logger must not be re-annotated")
	}

	unsampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: testTraceID, SpanID: testSpanID})
	if AnnotateLogger(ctx, unsampled) != ctx || AnnotateLogger(ctx, trace.SpanContext{}) != ctx {
		t.Fatal("unsampled/invalid span context must be a no-op")
	}
	disabled := zerolog.New(&buf).Level(zerolog.Disabled).WithContext(context.Background())
	if AnnotateLogger(disabled, sc) != disabled {
		t.Fatal("disabled logger must be a no-op")
	}
	bg := context.Background()
	if AnnotateLogger(bg, sc) != bg {
		t.Fatal("default context logger must be a no-op")
	}
}
