package hostkit

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// Config is the call/SIP host config: the standalone `network:` section, and jabber's `network.calls`
// minus `enabled`. Field names are YAML keys.
type Config struct {
	SIPBindIP              string `yaml:"sip_bind_ip"`
	SIPPortMin             int    `yaml:"sip_port_min"`
	SIPPortMax             int    `yaml:"sip_port_max"`
	ExternalIP             string `yaml:"external_ip"`
	STUNServer             string `yaml:"stun_server"`
	RTPPortMin             int    `yaml:"rtp_port_min"`
	RTPPortMax             int    `yaml:"rtp_port_max"`
	RegisterExpirySeconds  int    `yaml:"register_expiry_seconds"`
	RegisterRefreshSeconds int    `yaml:"register_refresh_seconds"`
	RingTimeoutSeconds     int    `yaml:"ring_timeout_seconds"`
	RTPTimeoutSeconds      int    `yaml:"rtp_timeout_seconds"`
	LiveKitServiceURL      string `yaml:"livekit_service_url"`
	DefaultCountryCode     string `yaml:"default_country_code"`

	// MatrixRTC 2.0 (docs/MATRIXRTC2-SPEC.md): membership format "auto" | "legacy" | "sticky" | "dual"
	// and LiveKit token auth "auto" | "sfu_get" | "msc4195". Empty = "auto".
	MatrixRTCFormat string `yaml:"matrixrtc_format"`
	LiveKitAuth     string `yaml:"livekit_auth"`

	// Outbound calls (Phase 3).
	OutboundTrigger     string   `yaml:"outbound_trigger"`
	DialTimeoutSeconds  int      `yaml:"dial_timeout_seconds"`
	DialAllowedPrefixes []string `yaml:"dial_allowed_prefixes"` // nil = default ["+1"]; explicit [] = any number
	DialNumberFormat    string   `yaml:"dial_number_format"`
	AllowSelfDial       bool     `yaml:"allow_self_dial"`
	DTMFRedactCommand   *bool    `yaml:"dtmf_redact_command"` // nil = true

	// Dialpad widget (docs/WIDGET-SPEC.md §4).
	Widget WidgetConfig `yaml:"widget"`
}

// WidgetConfig is the shared `widget` block (network.widget standalone, network.calls.widget in jabber).
type WidgetConfig struct {
	Enabled            bool     `yaml:"enabled"`
	PublicURL          string   `yaml:"public_url"`
	Secret             string   `yaml:"secret"` // "generate" is replaced by the upgrader
	FrameAncestors     []string `yaml:"frame_ancestors"`
	AllowRemoteUsers   bool     `yaml:"allow_remote_users"`
	RemoveWhenDisabled bool     `yaml:"remove_when_disabled"`
	// TrustedProxies lists CIDRs (or bare IPs) of reverse proxies whose last X-Forwarded-For hop is
	// believed. Empty: believed only from private and loopback peers.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// PWA is the installable phone app (docs/PWA-SPEC.md §8.1).
	PWA PWAConfig `yaml:"pwa"`
}

// PWAConfig is the `widget.pwa` block.
type PWAConfig struct {
	Enabled              bool  `yaml:"enabled"`
	InactivityDays       int   `yaml:"inactivity_days"`        // default 90
	MaxLifetimeDays      int   `yaml:"max_lifetime_days"`      // 0 = none
	RotationGraceSeconds int   `yaml:"rotation_grace_seconds"` // default 120
	EnrollMinutes        int   `yaml:"enroll_minutes"`         // default 15
	MaxLinksPerUser      int   `yaml:"max_links_per_user"`     // default 5
	NotifyOnEnroll       *bool `yaml:"notify_on_enroll"`       // nil = true
}

// NotifyOnEnrollEnabled reports NotifyOnEnroll (default true).
func (p *PWAConfig) NotifyOnEnrollEnabled() bool { return p.NotifyOnEnroll == nil || *p.NotifyOnEnroll }

const (
	defaultPWAInactivityDays = 90
	defaultPWAGraceSeconds   = 120
	defaultPWAEnrollMinutes  = 15
	defaultPWAMaxLinks       = 5
)

// WidgetSecretGenerate is the Secret value the upgrader replaces with a random secret.
const WidgetSecretGenerate = "generate"

const minWidgetSecretLen = 32

const (
	defaultSIPPortMin             = 5060
	defaultSIPPortMax             = 5069
	defaultRTPPortMin             = 40000
	defaultRTPPortMax             = 40100
	defaultRegisterExpirySeconds  = 60
	defaultRegisterRefreshSeconds = 25
	defaultRingTimeoutSeconds     = 30
	defaultRTPTimeoutSeconds      = 60
	defaultCountryCode            = "1"
	defaultOutboundTrigger        = "ring"
	defaultMatrixRTCFormat        = "auto"
	defaultLiveKitAuth            = "auto"
	defaultDialTimeoutSeconds     = 60
	defaultDialNumberFormat       = "digits"
	defaultSTUNServer             = "stun.l.google.com:19302" // only used in the example YAML
)

// ApplyDefaults fills zero-valued ints and an empty DefaultCountryCode with the defaults above.
// Strings that may legitimately be empty (SIPBindIP, ExternalIP, STUNServer, LiveKitServiceURL) are left alone.
func (c *Config) ApplyDefaults() {
	setInt := func(v *int, def int) {
		if *v == 0 {
			*v = def
		}
	}
	setInt(&c.SIPPortMin, defaultSIPPortMin)
	setInt(&c.SIPPortMax, defaultSIPPortMax)
	setInt(&c.RTPPortMin, defaultRTPPortMin)
	setInt(&c.RTPPortMax, defaultRTPPortMax)
	setInt(&c.RegisterExpirySeconds, defaultRegisterExpirySeconds)
	setInt(&c.RegisterRefreshSeconds, defaultRegisterRefreshSeconds)
	setInt(&c.RingTimeoutSeconds, defaultRingTimeoutSeconds)
	setInt(&c.RTPTimeoutSeconds, defaultRTPTimeoutSeconds)
	if c.DefaultCountryCode == "" {
		c.DefaultCountryCode = defaultCountryCode
	}
	setInt(&c.DialTimeoutSeconds, defaultDialTimeoutSeconds)
	if c.MatrixRTCFormat == "" {
		c.MatrixRTCFormat = defaultMatrixRTCFormat
	}
	if c.LiveKitAuth == "" {
		c.LiveKitAuth = defaultLiveKitAuth
	}
	if c.OutboundTrigger == "" {
		c.OutboundTrigger = defaultOutboundTrigger
	}
	if c.DialNumberFormat == "" {
		c.DialNumberFormat = defaultDialNumberFormat
	}
	if c.DialAllowedPrefixes == nil {
		c.DialAllowedPrefixes = []string{"+1"}
	}
	if c.DTMFRedactCommand == nil {
		t := true
		c.DTMFRedactCommand = &t
	}
	if c.Widget.FrameAncestors == nil {
		c.Widget.FrameAncestors = []string{"*"}
	}
	setInt(&c.Widget.PWA.InactivityDays, defaultPWAInactivityDays)
	setInt(&c.Widget.PWA.RotationGraceSeconds, defaultPWAGraceSeconds)
	setInt(&c.Widget.PWA.EnrollMinutes, defaultPWAEnrollMinutes)
	setInt(&c.Widget.PWA.MaxLinksPerUser, defaultPWAMaxLinks)
	if c.Widget.PWA.NotifyOnEnroll == nil {
		t := true
		c.Widget.PWA.NotifyOnEnroll = &t
	}
}

var dialPrefixRE = regexp.MustCompile(`^\+[1-9][0-9]{0,14}$`)

func validPort(p int) bool { return p >= 1 && p <= 65535 }

func validateRange(prefix, minKey, maxKey string, lo, hi int) error {
	if !validPort(lo) {
		return fmt.Errorf("%s%s: %d is not a valid port (1..65535)", prefix, minKey, lo)
	}
	if !validPort(hi) {
		return fmt.Errorf("%s%s: %d is not a valid port (1..65535)", prefix, maxKey, hi)
	}
	if lo > hi {
		return fmt.Errorf("%s%s: %d is greater than %s (%d)", prefix, minKey, lo, maxKey, hi)
	}
	return nil
}

func parseIPv4(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return netip.Addr{}, false
	}
	return a, true
}

// Validate returns the first error found, naming the YAML key ("<prefix><key>: ..."). prefix is the
// key path of the section: "network." for the standalone bridge, "network.calls." for jabber.
func (c *Config) Validate(prefix string) error {
	if c.SIPBindIP != "" {
		if _, ok := parseIPv4(c.SIPBindIP); !ok {
			return fmt.Errorf("%ssip_bind_ip: %q is not an IPv4 address", prefix, c.SIPBindIP)
		}
	}
	if err := validateRange(prefix, "sip_port_min", "sip_port_max", c.SIPPortMin, c.SIPPortMax); err != nil {
		return err
	}
	if err := validateRange(prefix, "rtp_port_min", "rtp_port_max", c.RTPPortMin, c.RTPPortMax); err != nil {
		return err
	}
	if c.RTPPortMax-c.RTPPortMin < 1 {
		return fmt.Errorf("%srtp_port_max: rtp range must contain at least 2 ports", prefix)
	}
	if c.SIPPortMin <= c.RTPPortMax && c.RTPPortMin <= c.SIPPortMax {
		return fmt.Errorf("%ssip_port_min: SIP port range %d-%d overlaps RTP port range %d-%d (rtp_port_min/rtp_port_max)", prefix,
			c.SIPPortMin, c.SIPPortMax, c.RTPPortMin, c.RTPPortMax)
	}
	if c.ExternalIP != "" {
		a, ok := parseIPv4(c.ExternalIP)
		if !ok || a.IsUnspecified() {
			return fmt.Errorf("%sexternal_ip: %q is not a usable IPv4 address", prefix, c.ExternalIP)
		}
	}
	if c.STUNServer != "" {
		_, port, err := net.SplitHostPort(c.STUNServer)
		if err != nil {
			return fmt.Errorf("%sstun_server: %q is not host:port: %w", prefix, c.STUNServer, err)
		}
		if _, err = strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("%sstun_server: port %q is not numeric", prefix, port)
		}
	}
	if c.RegisterExpirySeconds > 3600 {
		return fmt.Errorf("%sregister_expiry_seconds: %d exceeds 3600", prefix, c.RegisterExpirySeconds)
	}
	if c.RegisterRefreshSeconds < 5 {
		return fmt.Errorf("%sregister_refresh_seconds: %d is below 5", prefix, c.RegisterRefreshSeconds)
	}
	if c.RegisterRefreshSeconds >= c.RegisterExpirySeconds {
		return fmt.Errorf("%sregister_refresh_seconds: %d must be less than register_expiry_seconds (%d)", prefix,
			c.RegisterRefreshSeconds, c.RegisterExpirySeconds)
	}
	if c.RingTimeoutSeconds < 5 || c.RingTimeoutSeconds > 300 {
		return fmt.Errorf("%sring_timeout_seconds: %d is outside 5..300", prefix, c.RingTimeoutSeconds)
	}
	if c.RTPTimeoutSeconds < 10 || c.RTPTimeoutSeconds > 3600 {
		return fmt.Errorf("%srtp_timeout_seconds: %d is outside 10..3600", prefix, c.RTPTimeoutSeconds)
	}
	if c.LiveKitServiceURL != "" {
		u, err := url.Parse(c.LiveKitServiceURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%slivekit_service_url: %q is not an absolute http(s) URL", prefix, c.LiveKitServiceURL)
		}
	}
	switch c.MatrixRTCFormat {
	case "auto", "legacy", "sticky", "dual":
	default:
		return fmt.Errorf("%smatrixrtc_format: %q must be auto, legacy, sticky or dual", prefix, c.MatrixRTCFormat)
	}
	switch c.LiveKitAuth {
	case "auto", "sfu_get", "msc4195":
	default:
		return fmt.Errorf("%slivekit_auth: %q must be auto, sfu_get or msc4195", prefix, c.LiveKitAuth)
	}
	cc := c.DefaultCountryCode
	if len(cc) < 1 || len(cc) > 3 || cc[0] == '0' {
		return fmt.Errorf("%sdefault_country_code: %q must be 1-3 digits and not start with 0", prefix, cc)
	}
	for _, r := range cc {
		if r < '0' || r > '9' {
			return fmt.Errorf("%sdefault_country_code: %q must be 1-3 digits and not start with 0", prefix, cc)
		}
	}
	switch c.OutboundTrigger {
	case "ring", "membership", "off":
	default:
		return fmt.Errorf("%soutbound_trigger: %q must be ring, membership or off", prefix, c.OutboundTrigger)
	}
	if c.DialTimeoutSeconds < 10 || c.DialTimeoutSeconds > 300 {
		return fmt.Errorf("%sdial_timeout_seconds: %d is outside 10..300", prefix, c.DialTimeoutSeconds)
	}
	for _, p := range c.DialAllowedPrefixes {
		if !dialPrefixRE.MatchString(p) {
			return fmt.Errorf("%sdial_allowed_prefixes: %q is not an E.164 prefix like +1 or +44", prefix, p)
		}
	}
	switch c.DialNumberFormat {
	case "digits", "e164":
	default:
		return fmt.Errorf("%sdial_number_format: %q must be digits or e164", prefix, c.DialNumberFormat)
	}
	return c.Widget.Validate(prefix + "widget.")
}

var frameSourceRE = regexp.MustCompile(`^([a-z][a-z0-9+.-]*://)?(\*\.)?[A-Za-z0-9.-]+(:([0-9]+|\*))?$`)

// Validate checks the widget block (only when Enabled). prefix is the key path of the block, e.g.
// "network.widget.". Run it after the upgrader has replaced a "generate" secret.
func (w *WidgetConfig) Validate(prefix string) error {
	if !w.Enabled {
		if w.PWA.Enabled {
			return fmt.Errorf("%spwa.enabled: requires %senabled: true", prefix, prefix)
		}
		return nil
	}
	u, err := url.Parse(w.PublicURL)
	switch {
	case err != nil || w.PublicURL == "" || !u.IsAbs() || u.Host == "" || u.Opaque != "":
		return fmt.Errorf("%spublic_url: %q is not an absolute https URL", prefix, w.PublicURL)
	case u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")):
		return fmt.Errorf("%spublic_url: %q must use https (http only for localhost)", prefix, w.PublicURL)
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(w.PublicURL, "?#"):
		return fmt.Errorf("%spublic_url: %q must not have a query or fragment", prefix, w.PublicURL)
	case u.User != nil:
		return fmt.Errorf("%spublic_url: %q must not contain credentials", prefix, w.PublicURL)
	}
	p := u.EscapedPath()
	switch {
	case !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, "/") || p == "/" || strings.HasPrefix(p, "/_matrix/") ||
		strings.Contains(p, "//") || strings.Contains(p, "/../") || strings.Contains(p, "/./") || u.Path != path.Clean(u.Path)+"/":
		return fmt.Errorf("%spublic_url: path %q must start and end with /, must not be / or start with /_matrix/, and must be a plain path", prefix, p)
	}
	if len(w.Secret) < minWidgetSecretLen {
		return fmt.Errorf("%ssecret: must be at least %d characters (use generate)", prefix, minWidgetSecretLen)
	}
	for _, a := range w.FrameAncestors {
		if a == "*" || a == "'self'" {
			continue
		}
		if strings.ContainsAny(a, " ;,'\"\t\r\n") || !frameSourceRE.MatchString(a) {
			return fmt.Errorf("%sframe_ancestors: %q is not *, 'self' or a scheme/host source", prefix, a)
		}
	}
	if _, err := w.ParseTrustedProxies(); err != nil {
		return fmt.Errorf("%strusted_proxies: %w", prefix, err)
	}
	// An explicit default port would make the Origin check (browsers omit it) reject every request.
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		return fmt.Errorf("%spublic_url: %q must not spell out the default port %s", prefix, w.PublicURL, u.Port())
	}
	return w.PWA.Validate(prefix + "pwa.")
}

// Validate checks the PWA block (only when Enabled; defaults must be applied first).
func (p *PWAConfig) Validate(prefix string) error {
	if !p.Enabled {
		return nil
	}
	switch {
	case p.InactivityDays < 1 || p.InactivityDays > 3650:
		return fmt.Errorf("%sinactivity_days: %d must be 1..3650", prefix, p.InactivityDays)
	case p.MaxLifetimeDays != 0 && p.MaxLifetimeDays < p.InactivityDays:
		return fmt.Errorf("%smax_lifetime_days: %d must be 0 or at least inactivity_days (%d)", prefix, p.MaxLifetimeDays, p.InactivityDays)
	case p.RotationGraceSeconds < 10 || p.RotationGraceSeconds > 3600:
		return fmt.Errorf("%srotation_grace_seconds: %d must be 10..3600", prefix, p.RotationGraceSeconds)
	case p.EnrollMinutes < 1 || p.EnrollMinutes > 60:
		return fmt.Errorf("%senroll_minutes: %d must be 1..60", prefix, p.EnrollMinutes)
	case p.MaxLinksPerUser < 1 || p.MaxLinksPerUser > 50:
		return fmt.Errorf("%smax_links_per_user: %d must be 1..50", prefix, p.MaxLinksPerUser)
	}
	return nil
}

// ParseTrustedProxies parses TrustedProxies (CIDRs or bare IPs) into prefixes.
func (w *WidgetConfig) ParseTrustedProxies() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(w.TrustedProxies))
	for _, t := range w.TrustedProxies {
		t = strings.TrimSpace(t)
		if p, err := netip.ParsePrefix(t); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(t)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR or IP address", t)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// PublicPrefix returns the path of PublicURL (the prefix the widget server is mounted at).
func (w *WidgetConfig) PublicPrefix() string {
	u, err := url.Parse(w.PublicURL)
	if err != nil {
		return ""
	}
	return u.Path
}

// DialTimeout returns how long an outgoing call may ring before it is cancelled.
func (c *Config) DialTimeout() time.Duration {
	return time.Duration(c.DialTimeoutSeconds) * time.Second
}

// NumberFormat returns the Request-URI user format for dialled numbers.
func (c *Config) NumberFormat() sipua.NumberFormat {
	if c.DialNumberFormat == "e164" {
		return sipua.FormatE164
	}
	return sipua.FormatDigits
}

// RedactDTMFCommand reports whether "dtmf" command messages are redacted (default true).
func (c *Config) RedactDTMFCommand() bool {
	return c.DTMFRedactCommand == nil || *c.DTMFRedactCommand
}

// RegisterExpiry returns the REGISTER Expires as a Duration.
func (c *Config) RegisterExpiry() time.Duration {
	return time.Duration(c.RegisterExpirySeconds) * time.Second
}

// RegisterRefresh returns the re-REGISTER interval as a Duration.
func (c *Config) RegisterRefresh() time.Duration {
	return time.Duration(c.RegisterRefreshSeconds) * time.Second
}

// RingTimeout returns how long an incoming call rings before it counts as missed.
func (c *Config) RingTimeout() time.Duration {
	return time.Duration(c.RingTimeoutSeconds) * time.Second
}

// RTPTimeout returns how long an answered call may receive no RTP before it is hung up.
func (c *Config) RTPTimeout() time.Duration {
	return time.Duration(c.RTPTimeoutSeconds) * time.Second
}

// NetConfig returns the sipaccount view of c.
func (c *Config) NetConfig() sipaccount.NetConfig {
	return sipaccount.NetConfig{
		SIPBindIP:       c.SIPBindIP,
		SIPPortMin:      c.SIPPortMin,
		SIPPortMax:      c.SIPPortMax,
		ExternalIP:      c.ExternalIP,
		STUNServer:      c.STUNServer,
		RTPPortMin:      c.RTPPortMin,
		RTPPortMax:      c.RTPPortMax,
		RegisterExpiry:  c.RegisterExpiry(),
		RegisterRefresh: c.RegisterRefresh(),
	}
}
