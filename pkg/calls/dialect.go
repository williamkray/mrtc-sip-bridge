package calls

import (
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

// Dialect is what the owner's Matrix client last published as a sticky membership: the event type
// spelling, slot id spelling and LiveKit URL. The ghost mirrors it later (MATRIXRTC2-SPEC §2).
type Dialect struct {
	TypeName string // evt.Type as received
	SlotID   string // slot_id as received
	URL      string // PreferredLiveKitURL of the membership; "" when it had none
	At       time.Time
}

// dialectFor returns the dialect last seen from the login's owner, if any.
func (m *Manager) dialectFor(login core.LoginRef) (Dialect, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.dialect[login]
	return d, ok
}

// noteDialectLocked records the dialect of a live owner sticky membership. m.mu must be held.
func (m *Manager) noteDialectLocked(login core.LoginRef, mem matrixrtc.Membership, now time.Time) {
	m.dialect[login] = Dialect{TypeName: mem.TypeName, SlotID: mem.SlotID, URL: mem.PreferredLiveKitURL(), At: now}
}

// withdrawNotif drops the owner's pending notification of the room after a sticky withdrawal. Only
// the owner's withdrawal counts; no trigger.
func (m *Manager) withdrawNotif(login core.LoginRef, evt matrixrtc.Event) {
	owner := m.cfg.Host.OwnerMXID(login)
	if owner == "" || evt.Sender != owner {
		return
	}
	m.mu.Lock()
	delete(m.pendingNotif[login], evt.RoomID)
	m.mu.Unlock()
}
