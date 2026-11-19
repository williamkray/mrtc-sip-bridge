package matrixrtc

import "testing"

func TestHashJSONVectors(t *testing.T) {
	// MSC4195 room alias test vector; the 43-char result shows standard base64 WITHOUT padding.
	if got := hashJSON("!roomid:example.com", "slot1234"); got != "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc" {
		t.Fatalf("MSC4195 vector = %s", got)
	}
	// Computed independently with python3:
	//   h = lambda a: base64.b64encode(hashlib.sha256(json.dumps(a, separators=(',',':'), ensure_ascii=False)
	//       .encode()).digest()).decode().rstrip('=')
	//   h(['@ghost:example.org','0b9c7c1e-5a4d-4a52-9d3f-1f6f2f1e7a10'])
	if got := LiveKitIdentity("@ghost:example.org", "0b9c7c1e-5a4d-4a52-9d3f-1f6f2f1e7a10"); got != "/dgcdzUiiGTCwYCmloo90q5NJzTqic/eSgnHf7dHU3U" {
		t.Fatalf("identity = %s", got)
	}
	// Same method: h(['!room:example.org','m.call#room'])
	const want = "7GA+kQD3mMQfLS9KmBnMOFvKRSGhnWx6aRMOpJg8evw"
	if got := LiveKitRoomAlias("!room:example.org", SlotIDRoom); got != want {
		t.Fatalf("alias = %s", got)
	}
	if got := LiveKitRoomAlias("!room:example.org", SlotIDRoomLegacy); got != want {
		t.Fatalf("legacy slot alias = %s, want mapped to stable", got)
	}
	if got := LegacyLiveKitIdentity("@u:s", "DEV"); got != "@u:s:DEV" {
		t.Fatalf("legacy identity = %s", got)
	}
}
