package matrixrtc

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"

	"maunium.net/go/mautrix/id"
)

// hashJSON is lk-jwt-service's livekit_room_alias_for / MSC4195 identity hash: sha256 of the compact
// JSON array (HTML escaping off), standard base64 without padding. Checked against the MSC4195 test
// vector (see identity_test.go). pkg/rtc keeps a private copy until it moves to these helpers.
func hashJSON(parts ...string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(parts)
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// LiveKitIdentity is the MSC4195 hashed LiveKit identity: b64(sha256([user, memberID])).
func LiveKitIdentity(user id.UserID, memberID string) string {
	return hashJSON(string(user), memberID)
}

// LegacyLiveKitIdentity is the pre-MSC4195 identity "<user>:<device>" (OpenID /sfu/get).
func LegacyLiveKitIdentity(user id.UserID, deviceID string) string {
	return string(user) + ":" + deviceID
}

// LiveKitRoomAlias is the LiveKit room name for (room, slot): b64(sha256([room, slot])), with the legacy
// slot spelling "m.call#ROOM" mapped to "m.call#room" first, like lk-jwt-service's
// map_to_stable_m_call_slot_id.
func LiveKitRoomAlias(room id.RoomID, slotID string) string {
	if slotID == SlotIDRoomLegacy {
		slotID = SlotIDRoom
	}
	return hashJSON(string(room), slotID)
}
