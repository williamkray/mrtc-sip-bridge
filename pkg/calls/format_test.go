package calls

import (
	"testing"

	mevent "maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
)

func ownerStickyMem(typ mevent.Type, slot string, ts []matrixrtc.Transport) matrixrtc.Membership {
	return matrixrtc.Membership{
		Format: matrixrtc.FormatSticky, Key: "sticky\x00m1", UserID: testOwner, MemberID: "m1", SlotID: slot,
		TypeName: typ.Type, Live: true, Transports: ts,
	}
}

var urlTransports = []matrixrtc.Transport{{Type: "livekit", URL: testRTCURL}}

// TestDecidePlan is the MATRIXRTC2-SPEC §2 decision table, one case per row (and the config variants).
func TestDecidePlan(t *testing.T) {
	base := planInputs{format: fmtAuto, auth: authAuto, rtcIntent: true, stickyOK: true, delayedOK: true, serviceURL: testServiceURL}
	with := func(f func(*planInputs)) planInputs {
		in := base
		f(&in)
		return in
	}
	dialectU := Dialect{TypeName: matrixrtc.RTCMember.Type, SlotID: matrixrtc.SlotIDRoomLegacy, URL: testRTCURL}
	type want struct {
		legacy, sticky, ringSticky, mirror, fallback bool
		typ                                          mevent.Type
		slot                                         string
		slots                                        []string
		auth                                         rtc.AuthMode
		url, reason                                  string
		ringSlot                                     string // sticky plans only
		slotTypes                                    int
	}
	tests := []struct {
		name string
		in   planInputs
		want want
	}{
		{
			"owner sticky, mirrors dialect and url, sticky only",
			with(func(in *planInputs) {
				in.ownerInit, in.ownerFormat = true, matrixrtc.FormatSticky
				in.ownerMem = ownerStickyMem(matrixrtc.RTCMember, matrixrtc.SlotIDRoomLegacy, urlTransports)
			}),
			want{sticky: true, ringSticky: true, mirror: true, fallback: true, typ: matrixrtc.RTCMember,
				slot: "m.call#ROOM", slots: []string{"m.call#ROOM"}, auth: rtc.AuthMSC4195, url: testRTCURL, reason: reasonOwnerMirror,
				ringSlot: "m.call#ROOM", slotTypes: 1},
		},
		{
			"owner sticky stable names",
			with(func(in *planInputs) {
				in.ownerInit, in.ownerFormat = true, matrixrtc.FormatSticky
				in.ownerMem = ownerStickyMem(matrixrtc.RTCMemberAlias, matrixrtc.SlotIDRoom, urlTransports)
			}),
			want{sticky: true, ringSticky: true, mirror: true, fallback: true, typ: matrixrtc.RTCMemberAlias,
				slot: "m.call#room", slots: []string{"m.call#room"}, auth: rtc.AuthMSC4195, url: testRTCURL, reason: reasonOwnerMirror,
				ringSlot: "m.call#room", slotTypes: 1},
		},
		{
			"owner sticky with a service-url transport only: OpenID",
			with(func(in *planInputs) {
				in.ownerInit, in.ownerFormat = true, matrixrtc.FormatSticky
				in.ownerMem = ownerStickyMem(matrixrtc.RTCMember, matrixrtc.SlotIDRoomLegacy,
					[]matrixrtc.Transport{{Type: "livekit", ServiceURL: testServiceURL}})
			}),
			want{sticky: true, ringSticky: true, mirror: true, typ: matrixrtc.RTCMember,
				slot: "m.call#ROOM", slots: []string{"m.call#ROOM"}, auth: rtc.AuthSFUGet, reason: reasonOwnerMirror,
				ringSlot: "m.call#ROOM", slotTypes: 1},
		},
		{
			"owner legacy: unchanged",
			with(func(in *planInputs) {
				in.ownerInit, in.ownerFormat = true, matrixrtc.FormatLegacy
				in.transportURL = testRTCURL
			}),
			want{legacy: true, slot: "m.call#ROOM", auth: rtc.AuthSFUGet, reason: reasonOwnerLegacy},
		},
		{
			"owner unknown format: legacy",
			with(func(in *planInputs) { in.ownerInit = true }),
			want{legacy: true, slot: "m.call#ROOM", auth: rtc.AuthSFUGet, reason: reasonOwnerLegacy},
		},
		{
			"bridge-initiated with dialect: dual in the dialect, both slots, MSC4195 from transports",
			with(func(in *planInputs) {
				in.dialect, in.hasDialect = dialectU, true
				in.transportURL = "wss://sfu.transports.example.org"
			}),
			want{legacy: true, sticky: true, ringSticky: true, fallback: true, typ: matrixrtc.RTCMember,
				slot: "m.call#ROOM", slots: []string{"m.call#ROOM", "m.call#room"}, auth: rtc.AuthMSC4195,
				url: "wss://sfu.transports.example.org", reason: reasonBridgeDialect, ringSlot: "m.call#ROOM", slotTypes: 2},
		},
		{
			"bridge-initiated, no dialect: dual in stable names, but legacy ring shape and sfu_get even with a transports url",
			with(func(in *planInputs) { in.transportURL = testRTCURL }),
			want{legacy: true, sticky: true, ringSticky: false, typ: matrixrtc.RTCMemberAlias,
				slot: "m.call#room", slots: []string{"m.call#room", "m.call#ROOM"}, auth: rtc.AuthSFUGet, reason: reasonBridgeStable,
				ringSlot: "m.call#ROOM", slotTypes: 2},
		},
		{
			"bridge-initiated, dialect known but MSC4195 failed recently: sfu_get, no fallback",
			with(func(in *planInputs) {
				in.dialect, in.hasDialect = dialectU, true
				in.transportURL = testRTCURL
				in.tokenBlock = true
			}),
			want{legacy: true, sticky: true, ringSticky: true, typ: matrixrtc.RTCMember, slot: "m.call#ROOM",
				slots: []string{"m.call#ROOM", "m.call#room"}, auth: rtc.AuthSFUGet, reason: reasonMSC4195Failed,
				ringSlot: "m.call#ROOM", slotTypes: 2},
		},
		{
			"owner sticky mirror, MSC4195 failed recently: sfu_get",
			with(func(in *planInputs) {
				in.ownerInit, in.ownerFormat = true, matrixrtc.FormatSticky
				in.ownerMem = ownerStickyMem(matrixrtc.RTCMember, matrixrtc.SlotIDRoomLegacy, urlTransports)
				in.tokenBlock = true
			}),
			want{sticky: true, ringSticky: true, mirror: true, typ: matrixrtc.RTCMember, slot: "m.call#ROOM",
				slots: []string{"m.call#ROOM"}, auth: rtc.AuthSFUGet, reason: reasonMSC4195Failed, ringSlot: "m.call#ROOM", slotTypes: 1},
		},
		{
			"livekit_auth msc4195 ignores the failure memory",
			with(func(in *planInputs) {
				in.auth = authMSC4195
				in.dialect, in.hasDialect = dialectU, true
				in.transportURL = testRTCURL
				in.tokenBlock = true
			}),
			want{legacy: true, sticky: true, ringSticky: true, typ: matrixrtc.RTCMember, slot: "m.call#ROOM",
				slots: []string{"m.call#ROOM", "m.call#room"}, auth: rtc.AuthMSC4195, url: testRTCURL, reason: reasonBridgeDialect,
				ringSlot: "m.call#ROOM", slotTypes: 2},
		},
		{
			"no sticky capability: legacy only",
			with(func(in *planInputs) { in.stickyOK = false }),
			want{legacy: true, slot: "m.call#ROOM", auth: rtc.AuthSFUGet, reason: reasonNoSticky},
		},
		{
			"sticky refused recently: legacy only",
			with(func(in *planInputs) { in.stickyBlock = true }),
			want{legacy: true, slot: "m.call#ROOM", auth: rtc.AuthSFUGet, reason: reasonStickyRecently},
		},
		{
			"ghost without GhostRTCIntent: legacy only",
			with(func(in *planInputs) { in.rtcIntent = false }),
			want{legacy: true, slot: "m.call#ROOM", auth: rtc.AuthSFUGet, reason: reasonNoRTCIntent},
		},
		{
			"config legacy ignores everything",
			with(func(in *planInputs) {
				in.format = fmtLegacy
				in.ownerInit, in.ownerFormat = true, matrixrtc.FormatSticky
				in.ownerMem = ownerStickyMem(matrixrtc.RTCMember, matrixrtc.SlotIDRoomLegacy, urlTransports)
			}),
			want{legacy: true, slot: "m.call#ROOM", auth: rtc.AuthSFUGet, reason: reasonConfigLegacy},
		},
		{
			"config sticky, bridge-initiated, dialect: sticky only, both slots",
			with(func(in *planInputs) {
				in.format = fmtSticky
				in.dialect, in.hasDialect = dialectU, true
				in.transportURL = testRTCURL
			}),
			want{sticky: true, ringSticky: true, fallback: true, typ: matrixrtc.RTCMember, slot: "m.call#ROOM",
				slots: []string{"m.call#ROOM", "m.call#room"}, auth: rtc.AuthMSC4195, url: testRTCURL, reason: reasonConfigSticky,
				ringSlot: "m.call#ROOM", slotTypes: 2},
		},
		{
			"config sticky, owner-initiated with a legacy owner: sticky in stable names",
			with(func(in *planInputs) {
				in.format = fmtSticky
				in.ownerInit, in.ownerFormat = true, matrixrtc.FormatLegacy
			}),
			want{sticky: true, ringSticky: true, typ: matrixrtc.RTCMemberAlias, slot: "m.call#room",
				slots: []string{"m.call#room", "m.call#ROOM"}, auth: rtc.AuthSFUGet, reason: reasonConfigSticky,
				ringSlot: "m.call#room", slotTypes: 2},
		},
		{
			"config dual, owner-initiated with a sticky owner: dual mirrored",
			with(func(in *planInputs) {
				in.format = fmtDual
				in.ownerInit, in.ownerFormat = true, matrixrtc.FormatSticky
				in.ownerMem = ownerStickyMem(matrixrtc.RTCMemberAlias, matrixrtc.SlotIDRoom, urlTransports)
			}),
			want{legacy: true, sticky: true, ringSticky: true, mirror: true, fallback: true, typ: matrixrtc.RTCMemberAlias,
				slot: "m.call#room", slots: []string{"m.call#room"}, auth: rtc.AuthMSC4195, url: testRTCURL, reason: reasonConfigDual,
				ringSlot: "m.call#room", slotTypes: 1},
		},
		{
			"livekit_auth sfu_get never uses MSC4195",
			with(func(in *planInputs) {
				in.auth = authSFUGet
				in.transportURL = testRTCURL
			}),
			want{legacy: true, sticky: true, ringSticky: false, typ: matrixrtc.RTCMemberAlias, slot: "m.call#room",
				slots: []string{"m.call#room", "m.call#ROOM"}, auth: rtc.AuthSFUGet, reason: reasonBridgeStable,
				ringSlot: "m.call#ROOM", slotTypes: 2},
		},
		{
			"livekit_auth msc4195: no fallback",
			with(func(in *planInputs) {
				in.auth = authMSC4195
				in.transportURL = testRTCURL
			}),
			want{legacy: true, sticky: true, ringSticky: false, typ: matrixrtc.RTCMemberAlias, slot: "m.call#room",
				slots: []string{"m.call#room", "m.call#ROOM"}, auth: rtc.AuthMSC4195, url: testRTCURL, reason: reasonBridgeStable,
				ringSlot: "m.call#ROOM", slotTypes: 2},
		},
		{
			"livekit_auth msc4195 without any url degrades to OpenID",
			with(func(in *planInputs) { in.auth = authMSC4195 }),
			want{legacy: true, sticky: true, ringSticky: false, typ: matrixrtc.RTCMemberAlias, slot: "m.call#room",
				slots: []string{"m.call#room", "m.call#ROOM"}, auth: rtc.AuthSFUGet, reason: reasonBridgeStable,
				ringSlot: "m.call#ROOM", slotTypes: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := decidePlan(tt.in)
			ring := p.ringSlot
			if ring == "" {
				ring = p.slot
			}
			got := want{p.legacy, p.sticky, p.ringSticky, p.mirror, p.fallback, p.typ, p.slot, p.slots, p.auth, p.url, p.reason,
				ring, len(p.slotPairs()) / max(len(p.slots), 1)}
			if !p.sticky {
				got.ringSlot, got.slotTypes = tt.want.ringSlot, tt.want.slotTypes // not published
			}
			if got.legacy != tt.want.legacy || got.sticky != tt.want.sticky || got.ringSticky != tt.want.ringSticky ||
				got.mirror != tt.want.mirror || got.fallback != tt.want.fallback || got.typ != tt.want.typ ||
				got.slot != tt.want.slot || got.auth != tt.want.auth || got.url != tt.want.url || got.reason != tt.want.reason ||
				got.ringSlot != tt.want.ringSlot || got.slotTypes != tt.want.slotTypes ||
				len(got.slots) != len(tt.want.slots) {
				t.Fatalf("plan\n got %+v\nwant %+v", got, tt.want)
			}
			for i := range got.slots {
				if got.slots[i] != tt.want.slots[i] {
					t.Fatalf("slots %v want %v", got.slots, tt.want.slots)
				}
			}
			if p.sticky && p.slotType != slotTypeFor(p.typ) {
				t.Errorf("slot type %v for %v", p.slotType, p.typ)
			}
			if p.serviceURL != testServiceURL {
				t.Errorf("service url %q", p.serviceURL)
			}
		})
	}
}

func TestPlanFormatName(t *testing.T) {
	for _, tc := range []struct {
		p    plan
		want string
	}{
		{plan{legacy: true}, fmtLegacy},
		{plan{sticky: true}, fmtSticky},
		{plan{legacy: true, sticky: true}, fmtDual},
	} {
		if got := tc.p.formatName(); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.p, got, tc.want)
		}
	}
}

func TestWantTransports(t *testing.T) {
	base := planInputs{format: fmtAuto, auth: authAuto, rtcIntent: true, stickyOK: true}
	owner := func(url bool) planInputs {
		in := base
		in.ownerInit, in.ownerFormat = true, matrixrtc.FormatSticky
		ts := []matrixrtc.Transport{{Type: "livekit", ServiceURL: "x"}}
		if url {
			ts = urlTransports
		}
		in.ownerMem = ownerStickyMem(matrixrtc.RTCMember, matrixrtc.SlotIDRoom, ts)
		return in
	}
	legacyOwner := base
	legacyOwner.ownerInit, legacyOwner.ownerFormat = true, matrixrtc.FormatLegacy
	cfgLegacy := base
	cfgLegacy.format = fmtLegacy
	sfu := base
	sfu.auth = authSFUGet
	noCaps := base
	noCaps.stickyOK = false
	withDialect := base
	withDialect.hasDialect = true
	forced := base
	forced.auth = authMSC4195
	blocked := withDialect
	blocked.tokenBlock = true
	cfgSticky := base
	cfgSticky.format = fmtSticky
	for name, tc := range map[string]struct {
		in   planInputs
		want bool
	}{
		"bridge-initiated, no dialect: sfu_get":    {base, false},
		"bridge-initiated, dialect":                {withDialect, true},
		"no dialect, explicit msc4195":             {forced, true},
		"no dialect, config sticky":                {cfgSticky, true},
		"msc4195 failed recently":                  {blocked, false},
		"owner with url":                           {owner(true), false},
		"owner without url (mirror never fetches)": {owner(false), false},
		"legacy owner":                             {legacyOwner, false},
		"config legacy":                            {cfgLegacy, false},
		"sfu_get":                                  {sfu, false},
		"no sticky capability":                     {noCaps, false},
	} {
		if got := wantTransports(tc.in); got != tc.want {
			t.Errorf("%s: %v want %v", name, got, tc.want)
		}
	}
}

func TestNormalizeConfig(t *testing.T) {
	for in, want := range map[string]string{"": fmtAuto, "auto": fmtAuto, "legacy": fmtLegacy, "sticky": fmtSticky, "dual": fmtDual, "x": fmtAuto} {
		if got := normalizeFormat(in); got != want {
			t.Errorf("format %q = %q", in, got)
		}
	}
	for in, want := range map[string]string{"": authAuto, "sfu_get": authSFUGet, "msc4195": authMSC4195, "x": authAuto} {
		if got := normalizeAuth(in); got != want {
			t.Errorf("auth %q = %q", in, got)
		}
	}
}
