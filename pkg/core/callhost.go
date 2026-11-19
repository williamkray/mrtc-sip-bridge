package core

import (
	"context"
	"errors"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// LoginRef identifies one SIP account login inside a host. The host chooses the value
// (the standalone bridge uses its bridgev2 UserLoginID). Core treats it as opaque.
type LoginRef string

// OpenIDToken is the Matrix OpenID token returned by
// POST /_matrix/client/v3/user/{userId}/openid/request_token, minted for a ghost.
type OpenIDToken struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	MatrixServerName string `json:"matrix_server_name"`
	ExpiresIn        int    `json:"expires_in"`
}

// GhostIntent acts as the ghost user that stands for the remote phone number.
// Content maps are sent as raw JSON event content.
type GhostIntent interface {
	UserID() id.UserID
	SendState(ctx context.Context, roomID id.RoomID, evtType event.Type, stateKey string, content map[string]any) (id.EventID, error)
	SendMessage(ctx context.Context, roomID id.RoomID, evtType event.Type, content map[string]any) (id.EventID, error)
	Redact(ctx context.Context, roomID id.RoomID, eventID id.EventID, reason string) error
	RequestOpenIDToken(ctx context.Context) (*OpenIDToken, error)
}

// GhostRTCIntent is the optional MatrixRTC 2.0 capability of a GhostIntent (MSC4143 slots, MSC4354
// sticky m.rtc.member, MSC4140 delayed events, MSC4195 homeserver LiveKit tokens; docs/MATRIXRTC2-SPEC.md).
// Callers type-assert it; a ghost without it (or a homeserver returning ErrUnsupported) gets the legacy
// call.member path. Implementations act as the ghost (appservice masquerade) and never put tokens,
// JWTs or query strings into errors.
type GhostRTCIntent interface {
	// SendSticky sends a message event with sticky duration d. Returns the event id.
	SendSticky(ctx context.Context, roomID id.RoomID, t event.Type, content map[string]any, d time.Duration) (id.EventID, error)
	// ScheduleDelayed schedules content to be sent after delay; sticky > 0 makes it a sticky delayed
	// event. New body endpoint first, then the deprecated query form on 404/M_UNRECOGNIZED.
	ScheduleDelayed(ctx context.Context, roomID id.RoomID, t event.Type, content map[string]any, delay, sticky time.Duration) (DelayedEventID, error)
	// UpdateDelayed applies action ("restart", "cancel" or "send") to a scheduled delayed event.
	UpdateDelayed(ctx context.Context, did DelayedEventID, action string) error
	// StateEvent returns the current state content, or (nil, nil) when there is none (404).
	StateEvent(ctx context.Context, roomID id.RoomID, t event.Type, stateKey string) (map[string]any, error)
	// RTCTransports returns the raw body of the MSC4143 transports endpoint (stable path, then
	// unstable), parsed by matrixrtc.ParseTransports.
	RTCTransports(ctx context.Context) ([]byte, error)
	// LiveKitToken requests a LiveKit JWT from the homeserver C-S get_token endpoint (MSC4195, stable
	// path then unstable) as the ghost. Returns the JWT only.
	LiveKitToken(ctx context.Context, req LiveKitTokenRequest) (string, error)
	// Capabilities returns the homeserver's /versions unstable_features (cached; errors give an empty map).
	Capabilities(ctx context.Context) map[string]bool
}

// DelayedEventID is the MSC4140 delay_id of a scheduled delayed event.
type DelayedEventID string

// LiveKitTokenRequest is the MSC4195 get_token request body (url, room_id, slot_id, member_id).
type LiveKitTokenRequest struct {
	URL      string // LiveKit SFU URL; must match the auth service's configured URL exactly
	RoomID   id.RoomID
	SlotID   string
	MemberID string
}

// CallRoom is the Matrix room in which calls with one remote number ring, plus the ghost for that number.
type CallRoom struct {
	RoomID     id.RoomID
	RemoteE164 string
	Ghost      GhostIntent
	// Encrypted is true if the room has m.room.encryption. Calls refuse to ring in encrypted rooms
	// (Element Call would use SFrame E2EE and the bridged audio would decode to silence, ADR-002).
	Encrypted bool
	// Created is true when ResolveCallRoom created the Matrix room in this call (clients may not have
	// synced it yet). Always false from LookupCallRoom.
	Created bool
}

// CallHost is everything the call core needs from the bridge that embeds it.
type CallHost interface {
	// ResolveCallRoom returns the room and ghost for e164 (E.164, e.g. "+15551234567"),
	// creating the portal room if needed.
	ResolveCallRoom(ctx context.Context, login LoginRef, e164 string) (*CallRoom, error)
	// LookupCallRoom returns the CallRoom for an existing room, or ErrNotCallRoom if roomID is not a
	// 1:1 call portal of login (unknown room, other login, group chat). Must not create anything.
	LookupCallRoom(ctx context.Context, login LoginRef, roomID id.RoomID) (*CallRoom, error)
	// SendCallNotice posts a short text notice ("Missed call", "Call · 1m20s") in the room.
	SendCallNotice(ctx context.Context, room *CallRoom, text string) error
	// OwnerMXID returns the Matrix user who owns the login, or "" if the login is unknown.
	OwnerMXID(login LoginRef) id.UserID
	// MarkReadAsOwner sends a read receipt for eventID as the owner (double puppet).
	// Returns ErrNoDoublePuppet if the owner has no double puppet.
	MarkReadAsOwner(ctx context.Context, login LoginRef, roomID id.RoomID, eventID id.EventID) error
}

var (
	// ErrNotCallRoom is returned by LookupCallRoom for rooms that cannot carry calls.
	ErrNotCallRoom = errors.New("core: not a call room")
	// ErrUnknownLogin is returned by hosts when a LoginRef does not belong to any known login.
	ErrUnknownLogin = errors.New("core: unknown login")
	// ErrNoDoublePuppet is returned by MarkReadAsOwner when the owner has no double puppet.
	ErrNoDoublePuppet = errors.New("core: owner has no double puppet")
	// ErrUnsupported is returned by GhostRTCIntent methods when the homeserver does not support the
	// feature (HTTP 404, M_UNRECOGNIZED, or a 400 rejecting the sticky/delay parameter).
	ErrUnsupported = errors.New("core: homeserver does not support this MatrixRTC feature")
	// ErrNotFound is matched (errors.Is) by GhostRTCIntent errors for a homeserver answer of M_NOT_FOUND:
	// the addressed object does not exist (e.g. UpdateDelayed on a delayed event that already fired or
	// expired). It is not ErrUnsupported: the endpoint exists.
	ErrNotFound = errors.New("core: not found on the homeserver")
)
