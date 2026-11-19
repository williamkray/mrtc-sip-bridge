package calls

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// memberSeen is one live owner membership seen by the trigger detector.
type memberSeen struct {
	created time.Time // membership created_ts (or event timestamp)
	eventID id.EventID
	seenAt  time.Time            // when the bridge received it
	mem     matrixrtc.Membership // parsed membership: identities, url and format at trigger time
}

// notifSeen is an owner notification waiting for a fresh owner membership (ring mode).
type notifSeen struct {
	eventID id.EventID
	seenAt  time.Time
	sticky  time.Duration // sticky duration of the notification, 0 = not sticky
}

// within reports whether t is within d of now in either direction (future skew allowed).
func within(now, t time.Time, d time.Duration) bool {
	if t.IsZero() {
		return false
	}
	diff := now.Sub(t)
	return diff <= d && diff >= -d
}

// detect is the owner-trigger detector (PHASE3-SPEC §6.3) for events not routed to an active call. It
// runs on the appservice path: only OwnerMXID, parsing and map operations, never I/O or blocking.
func (m *Manager) detect(ctx context.Context, login core.LoginRef, evt matrixrtc.Event) {
	if evt.Kind != matrixrtc.KindCallMember && evt.Kind != matrixrtc.KindRTCMember && evt.Kind != matrixrtc.KindRTCNotification {
		return
	}
	owner := m.cfg.Host.OwnerMXID(login)
	if owner == "" || evt.Sender != owner {
		return // ghosts, the bot and other users never trigger a call
	}
	now := m.cfg.Clock.Now()
	var c *call
	var refused string
	notif := id.EventID("")
	var notifSticky time.Duration

	switch evt.Kind {
	case matrixrtc.KindCallMember, matrixrtc.KindRTCMember:
		mem, ok := matrixrtc.ParseMembership(evt, now)
		if !ok || mem.UserID != owner {
			return
		}
		fresh := mem.Live && within(now, mem.Created, triggerFreshness) && within(now, evt.Timestamp, triggerFreshness)
		m.mu.Lock()
		_, existed := m.ownerLive[login][evt.RoomID][mem.Key]
		othersLive := m.updateOwnerLiveLocked(login, evt, mem, now)
		trigger := false
		if fresh {
			switch m.mode {
			case modeRing:
				if p, ok := m.pendingNotif[login][evt.RoomID]; ok && now.Sub(p.seenAt) <= triggerPairWindow {
					notif, notifSticky, trigger = p.eventID, p.sticky, true
				}
			case modeMembership:
				trigger = !othersLive && !existed
			}
		}
		if trigger {
			c, refused = m.triggerLocked(ctx, login, evt.RoomID, notif, notifSticky, now)
		}
		m.mu.Unlock()
		if !trigger {
			return
		}
	case matrixrtc.KindRTCNotification:
		if m.mode != modeRing {
			return
		}
		n, ok := matrixrtc.ParseNotification(evt)
		if !ok || n.Withdrawn {
			return
		}
		// A new-format notification has no m.relates_to and no notification_type: pairing never needs
		// either. Only a notification for another slot than the room call is not a trigger.
		if n.SlotID != "" && !matrixrtc.IsRoomSlot(n.SlotID) {
			return
		}
		if !within(now, evt.Timestamp, triggerFreshness) {
			obs.Logger(ctx, &m.log).Debug().Stringer("event_id", evt.EventID).Stringer("room_id", evt.RoomID).
				Msg("Ignoring stale owner call notification")
			return
		}
		m.mu.Lock()
		joined := false
		for _, s := range m.ownerLive[login][evt.RoomID] {
			if within(evt.Timestamp, s.created, freshJoinWindow) {
				joined = true
				break
			}
		}
		if joined {
			notif, notifSticky = evt.EventID, notificationSticky(evt)
			c, refused = m.triggerLocked(ctx, login, evt.RoomID, notif, notifSticky, now)
		} else {
			m.storePendingLocked(login, evt, now)
		}
		m.mu.Unlock()
		if !joined {
			return
		}
	}

	l := obs.Logger(ctx, &m.log)
	if c == nil {
		l.Info().Stringer("room_id", evt.RoomID).Str("reason", refused).Msg("Ignoring owner call trigger")
		return
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(obs.AttrMatrixRTCRoutedTo.String("call"), obs.AttrCallID.String(c.id))
	l.Info().Stringer("room_id", evt.RoomID).Str("call_id", c.id).Bool("busy", c.busy).
		Stringer("notification_id", notif).Msg("Owner call trigger")
	go c.run()
}

// updateOwnerLiveLocked records (or removes) an owner membership and reports whether another owner
// device was live in that room before this event. m.mu must be held.
func (m *Manager) updateOwnerLiveLocked(login core.LoginRef, evt matrixrtc.Event, mem matrixrtc.Membership, now time.Time) bool {
	rooms := m.ownerLive[login]
	if rooms == nil {
		rooms = make(map[id.RoomID]map[string]memberSeen)
		m.ownerLive[login] = rooms
	}
	if mem.Live {
		// Prune on insert.
		retention := ownerLiveRetention
		if m.mode == modeMembership {
			retention = matrixrtc.DefaultMembershipExpiry
		}
		for room, ds := range rooms {
			for k, s := range ds {
				if now.Sub(s.seenAt) > retention {
					delete(ds, k)
				}
			}
			if len(ds) == 0 {
				delete(rooms, room)
			}
		}
	}
	devs := rooms[evt.RoomID]
	others := false
	for k := range devs {
		if k != mem.Key {
			others = true
			break
		}
	}
	if !mem.Live {
		delete(devs, mem.Key)
		if len(devs) == 0 {
			delete(rooms, evt.RoomID)
		}
		if len(rooms) == 0 {
			delete(m.ownerLive, login)
		}
		return others
	}
	if devs == nil {
		devs = make(map[string]memberSeen)
		rooms[evt.RoomID] = devs
	}
	devs[mem.Key] = memberSeen{created: mem.Created, eventID: evt.EventID, seenAt: now, mem: mem}
	if mem.Format == matrixrtc.FormatSticky {
		m.noteDialectLocked(login, mem, now)
	}
	return others
}

// noteRoutedMembership updates the detector's owner device map for a membership routed to a call.
func (m *Manager) noteRoutedMembership(login core.LoginRef, evt matrixrtc.Event) {
	owner := m.cfg.Host.OwnerMXID(login)
	if owner == "" || evt.Sender != owner {
		return
	}
	now := m.cfg.Clock.Now()
	mem, ok := matrixrtc.ParseMembership(evt, now)
	if !ok || mem.UserID != owner {
		return
	}
	m.mu.Lock()
	m.updateOwnerLiveLocked(login, evt, mem, now)
	m.mu.Unlock()
}

// storePendingLocked stores an owner notification waiting for its membership (one per room), pruning
// expired entries. m.mu must be held.
func (m *Manager) storePendingLocked(login core.LoginRef, evt matrixrtc.Event, now time.Time) {
	rooms := m.pendingNotif[login]
	if rooms == nil {
		rooms = make(map[id.RoomID]notifSeen)
		m.pendingNotif[login] = rooms
	}
	for room, p := range rooms {
		if now.Sub(p.seenAt) > triggerPairWindow {
			delete(rooms, room)
		}
	}
	rooms[evt.RoomID] = notifSeen{eventID: evt.EventID, seenAt: now, sticky: notificationSticky(evt)}
}

// setCooldownLocked starts the per-room trigger cooldown, pruning expired entries. m.mu must be held.
func (m *Manager) setCooldownLocked(room id.RoomID, now time.Time) {
	if room == "" {
		return
	}
	for r, until := range m.cooldown {
		if !now.Before(until) {
			delete(m.cooldown, r)
		}
	}
	m.cooldown[room] = now.Add(triggerCooldown)
}

// triggerLocked creates the actor for an owner trigger, or returns the reason it was refused. An active
// call in another room yields a busy actor (not active, not routable). The caller starts the actor after
// releasing m.mu. m.mu must be held.
func (m *Manager) triggerLocked(ctx context.Context, login core.LoginRef, room id.RoomID, notif id.EventID,
	notifSticky time.Duration, now time.Time) (*call, string) {
	if p, ok := m.pendingNotif[login][room]; ok && p.eventID == notif {
		delete(m.pendingNotif[login], room)
	}
	switch {
	case m.stopped:
		return nil, "manager stopped"
	case m.cfg.Dial == nil:
		return nil, "outbound calls disabled"
	case m.mode == modeOff:
		return nil, "owner trigger off"
	case now.Before(m.cooldown[room]):
		return nil, "room cooldown"
	}
	if a := m.active[login]; a != nil && (m.routable[login] != a || a.roomID == room) {
		// The active call is in this room, or its room is not resolved yet (not routable): no busy actor,
		// so nothing is declined or noticed in the room the owner is already calling in.
		return nil, "line busy, same room"
	}
	c := newOutCall(m, login, triggerOwnerMembership, trace.SpanContextFromContext(ctx), now)
	c.roomID = room
	c.ownerNotif = notif
	c.ownerNotifSticky = notifSticky
	if m.active[login] != nil {
		c.busy = true
		m.setCooldownLocked(room, now)
	} else {
		for k, s := range m.ownerLive[login][room] {
			c.devices[k] = struct{}{}
			if !s.mem.Live {
				continue
			}
			// Every live owner device may be the one that joins LiveKit, so all of their identities are
			// accepted (deduplicated, capped); the freshest membership is the dialect source.
			c.learnOwnerIdentities(s.mem)
			if c.ownerMem.Key == "" || !s.mem.Created.Before(c.ownerMem.Created) {
				c.ownerMem = s.mem
				c.ownerFormat = s.mem.Format
			}
		}
		m.active[login] = c
		m.routable[login] = c
	}
	m.calls[c] = struct{}{}
	return c, ""
}

// prefixAllowed reports whether e164 matches Config.DialAllowedPrefixes (empty = any).
func (m *Manager) prefixAllowed(e164 string) bool {
	if len(m.cfg.DialAllowedPrefixes) == 0 {
		return true
	}
	for _, p := range m.cfg.DialAllowedPrefixes {
		if p != "" && len(e164) >= len(p) && e164[:len(p)] == p {
			return true
		}
	}
	return false
}

// CheckDestination applies the destination guards (E.164 shape, NANP validity, dial_allowed_prefixes,
// self-dial) to e164 without placing a call. It returns nil, ErrInvalidNumber, ErrNotAllowed or ErrSelfDial.
func (m *Manager) CheckDestination(login core.LoginRef, e164 string) error {
	switch {
	case !phone.IsE164(e164) || !validNANP(e164):
		return ErrInvalidNumber
	case !m.prefixAllowed(e164):
		return ErrNotAllowed
	case m.isSelf(login, e164):
		return ErrSelfDial
	}
	return nil
}

// validNANP rejects +1 numbers that are not valid NANP numbers: wrong length, area code or exchange
// starting with 0/1, N11 codes, and the 900 and 976 premium codes. Other country codes pass.
func validNANP(e164 string) bool {
	if !strings.HasPrefix(e164, "+1") {
		return true
	}
	if len(e164) != 12 {
		return false
	}
	area, exch := e164[2:5], e164[5:8]
	for _, p := range []string{area, exch} {
		if p[0] < '2' || p[0] > '9' || p[1:] == "11" {
			return false
		}
	}
	return area != "900" && area != "976"
}

// isSelf reports whether e164 is the login's own number and self-dial is not allowed.
func (m *Manager) isSelf(login core.LoginRef, e164 string) bool {
	if m.cfg.AllowSelfDial || m.cfg.LoginNumber == nil {
		return false
	}
	own := m.cfg.LoginNumber(login)
	return own != "" && own == e164
}

// notificationSticky returns the sticky duration of an owner notification, 0 when it is not sticky. The
// duration comes from the event (sticky or unsigned TTL); a notification that only carries the sticky
// key counts as sticky with the minimum duration (appservice pushes may omit the sticky fields).
func notificationSticky(evt matrixrtc.Event) time.Duration {
	if d := max(evt.StickyDuration, evt.StickyRemaining); d > 0 {
		return d
	}
	var c struct {
		Key *string `json:"msc4354_sticky_key"`
	}
	if json.Unmarshal(evt.Content, &c) == nil && c.Key != nil {
		return matrixrtc.NotificationStickyMin
	}
	return 0
}
