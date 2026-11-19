package hostkit

import (
	_ "embed"

	"go.mau.fi/util/configupgrade"
	"go.mau.fi/util/random"
)

// ExampleYAML is the embedded example-config.yaml (top-level keys, no indent): the standalone `network:`
// section, and the body of jabber's `network.calls` after its `enabled` key.
//
//go:embed example-config.yaml
var ExampleYAML string

// CopyKeys copies every Config key from the user's file into the upgrade base under path
// (standalone: no path; jabber: "calls"). Every key is copied (jmpchat-matrix-bridge lesson: keys that are not
// copied get reset to the example value on every start).
func CopyKeys(h configupgrade.Helper, path ...string) {
	at := func(keys ...string) []string {
		p := make([]string, 0, len(path)+len(keys))
		return append(append(p, path...), keys...)
	}
	h.Copy(configupgrade.Str, at("sip_bind_ip")...)
	h.Copy(configupgrade.Int, at("sip_port_min")...)
	h.Copy(configupgrade.Int, at("sip_port_max")...)
	h.Copy(configupgrade.Str, at("external_ip")...)
	h.Copy(configupgrade.Str, at("stun_server")...)
	h.Copy(configupgrade.Int, at("rtp_port_min")...)
	h.Copy(configupgrade.Int, at("rtp_port_max")...)
	h.Copy(configupgrade.Int, at("register_expiry_seconds")...)
	h.Copy(configupgrade.Int, at("register_refresh_seconds")...)
	h.Copy(configupgrade.Int, at("ring_timeout_seconds")...)
	h.Copy(configupgrade.Int, at("rtp_timeout_seconds")...)
	h.Copy(configupgrade.Str, at("livekit_service_url")...)
	h.Copy(configupgrade.Str, at("default_country_code")...)
	h.Copy(configupgrade.Str, at("matrixrtc_format")...)
	h.Copy(configupgrade.Str, at("livekit_auth")...)
	h.Copy(configupgrade.Str, at("outbound_trigger")...)
	h.Copy(configupgrade.Int, at("dial_timeout_seconds")...)
	h.Copy(configupgrade.List, at("dial_allowed_prefixes")...)
	h.Copy(configupgrade.Str, at("dial_number_format")...)
	h.Copy(configupgrade.Bool, at("allow_self_dial")...)
	h.Copy(configupgrade.Bool, at("dtmf_redact_command")...)
	h.Copy(configupgrade.Bool, at("widget", "enabled")...)
	h.Copy(configupgrade.Str, at("widget", "public_url")...)
	h.Copy(configupgrade.Str, at("widget", "secret")...)
	h.Copy(configupgrade.List, at("widget", "frame_ancestors")...)
	h.Copy(configupgrade.Bool, at("widget", "allow_remote_users")...)
	h.Copy(configupgrade.Bool, at("widget", "remove_when_disabled")...)
	h.Copy(configupgrade.List, at("widget", "trusted_proxies")...)
	h.Copy(configupgrade.Bool, at("widget", "pwa", "enabled")...)
	h.Copy(configupgrade.Int, at("widget", "pwa", "inactivity_days")...)
	h.Copy(configupgrade.Int, at("widget", "pwa", "max_lifetime_days")...)
	h.Copy(configupgrade.Int, at("widget", "pwa", "rotation_grace_seconds")...)
	h.Copy(configupgrade.Int, at("widget", "pwa", "enroll_minutes")...)
	h.Copy(configupgrade.Int, at("widget", "pwa", "max_links_per_user")...)
	h.Copy(configupgrade.Bool, at("widget", "pwa", "notify_on_enroll")...)
	// Like bridgev2's as_token: a missing or "generate" secret becomes random (persisted on first start).
	if sec, ok := h.Get(configupgrade.Str, at("widget", "secret")...); !ok || sec == "" || sec == WidgetSecretGenerate {
		h.Set(configupgrade.Str, random.String(64), at("widget", "secret")...)
	}
}
