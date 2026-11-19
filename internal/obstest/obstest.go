// Package obstest is test-only support for asserting on traces and metrics recorded through pkg/obs
// (TELEMETRY-SPEC §8.3, §8.4). It installs in-memory SDK providers with obs.Use and offers span-tree
// helpers plus the privacy and cardinality assertions. Only _test.go files should import it.
package obstest

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// Env is a pair of in-memory exporters installed through obs.Use.
type Env struct {
	Rec    *tracetest.SpanRecorder
	Reader *sdkmetric.ManualReader
	TP     *sdktrace.TracerProvider
	MP     *sdkmetric.MeterProvider
}

// Setup installs an in-memory TracerProvider and MeterProvider with obs.Use and resets obs
// (all off) in t.Cleanup.
func Setup(t testing.TB) *Env {
	t.Helper()
	e := &Env{Rec: tracetest.NewSpanRecorder(), Reader: sdkmetric.NewManualReader()}
	e.TP = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(e.Rec))
	e.MP = sdkmetric.NewMeterProvider(sdkmetric.WithReader(e.Reader))
	obs.Use(obs.Providers{Tracer: e.TP, Meter: e.MP})
	t.Cleanup(func() {
		obs.Use(obs.Providers{})
		_ = e.TP.Shutdown(context.Background())
		_ = e.MP.Shutdown(context.Background())
	})
	return e
}

// Spans returns every ended span in end order.
func (e *Env) Spans() []sdktrace.ReadOnlySpan { return e.Rec.Ended() }

// SpanByName returns the first ended span with the given name, or nil.
func (e *Env) SpanByName(name string) sdktrace.ReadOnlySpan {
	for _, s := range e.Spans() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// SpansByName returns every ended span with the given name.
func (e *Env) SpansByName(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range e.Spans() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// Roots returns the ended spans without a valid parent.
func (e *Env) Roots() []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range e.Spans() {
		if !s.Parent().IsValid() {
			out = append(out, s)
		}
	}
	return out
}

// Children returns the ended spans whose parent is span.
func (e *Env) Children(span sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range e.Spans() {
		if s.Parent().IsValid() && s.Parent().SpanID() == span.SpanContext().SpanID() {
			out = append(out, s)
		}
	}
	return out
}

// InTrace returns the ended spans that belong to span's trace (including span).
func (e *Env) InTrace(span sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range e.Spans() {
		if s.SpanContext().TraceID() == span.SpanContext().TraceID() {
			out = append(out, s)
		}
	}
	return out
}

// Links returns the links of span.
func (e *Env) Links(span sdktrace.ReadOnlySpan) []sdktrace.Link { return span.Links() }

// Attr returns the value of attribute key on span (Valid()==false when absent).
func Attr(span sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

// Point is one metric data point reduced to attributes and a number: the value for sums and gauges,
// the observation count for histograms.
type Point struct {
	Attrs attribute.Set
	Value float64
}

// Get returns the value of attribute key as a string ("" when absent).
func (p Point) Get(key string) string {
	v, ok := p.Attrs.Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return v.Emit()
}

// Collect reads the manual reader.
func (e *Env) Collect(t testing.TB) metricdata.ResourceMetrics {
	t.Helper()
	return Collect(t, e.Reader)
}

// Collect reads rd.
func Collect(t testing.TB, rd *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return rm
}

// MetricPoints returns the data points of the named metric (nil if it has none).
func (e *Env) MetricPoints(t testing.TB, name string) []Point {
	t.Helper()
	var out []Point
	for _, sm := range e.Collect(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{dp.Attributes, float64(dp.Value)})
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{dp.Attributes, dp.Value})
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{dp.Attributes, float64(dp.Value)})
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, Point{dp.Attributes, float64(dp.Count)})
				}
			}
		}
	}
	return out
}

// Sum adds up the values of the points of metric name whose attributes include every want pair
// (key, value, key, value, ...).
func (e *Env) Sum(t testing.TB, name string, want ...string) float64 {
	t.Helper()
	var total float64
next:
	for _, p := range e.MetricPoints(t, name) {
		for i := 0; i+1 < len(want); i += 2 {
			if p.Get(want[i]) != want[i+1] {
				continue next
			}
		}
		total += p.Value
	}
	return total
}

// AssertNoSecrets walks every ended span in rec (name, attributes, events and their attributes, status
// description, links' attributes) and the captured log output, and fails if any secret appears.
// logBuf may be nil.
func AssertNoSecrets(t testing.TB, rec *tracetest.SpanRecorder, logBuf *bytes.Buffer, secrets ...string) {
	t.Helper()
	check := func(where, s string) {
		for _, sec := range secrets {
			if sec != "" && strings.Contains(s, sec) {
				t.Errorf("secret %q found in %s: %q", sec, where, s)
			}
		}
	}
	attrs := func(where string, kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			check(where+" attribute key", string(kv.Key))
			check(where+" attribute "+string(kv.Key), kv.Value.Emit())
		}
	}
	for _, s := range rec.Ended() {
		w := "span " + s.Name()
		check(w+" name", s.Name())
		attrs(w, s.Attributes())
		check(w+" status", s.Status().Description)
		for _, ev := range s.Events() {
			check(w+" event name", ev.Name)
			attrs(w+" event "+ev.Name, ev.Attributes)
		}
		for _, l := range s.Links() {
			attrs(w+" link", l.Attributes)
		}
	}
	if logBuf != nil {
		check("log output", logBuf.String())
	}
}

// MetricAllowlist maps metric name to allowed attribute keys; each key maps to its closed value set.
// A single value "*" means free-form (bounded by deployment, for example server.address) and skips all
// checks. A single value "~" means free-form but identity-checked: the enum membership check is skipped
// and the identity-pattern check still runs.
type MetricAllowlist map[string]map[string][]string

// callDirections and callOutcomes are the closed enums shared by the Phase 2 call instruments.
var (
	callDirections = []string{"inbound", "outbound"}
	callOutcomes   = []string{"answered", "missed", "declined", "cancelled", "timeout", "busy", "failed", "rejected"}
)

// DefaultMetricAllowlist is the TELEMETRY-SPEC §4.3 allowlist for the T6 and Phase 2 instruments.
var DefaultMetricAllowlist = MetricAllowlist{
	"mrtcsip.sip.register.attempts": {
		"sip.register.kind": {"full", "refresh", "unregister"},
		"outcome":           {"ok", "auth", "transient"},
	},
	"mrtcsip.sip.register.duration": {
		"sip.register.kind": {"full", "refresh", "unregister"},
		"outcome":           {"ok", "auth", "transient"},
	},
	"mrtcsip.sip.registrations": {
		"state": {"registering", "registered", "auth_failed", "transient_error"},
	},
	"mrtcsip.matrixrtc.events": {
		"matrixrtc.kind": {"call_member", "rtc_notification", "rtc_decline", "rtc_member", "rtc_slot", "unknown"},
		"routed":         {"true", "false"},
	},
	"mrtcsip.bridge.logins": {
		"outcome": {"ok", "auth_failed", "transient_error", "timeout", "error"},
	},
	"mrtcsip.call.started": {"call.direction": callDirections},
	"mrtcsip.call.ended": {
		"call.direction":            callDirections,
		"call.outcome":              callOutcomes,
		"sip.response.status_class": {"1xx", "2xx", "3xx", "4xx", "5xx", "6xx", "none"},
	},
	"mrtcsip.call.active": {"call.direction": callDirections},
	"mrtcsip.call.setup.duration": {
		"call.direction": callDirections,
		"call.outcome":   callOutcomes,
	},
	"mrtcsip.call.ring.duration": {
		"call.direction": callDirections,
		"call.outcome":   callOutcomes,
	},
	"mrtcsip.call.duration": {
		"call.direction": callDirections,
		"media.codec":    {"PCMU", "PCMA", "other"},
	},
	"mrtcsip.matrixrtc.delayed_leave.restarts": {
		"outcome": {"ok", "gone", "error"},
	},
	"mrtcsip.rtc.join.duration": {
		"outcome": {"ok", "openid_error", "token_error", "connect_error", "publish_error", "timeout"},
	},
	"mrtcsip.media.rtp.packets": {"media.direction": {"rx", "tx"}},
	"mrtcsip.media.rtp.lost":    {},
	"mrtcsip.media.rtp.late":    {},
	"mrtcsip.media.rtp.jitter":  {},
	"mrtcsip.media.concealed":   {"media.leg": {"sip_to_rtc", "rtc_to_sip"}},
	"mrtcsip.dtmf.sent":         {"dtmf.method": {"rfc4733"}},
	"mrtcsip.widget.requests": {
		"widget.action": {"session", "state", "normalize", "call", "dtmf", "hangup", "prefs", "chat", "contact", "contacts", "enroll", "device_session", "device_confirm", "forget"},
		"outcome":       {"ok", "unauthorized", "forbidden", "rate_limited", "invalid", "busy", "not_ready", "error", "expired", "used", "reuse"},
		"widget.client": {"widget", "pwa"},
	},
	"mrtcsip.widget.sessions":     {},
	"mrtcsip.widget.device_links": {"state": {"pending", "active"}},
	"mrtcsip.widget.device_events": {"event": {"issued", "enrolled", "rotated", "grace_used", "reuse_revoked", "revoked",
		"expired", "orphan_purged", "notice_redacted"}},
	"mrtcsip.widget.installs": {"outcome": {"installed", "updated", "unchanged", "removed", "failed"}},
	"http.client.request.duration": {
		"http.request.method":       {"~"},
		"http.response.status_code": {"*"},
		"server.address":            {"*"},
		"url.template":              {"~"},
		"error.type":                {"~"},
	},
}

var (
	reE164 = regexp.MustCompile(`\+?\d{7,15}`)
	reMXID = regexp.MustCompile(`[@!$#][^\s:]+:[^\s]+`)
)

// looksLikeIdentity reports whether v resembles an E.164 number, MXID, room/event id or IP address.
func looksLikeIdentity(v string) bool {
	if reE164.MatchString(v) || reMXID.MatchString(v) {
		return true
	}
	if _, err := netip.ParseAddr(v); err == nil {
		return true
	}
	if _, err := netip.ParseAddrPort(v); err == nil {
		return true
	}
	return false
}

// AssertMetricAttrsAllowed fails if any data point carries an attribute key that is not allowlisted for
// its metric, a value outside the closed set, or any value that looks like an E.164 number, MXID, room
// id or IP address. A nil allow uses DefaultMetricAllowlist. Metrics missing from the allowlist fail.
func AssertMetricAttrsAllowed(t testing.TB, rd *sdkmetric.ManualReader, allow MetricAllowlist) {
	t.Helper()
	if allow == nil {
		allow = DefaultMetricAllowlist
	}
	rm := Collect(t, rd)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			keys, ok := allow[m.Name]
			if !ok {
				t.Errorf("metric %q is not in the allowlist", m.Name)
				continue
			}
			for _, set := range attrSets(m.Data) {
				for _, kv := range set.ToSlice() {
					vals, ok := keys[string(kv.Key)]
					if !ok {
						t.Errorf("metric %q: attribute key %q not allowed", m.Name, kv.Key)
						continue
					}
					v := kv.Value.Emit()
					if len(vals) == 1 && vals[0] == "*" {
						continue
					}
					if looksLikeIdentity(v) {
						t.Errorf("metric %q attribute %q value %q looks like an identity", m.Name, kv.Key, v)
					}
					if len(vals) == 1 && vals[0] == "~" {
						continue
					}
					if !contains(vals, v) {
						t.Errorf("metric %q attribute %q value %q not in %v", m.Name, kv.Key, v, vals)
					}
				}
			}
		}
	}
}

func contains(vals []string, v string) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}

func attrSets(d metricdata.Aggregation) []attribute.Set {
	var out []attribute.Set
	switch d := d.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	default:
		panic(fmt.Sprintf("obstest: unsupported aggregation %T", d))
	}
	return out
}
