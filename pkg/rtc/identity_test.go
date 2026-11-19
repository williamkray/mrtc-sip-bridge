package rtc

import (
	"testing"

	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

func TestIsOwner(t *testing.T) {
	const owner = id.UserID("@o:s")
	for identity, want := range map[string]bool{
		"@o:s:DEV":   true,
		"@o:s2:DEV":  false,
		"@oo:s:DEV":  false,
		"@o:s":       false,
		"@ghost:s:D": false,
	} {
		if got := newOwnerMatcher(owner, nil).match(identity); got != want {
			t.Errorf("isOwner(%q)=%v want %v", identity, got, want)
		}
	}
}

func TestLiveKitRoomNameVectors(t *testing.T) {
	// lk-jwt-service >= 0.10 maps both spellings to the stable slot; <= 0.9 hashed "m.call#ROOM".
	const stable = "m.call#room"
	want := matrixrtc.LiveKitRoomAlias("!r:s", stable)
	if LiveKitRoomName("!r:s") != want || want != matrixrtc.LiveKitRoomAlias("!r:s", "m.call#ROOM") {
		t.Fatal("LiveKitRoomName mismatch")
	}
	const vec = "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc"
	if got := matrixrtc.LiveKitRoomAlias("!roomid:example.com", "slot1234"); got != vec {
		t.Fatalf("got %s", got)
	}
}

func TestOwnerMatcher(t *testing.T) {
	m := newOwnerMatcher("@o:s", []string{"HASH1"})
	for identity, want := range map[string]bool{"HASH1": true, "HASH2": false, "@o:s:D": true} {
		if m.match(identity) != want {
			t.Errorf("match(%q)=%v", identity, !want)
		}
	}
	m.add("HASH2")
	if !m.match("HASH2") {
		t.Fatal("added id not matched")
	}
	if newOwnerMatcher("", nil).match(":x") {
		t.Fatal("empty owner must not match prefix")
	}
}

func TestPathTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"/sfu/get":                                 "/sfu/get",
		"/livekit/jwt/sfu/get":                     "/sfu/get",
		"/.well-known/matrix/client":               "/.well-known/matrix/client",
		"/_matrix/client/v1/rtc/livekit/get_token": "/rtc/livekit/get_token",
		"/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token": "/rtc/livekit/get_token",
		"/_matrix/client/v1/rtc/transports":                                 "/rtc/transports",
		"/rtc/transports/x":                                                 "*",
		"/other":                                                            "*",
		"/sfu/get/x":                                                        "*",
	} {
		if got := PathTemplate(in); got != want {
			t.Errorf("PathTemplate(%q)=%q want %q", in, got, want)
		}
	}
}
