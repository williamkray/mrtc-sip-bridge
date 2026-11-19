package hostkit

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// CallStackDeps are the host callbacks of the call manager.
type CallStackDeps struct {
	Host        core.CallHost
	Dial        func(login core.LoginRef, req sipua.DialRequest) (sipua.OutboundCall, error)
	LoginNumber func(login core.LoginRef) string
	Log         zerolog.Logger // plain bridge logger
}

// NewCallStack builds the lk-jwt HTTP client (obs.WrapHTTPTransport with rtc.PathTemplate, 10 s
// timeout), the LiveKit service URL resolver (with the background "LiveKit service URL" log line),
// rtc.SetLibraryLogger and the calls.Manager from cfg. The host then routes MatrixRTC events to the
// manager (core.Ingest.SetSink).
func NewCallStack(br *bridgev2.Bridge, cfg *Config, deps CallStackDeps) (*calls.Manager, *rtc.ServiceURLResolver) {
	rlog := deps.Log.With().Str("component", "rtc").Logger()
	rtc.SetLibraryLogger(deps.Log) // plain logger: it adds component=livekit itself
	hc := &http.Client{Timeout: 10 * time.Second, Transport: obs.WrapHTTPTransport(http.DefaultTransport, rtc.PathTemplate)}
	resolver := rtc.NewServiceURLResolver(cfg.LiveKitServiceURL, br.Matrix.ServerName(), hc, clock.Real())
	go func() {
		u, err := resolver.Get(context.Background())
		if err != nil {
			rlog.Warn().Err(err).Msg("LiveKit service URL is not available yet (will retry per call)")
			return
		}
		host := ""
		if pu, perr := url.Parse(u); perr == nil {
			host = pu.Host
		}
		rlog.Info().Str("host", host).Str("source", resolver.Source()).Msg("LiveKit service URL")
	}()
	// The transports resolver caches /rtc/transports for the whole bridge (1 h, failures 60 s); the
	// ghost capability cache (/versions) is shared by all ghosts in ghostintent_rtc.go.
	transports := rtc.NewTransportResolver(clock.Real())
	mgr := calls.NewManager(calls.Config{
		Host:               deps.Host,
		Joiner:             rtc.NewJoiner(rtc.Config{HTTPClient: hc, Log: rlog}),
		ServiceURL:         resolver.Get,
		RingTimeout:        cfg.RingTimeout(),
		RTPTimeout:         cfg.RTPTimeout(),
		DefaultCountryCode: cfg.DefaultCountryCode,

		Dial:                deps.Dial,
		LoginNumber:         deps.LoginNumber,
		DialTimeout:         cfg.DialTimeout(),
		OutboundTrigger:     cfg.OutboundTrigger,
		DialAllowedPrefixes: cfg.DialAllowedPrefixes,
		DialNumberFormat:    cfg.NumberFormat(),
		AllowSelfDial:       cfg.AllowSelfDial,
		MembershipFormat:    cfg.MatrixRTCFormat,
		LiveKitAuth:         cfg.LiveKitAuth,
		Transports:          transports.Get,
		Log:                 deps.Log, // plain logger: calls adds component=calls itself
	})
	return mgr, resolver
}
