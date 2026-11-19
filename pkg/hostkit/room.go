package hostkit

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

// RoomEncrypted reports m.room.encryption from the matrix connector's state store; false (with a
// warning on the ctx logger) when unknown or when br.Matrix is not *matrix.Connector.
func RoomEncrypted(ctx context.Context, br *bridgev2.Bridge, roomID id.RoomID) bool {
	mc, ok := br.Matrix.(*matrix.Connector)
	if !ok {
		zerolog.Ctx(ctx).Warn().Msg("Cannot read room encryption state: not a matrix connector")
		return false
	}
	enc, err := mc.StateStore.IsEncrypted(ctx, roomID)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to read room encryption state")
		return false
	}
	return enc
}

// MarkReadAsOwner sends a read receipt as ul's double puppet; core.ErrNoDoublePuppet if none.
func MarkReadAsOwner(ctx context.Context, ul *bridgev2.UserLogin, roomID id.RoomID, eventID id.EventID) error {
	dp := ul.User.DoublePuppet(ctx)
	if dp == nil {
		return core.ErrNoDoublePuppet
	}
	return dp.MarkRead(ctx, roomID, eventID, time.Now())
}

// callEventTypes are the state events the room owner must be able to send to start or join a call.
var callEventTypes = matrixrtc.CallPowerLevelTypes()

// CallPowerLevelOverrides returns the PL-0 overrides for matrixrtc.CallPowerLevelTypes (call.member and
// rtc.slot, both names), for bridgev2.ChatMemberList.PowerLevels (a new map on every call).
func CallPowerLevelOverrides() *bridgev2.PowerLevelOverrides {
	events := make(map[event.Type]int, len(callEventTypes))
	for _, t := range callEventTypes {
		events[t] = 0
	}
	return &bridgev2.PowerLevelOverrides{Events: events}
}

// WithCallOverrides sets every matrixrtc.CallPowerLevelTypes entry to 0 on pl (allocating the map)
// and reports whether anything changed. Other entries are kept. A nil pl is left alone (false).
func WithCallOverrides(pl *event.PowerLevelsEventContent) (changed bool) {
	if pl == nil {
		return false
	}
	for _, t := range callEventTypes {
		if lvl, ok := pl.Events[t.String()]; ok && lvl == 0 {
			continue
		}
		if pl.Events == nil {
			pl.Events = make(map[string]int)
		}
		pl.Events[t.String()] = 0
		changed = true
	}
	return changed
}

// EnsureCallPowerLevels reads the room's power levels (br.Matrix.GetPowerLevels), applies
// WithCallOverrides and, if changed, sends m.room.power_levels as the bridge bot.
func EnsureCallPowerLevels(ctx context.Context, br *bridgev2.Bridge, roomID id.RoomID) (changed bool, err error) {
	pl, err := br.Matrix.GetPowerLevels(ctx, roomID)
	if err != nil {
		return false, err
	}
	if !WithCallOverrides(pl) {
		return false, nil
	}
	if _, err = br.Bot.SendState(ctx, roomID, event.StatePowerLevels, "", &event.Content{Parsed: pl}, time.Time{}); err != nil {
		return false, err
	}
	return true, nil
}
