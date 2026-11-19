package rtc

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// AuthMode selects how the LiveKit JWT is obtained.
type AuthMode int

// Auth modes. The zero value keeps the original OpenID + lk-jwt-service /sfu/get flow.
const (
	AuthSFUGet  AuthMode = iota // OpenID token as the ghost, POST <ServiceURL>/sfu/get (identity "<ghost>:<device>")
	AuthMSC4195                 // homeserver C-S get_token as the ghost (hashed identity); no OpenID step
)

// JoinParams describes one ghost join.
type JoinParams struct {
	Ghost      core.GhostIntent // OpenID token is requested as this ghost
	RoomID     id.RoomID
	DeviceID   string    // same device id as the ghost membership (identity = "<ghost>:<device>")
	ServiceURL string    // lk-jwt-service base URL (no trailing "/")
	Owner      id.UserID // only this user's audio tracks are subscribed
	CallID     string    // logs and spans only
	Attempt    int       // 1-based join attempt, rtc.join.attempt span attribute (0 = unset, omitted)

	// MSC4195 (Auth == AuthMSC4195; all unused for AuthSFUGet except Owners).
	Auth       AuthMode
	SlotID     string   // canonical slot spelling as received/published
	MemberID   string   // get_token member_id = the ghost's member.id
	LiveKitURL string   // transport url: sent to get_token and used as the connect URL
	Owners     []string // initial accepted owner LiveKit identities (exact); Owner's legacy prefix always applies
}

// Joiner joins LiveKit rooms. Join blocks until the track is published or fails (bounded by
// Config.ConnectTimeout plus the HTTP timeouts); ctx cancellation aborts the OpenID and token steps.
type Joiner interface {
	Join(ctx context.Context, p JoinParams) (Session, error)
}

// Session is a joined LiveKit participant publishing one Opus microphone track.
type Session interface {
	Identity() string      // LiveKit identity from the JWT, "<ghost>:<device>"
	LiveKitRoom() string   // LiveKit room name (hashed by lk-jwt-service)
	ServerAddress() string // LiveKit server host
	Audio() media.RTCLeg   // publish Opus; receive owner Opus
	// Events delivers owner presence and disconnects. Capacity 32; sends never block (an event that does
	// not fit is dropped and logged). Closed after Leave returns.
	Events() <-chan Event
	// AddOwnerIdentities accepts more exact owner LiveKit identities (e.g. hashed MSC4195 identities learned
	// from the owner's membership). Thread-safe. It rescans the remote participants, emits EventOwnerJoined
	// for newly matched ones and subscribes their already-published audio. Audio of unmatched participants
	// is never subscribed, read or forwarded.
	AddOwnerIdentities(ids ...string)
	// Leave disconnects (idempotent). Bounded by ctx.
	Leave(ctx context.Context) error
}

// EventKind identifies an Event.
type EventKind int

// Session event kinds.
const (
	EventOwnerJoined  EventKind = iota + 1 // an owner participant (identity "<owner>:<device>") connected
	EventOwnerLeft                         // an owner participant disconnected
	EventDisconnected                      // our own session ended (not by Leave); Reason set
)

// Event is delivered on Session.Events.
type Event struct {
	Kind     EventKind
	Identity string // owner participant identity for OwnerJoined/OwnerLeft
	Reason   string // EventDisconnected: lksdk DisconnectionReason
	At       time.Time
}

// JoinError says which stage failed; Stage values are the rtc.join.duration outcome enum.
type JoinError struct {
	Stage string // "openid_error", "token_error", "connect_error", "publish_error", "timeout"
	Err   error
	// BeforeConnect is true when the join failed before the LiveKit connect was started (OpenID, token
	// or a cancellation during those steps). With AuthMSC4195 it marks failures a switch to sfu_get can fix.
	BeforeConnect bool
}

// Error returns "rtc join: <stage>: <scrubbed err>".
func (e *JoinError) Error() string {
	msg := "<nil>"
	if e.Err != nil {
		msg = obs.Scrub(e.Err.Error())
	}
	return "rtc join: " + e.Stage + ": " + msg
}

// Unwrap returns the underlying error.
func (e *JoinError) Unwrap() error { return e.Err }

// Config configures NewJoiner.
type Config struct {
	// HTTPClient for lk-jwt-service and .well-known. nil = &http.Client{Timeout: 10 s} whose transport is
	// obs.WrapHTTPTransport(http.DefaultTransport, PathTemplate).
	HTTPClient     *http.Client
	ConnectTimeout time.Duration // LiveKit signalling+ICE connect; 0 = 10 s
	Log            zerolog.Logger
	Clock          clock.Clock // nil = clock.Real()
}

// NewJoiner returns the LiveKit-backed Joiner.
func NewJoiner(cfg Config) Joiner {
	return newJoiner(cfg)
}

// SetLibraryLogger routes LiveKit SDK and pion logs through log at Warn+, dropping key/values and
// scrubbing messages (D22). Process-global; the host calls it once at startup.
func SetLibraryLogger(log zerolog.Logger) {
	setLibraryLogger(log)
}

// PathTemplate maps lk-jwt-service and well-known paths to bounded url.template values:
// suffixes "/sfu/get", "/rtc/livekit/get_token" and "/rtc/transports" -> themselves,
// "/.well-known/matrix/client" -> itself, anything else -> "*".
func PathTemplate(path string) string {
	switch {
	case strings.HasSuffix(path, "/sfu/get"):
		return "/sfu/get"
	case strings.HasSuffix(path, "/rtc/livekit/get_token"):
		return "/rtc/livekit/get_token"
	case strings.HasSuffix(path, "/rtc/transports"):
		return "/rtc/transports"
	case path == "/.well-known/matrix/client":
		return path
	default:
		return "*"
	}
}

// ServiceURLResolver returns the lk-jwt-service base URL (D13).
type ServiceURLResolver struct{ resolverState }

// NewServiceURLResolver: override wins when non-empty; otherwise discover from
// https://<serverName>/.well-known/matrix/client. hc nil = the default client above.
func NewServiceURLResolver(override, serverName string, hc *http.Client, clk clock.Clock) *ServiceURLResolver {
	return newServiceURLResolver(override, serverName, hc, clk)
}

// Get returns the URL. Discovery results are cached forever on success; failures are cached for 60 s
// (Get returns the cached error without I/O meanwhile). Concurrent callers share one in-flight fetch.
// Each fetch is bounded by 5 s.
func (r *ServiceURLResolver) Get(ctx context.Context) (string, error) {
	return r.get(ctx)
}

// Source reports "config", "well_known" or "" (unknown yet).
func (r *ServiceURLResolver) Source() string {
	return r.source()
}

// LiveKitRoomName mirrors lk-jwt-service's room alias for the stable slot "m.call#room" (lk-jwt-service
// >= 0.10; <= 0.9 hashed "m.call#ROOM"), for diagnostics only.
func LiveKitRoomName(roomID id.RoomID) string {
	return liveKitRoomName(roomID)
}
