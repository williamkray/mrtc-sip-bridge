package hostkit

import (
	"reflect"
	"strings"
	"testing"

	"go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"

	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

func defaultConfig() Config {
	redact := true
	notify := true
	return Config{
		OutboundTrigger: "ring", DialTimeoutSeconds: 60, DialAllowedPrefixes: []string{"+1"},
		DialNumberFormat: "digits", DTMFRedactCommand: &redact,
		SIPPortMin: 5060, SIPPortMax: 5069, STUNServer: defaultSTUNServer,
		RTPPortMin: 40000, RTPPortMax: 40100,
		RegisterExpirySeconds: 60, RegisterRefreshSeconds: 25, RingTimeoutSeconds: 30, RTPTimeoutSeconds: 60,
		DefaultCountryCode: "1", MatrixRTCFormat: "auto", LiveKitAuth: "auto",
		Widget: WidgetConfig{Secret: "generate", FrameAncestors: []string{"*"}, TrustedProxies: []string{},
			PWA: PWAConfig{InactivityDays: 90, RotationGraceSeconds: 120, EnrollMinutes: 15, MaxLinksPerUser: 5, NotifyOnEnroll: &notify}},
	}
}

func TestExampleConfigDefaults(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte(ExampleYAML), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate("network."); err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	if !reflect.DeepEqual(c, defaultConfig()) {
		t.Fatalf("got %+v want %+v", c, defaultConfig())
	}
}

func TestApplyDefaults(t *testing.T) {
	var c Config
	c.ApplyDefaults()
	want := defaultConfig()
	want.STUNServer = ""
	want.Widget.TrustedProxies = nil
	want.Widget.Secret = ""
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v want %+v", c, want)
	}
	if err := c.Validate("network."); err != nil {
		t.Fatal(err)
	}
	if c.RegisterExpiry().Seconds() != 60 || c.RegisterRefresh().Seconds() != 25 || c.RingTimeout().Seconds() != 30 {
		t.Fatal("duration helpers wrong")
	}
}

type validateCase struct {
	name string
	mod  func(*Config)
	key  string
}

var validateCases = []validateCase{
	{"bind ip bad", func(c *Config) { c.SIPBindIP = "nope" }, "sip_bind_ip"},
	{"bind ip v6", func(c *Config) { c.SIPBindIP = "::1" }, "sip_bind_ip"},
	{"sip min zero-ish", func(c *Config) { c.SIPPortMin = 70000 }, "sip_port_min"},
	{"sip max big", func(c *Config) { c.SIPPortMax = 70000 }, "sip_port_max"},
	{"sip min>max", func(c *Config) { c.SIPPortMin, c.SIPPortMax = 5070, 5060 }, "sip_port_min"},
	{"rtp min bad", func(c *Config) { c.RTPPortMin = -1 }, "rtp_port_min"},
	{"rtp max bad", func(c *Config) { c.RTPPortMax = 70000 }, "rtp_port_max"},
	{"rtp min>max", func(c *Config) { c.RTPPortMin, c.RTPPortMax = 41000, 40000 }, "rtp_port_min"},
	{"rtp one port", func(c *Config) { c.RTPPortMin, c.RTPPortMax = 40000, 40000 }, "rtp_port"},
	{"overlap", func(c *Config) { c.SIPPortMin, c.SIPPortMax = 40050, 40060 }, "sip_port_min"},
	{"external bad", func(c *Config) { c.ExternalIP = "nope" }, "external_ip"},
	{"external unspecified", func(c *Config) { c.ExternalIP = "0.0.0.0" }, "external_ip"},
	{"stun no port", func(c *Config) { c.STUNServer = "stun.example.com" }, "stun_server"},
	{"stun port nonnumeric", func(c *Config) { c.STUNServer = "stun.example.com:abc" }, "stun_server"},
	{"refresh low", func(c *Config) { c.RegisterRefreshSeconds = 4 }, "register_refresh_seconds"},
	{"refresh>=expiry", func(c *Config) { c.RegisterRefreshSeconds = 60 }, "register_refresh_seconds"},
	{"expiry big", func(c *Config) { c.RegisterExpirySeconds = 3601 }, "register_expiry_seconds"},
	{"ring low", func(c *Config) { c.RingTimeoutSeconds = 4 }, "ring_timeout_seconds"},
	{"ring high", func(c *Config) { c.RingTimeoutSeconds = 301 }, "ring_timeout_seconds"},
	{"rtp timeout low", func(c *Config) { c.RTPTimeoutSeconds = 9 }, "rtp_timeout_seconds"},
	{"rtp timeout high", func(c *Config) { c.RTPTimeoutSeconds = 3601 }, "rtp_timeout_seconds"},
	{"livekit relative", func(c *Config) { c.LiveKitServiceURL = "/jwt" }, "livekit_service_url"},
	{"livekit scheme", func(c *Config) { c.LiveKitServiceURL = "ftp://x.example" }, "livekit_service_url"},
	{"cc empty", func(c *Config) { c.DefaultCountryCode = "" }, "default_country_code"},
	{"cc zero", func(c *Config) { c.DefaultCountryCode = "01" }, "default_country_code"},
	{"cc long", func(c *Config) { c.DefaultCountryCode = "1234" }, "default_country_code"},
	{"trigger bad", func(c *Config) { c.OutboundTrigger = "always" }, "outbound_trigger"},
	{"dial timeout low", func(c *Config) { c.DialTimeoutSeconds = 9 }, "dial_timeout_seconds"},
	{"dial timeout high", func(c *Config) { c.DialTimeoutSeconds = 301 }, "dial_timeout_seconds"},
	{"prefix no plus", func(c *Config) { c.DialAllowedPrefixes = []string{"1"} }, "dial_allowed_prefixes"},
	{"prefix zero", func(c *Config) { c.DialAllowedPrefixes = []string{"+0"} }, "dial_allowed_prefixes"},
	{"prefix alpha", func(c *Config) { c.DialAllowedPrefixes = []string{"+1", "+4x"} }, "dial_allowed_prefixes"},
	{"format bad", func(c *Config) { c.DialNumberFormat = "national" }, "dial_number_format"},
	{"cc alpha", func(c *Config) { c.DefaultCountryCode = "4a" }, "default_country_code"},
	{"rtc format bad", func(c *Config) { c.MatrixRTCFormat = "both" }, "matrixrtc_format"},
	{"rtc format empty", func(c *Config) { c.MatrixRTCFormat = "" }, "matrixrtc_format"},
	{"livekit auth bad", func(c *Config) { c.LiveKitAuth = "openid" }, "livekit_auth"},
}

func TestValidate(t *testing.T) {
	tests := validateCases
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaultConfig()
			tt.mod(&c)
			err := c.Validate("network.")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("error %q does not mention %q", err, tt.key)
			}
		})
	}
	for _, ok := range []func(*Config){
		func(c *Config) { c.SIPBindIP = "0.0.0.0" },
		func(c *Config) { c.ExternalIP = "203.0.113.9" },
		func(c *Config) { c.STUNServer = "" },
		func(c *Config) { c.OutboundTrigger = "off" },
		func(c *Config) { c.OutboundTrigger = "membership" },
		func(c *Config) { c.DialAllowedPrefixes = []string{} },
		func(c *Config) { c.DialAllowedPrefixes = []string{"+1", "+44"} },
		func(c *Config) { c.DialNumberFormat = "e164" },
		func(c *Config) { c.LiveKitServiceURL = "https://lk.example.com/jwt" },
		func(c *Config) { c.MatrixRTCFormat, c.LiveKitAuth = "legacy", "sfu_get" },
		func(c *Config) { c.MatrixRTCFormat, c.LiveKitAuth = "sticky", "msc4195" },
		func(c *Config) { c.MatrixRTCFormat = "dual" },
	} {
		c := defaultConfig()
		ok(&c)
		if err := c.Validate("network."); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

func TestUpgradePreservesUserValues(t *testing.T) {
	userYAML := "external_ip: 203.0.113.9\nrtp_port_min: 41000\nrtp_timeout_seconds: 90\ndefault_country_code: \"44\"\n" +
		"outbound_trigger: membership\ndial_timeout_seconds: 90\ndial_allowed_prefixes: []\ndial_number_format: e164\nallow_self_dial: true\ndtmf_redact_command: false\n" +
		"matrixrtc_format: sticky\nlivekit_auth: msc4195\n"
	out, err := configupgrade.DoBytes([]byte(userYAML), &configupgrade.StructUpgrader{
		SimpleUpgrader: configupgrade.SimpleUpgrader(func(h configupgrade.Helper) { CopyKeys(h) }),
		Base:           ExampleYAML,
	})
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err = yaml.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	want := defaultConfig()
	want.MatrixRTCFormat, want.LiveKitAuth = "sticky", "msc4195"
	want.ExternalIP = "203.0.113.9"
	want.RTPPortMin = 41000
	want.RTPTimeoutSeconds = 90
	want.DefaultCountryCode = "44"
	no := false
	want.OutboundTrigger, want.DialTimeoutSeconds, want.DialAllowedPrefixes = "membership", 90, []string{}
	want.DialNumberFormat, want.AllowSelfDial, want.DTMFRedactCommand = "e164", true, &no
	want.Widget.Secret = c.Widget.Secret // generated
	if len(c.Widget.Secret) != 64 {
		t.Fatalf("secret not generated: %q", c.Widget.Secret)
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v want %+v", c, want)
	}
}

func TestOutboundDefaultsAndHelpers(t *testing.T) {
	var c Config
	c.ApplyDefaults()
	if !reflect.DeepEqual(c.DialAllowedPrefixes, []string{"+1"}) || !c.RedactDTMFCommand() || c.DialTimeout().Seconds() != 60 {
		t.Fatalf("defaults wrong: %+v", c)
	}
	if c.NumberFormat() != sipua.FormatDigits {
		t.Fatal("default format should be digits")
	}
	c.DialNumberFormat = "e164"
	if c.NumberFormat() != sipua.FormatE164 {
		t.Fatal("e164 format")
	}
	empty := Config{DialAllowedPrefixes: []string{}}
	empty.ApplyDefaults()
	if len(empty.DialAllowedPrefixes) != 0 {
		t.Fatal("explicit empty prefix list must stay empty")
	}
}

func validWidget() Config {
	c := defaultConfig()
	c.Widget = WidgetConfig{Enabled: true, PublicURL: "https://dev.example.org/dialpad/", Secret: strings.Repeat("s", 32),
		FrameAncestors: []string{"*"}}
	return c
}

func TestWidgetValidate(t *testing.T) {
	if c := defaultConfig(); c.Validate("network.") != nil {
		t.Fatal("disabled widget with example values must validate")
	}
	c := defaultConfig()
	c.Widget.PublicURL = "garbage" // ignored while disabled
	if err := c.Validate("network."); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []func(*Config){
		func(c *Config) {},
		func(c *Config) { c.Widget.TrustedProxies = []string{"10.0.0.0/8", "2001:db8::/32", "192.0.2.1", "::1"} },
		func(c *Config) { c.Widget.PublicURL = "https://dev.example.org:8443/w/" },
		func(c *Config) { c.Widget.PublicURL = "http://localhost:8080/w/" },
		func(c *Config) { c.Widget.PublicURL = "http://127.0.0.1/w/" },
		func(c *Config) {
			c.Widget.FrameAncestors = []string{"'self'", "https://app.element.io", "https://*.example.org", "https://x.org:8443"}
		},
		func(c *Config) { c.Widget.FrameAncestors = []string{} },
	} {
		c := validWidget()
		ok(&c)
		if err := c.Validate("network.calls."); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
	for name, tt := range map[string]struct {
		mod func(*Config)
		key string
	}{
		"empty url":    {func(c *Config) { c.Widget.PublicURL = "" }, "public_url"},
		"relative":     {func(c *Config) { c.Widget.PublicURL = "/dialpad/" }, "public_url"},
		"http":         {func(c *Config) { c.Widget.PublicURL = "http://dev.example.org/dialpad/" }, "public_url"},
		"query":        {func(c *Config) { c.Widget.PublicURL = "https://x.org/d/?a=b" }, "public_url"},
		"fragment":     {func(c *Config) { c.Widget.PublicURL = "https://x.org/d/#f" }, "public_url"},
		"no slash":     {func(c *Config) { c.Widget.PublicURL = "https://x.org/d" }, "public_url"},
		"root":         {func(c *Config) { c.Widget.PublicURL = "https://x.org/" }, "public_url"},
		"no path":      {func(c *Config) { c.Widget.PublicURL = "https://x.org" }, "public_url"},
		"matrix":       {func(c *Config) { c.Widget.PublicURL = "https://x.org/_matrix/app/" }, "public_url"},
		"dotdot":       {func(c *Config) { c.Widget.PublicURL = "https://x.org/a/../_matrix/" }, "public_url"},
		"double slash": {func(c *Config) { c.Widget.PublicURL = "https://x.org/a//b/" }, "public_url"},
		"credentials":  {func(c *Config) { c.Widget.PublicURL = "https://u:p@x.org/d/" }, "public_url"},
		"short secret": {func(c *Config) { c.Widget.Secret = strings.Repeat("s", 31) }, "secret"},
		"generate":     {func(c *Config) { c.Widget.Secret = "generate" }, "secret"},
		"tp junk":      {func(c *Config) { c.Widget.TrustedProxies = []string{"not-an-ip"} }, "trusted_proxies"},
		"tp bad mask":  {func(c *Config) { c.Widget.TrustedProxies = []string{"10.0.0.0/99"} }, "trusted_proxies"},
		"port 443":     {func(c *Config) { c.Widget.PublicURL = "https://dev.example.org:443/w/" }, "public_url"},
		"port 80":      {func(c *Config) { c.Widget.PublicURL = "http://localhost:80/w/" }, "public_url"},
		"fa space":     {func(c *Config) { c.Widget.FrameAncestors = []string{"https://a.org https://b.org"} }, "frame_ancestors"},
		"fa semicolon": {func(c *Config) { c.Widget.FrameAncestors = []string{"https://a.org;script-src *"} }, "frame_ancestors"},
		"fa path":      {func(c *Config) { c.Widget.FrameAncestors = []string{"https://a.org/x"} }, "frame_ancestors"},
		"fa none":      {func(c *Config) { c.Widget.FrameAncestors = []string{"'none'"} }, "frame_ancestors"},
		"fa empty":     {func(c *Config) { c.Widget.FrameAncestors = []string{""} }, "frame_ancestors"},
	} {
		c := validWidget()
		tt.mod(&c)
		err := c.Validate("network.calls.")
		if err == nil {
			t.Errorf("%s: expected error", name)
		} else if !strings.HasPrefix(err.Error(), "network.calls.widget."+tt.key) {
			t.Errorf("%s: error %q", name, err)
		}
	}
	c = validWidget()
	if c.Widget.PublicPrefix() != "/dialpad/" {
		t.Fatalf("prefix %q", c.Widget.PublicPrefix())
	}
}

func TestWidgetUpgrade(t *testing.T) {
	up := func(user string) Config {
		out, err := configupgrade.DoBytes([]byte(user), &configupgrade.StructUpgrader{
			SimpleUpgrader: configupgrade.SimpleUpgrader(func(h configupgrade.Helper) { CopyKeys(h) }),
			Base:           ExampleYAML,
		})
		if err != nil {
			t.Fatal(err)
		}
		var c Config
		if err = yaml.Unmarshal(out, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	// Old config without a widget block: generated secret, defaults, disabled.
	c := up("external_ip: 203.0.113.9\n")
	if c.Widget.Enabled || len(c.Widget.Secret) != 64 || c.Widget.Secret == "generate" || !reflect.DeepEqual(c.Widget.FrameAncestors, []string{"*"}) {
		t.Fatalf("defaults: %+v", c.Widget)
	}
	// User values survive, including an explicit secret.
	c = up("widget:\n    enabled: true\n    public_url: https://x.org/d/\n    secret: " + strings.Repeat("k", 40) +
		"\n    frame_ancestors: [\"https://app.element.io\"]\n    allow_remote_users: true\n    remove_when_disabled: true\n    trusted_proxies: [\"10.0.0.0/8\"]\n")
	notify := true
	want := WidgetConfig{Enabled: true, PWA: PWAConfig{InactivityDays: 90, RotationGraceSeconds: 120, EnrollMinutes: 15, MaxLinksPerUser: 5, NotifyOnEnroll: &notify}, PublicURL: "https://x.org/d/", Secret: strings.Repeat("k", 40),
		FrameAncestors: []string{"https://app.element.io"}, AllowRemoteUsers: true, RemoveWhenDisabled: true, TrustedProxies: []string{"10.0.0.0/8"}}
	if !reflect.DeepEqual(c.Widget, want) {
		t.Fatalf("got %+v want %+v", c.Widget, want)
	}
	// "generate" in the user's file is replaced too.
	c = up("widget:\n    secret: generate\n")
	if len(c.Widget.Secret) != 64 {
		t.Fatalf("generate not replaced: %q", c.Widget.Secret)
	}
}

func TestPWAUpgradePreservesValues(t *testing.T) {
	user := "widget:\n    pwa:\n        enabled: true\n        inactivity_days: 30\n        max_lifetime_days: 365\n" +
		"        rotation_grace_seconds: 60\n        enroll_minutes: 10\n        max_links_per_user: 2\n        notify_on_enroll: false\n"
	out, err := configupgrade.DoBytes([]byte(user), &configupgrade.StructUpgrader{
		SimpleUpgrader: configupgrade.SimpleUpgrader(func(h configupgrade.Helper) { CopyKeys(h) }),
		Base:           ExampleYAML,
	})
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err = yaml.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	no := false
	want := PWAConfig{Enabled: true, InactivityDays: 30, MaxLifetimeDays: 365, RotationGraceSeconds: 60, EnrollMinutes: 10, MaxLinksPerUser: 2, NotifyOnEnroll: &no}
	if !reflect.DeepEqual(c.Widget.PWA, want) {
		t.Fatalf("got %+v want %+v", c.Widget.PWA, want)
	}
}
