package hostkit

import (
	"context"
	"errors"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
)

// ghostIntent adapts a bridgev2 ghost to core.GhostIntent.
type ghostIntent struct {
	api bridgev2.MatrixAPI    // ghost.Intent
	raw *appservice.IntentAPI // ghost.Intent.(*matrix.ASIntent).Matrix
}

var _ core.GhostIntent = (*ghostIntent)(nil)

// NewGhostIntent adapts a bridgev2 ghost to core.GhostIntent. It type-asserts ghost.Intent to
// *matrix.ASIntent (needed for redactions and OpenID tokens); error if that fails.
func NewGhostIntent(ghost *bridgev2.Ghost) (core.GhostIntent, error) {
	if ghost == nil {
		return nil, errors.New("nil ghost")
	}
	as, ok := ghost.Intent.(*matrix.ASIntent)
	if !ok || as == nil {
		return nil, errors.New("ghost intent is not an appservice intent")
	}
	return &ghostIntent{api: ghost.Intent, raw: as.Matrix()}, nil
}

func (g *ghostIntent) UserID() id.UserID { return g.api.GetMXID() }

func (g *ghostIntent) SendState(ctx context.Context, roomID id.RoomID, evtType event.Type, stateKey string, content map[string]any) (id.EventID, error) {
	resp, err := g.api.SendState(ctx, roomID, evtType, stateKey, &event.Content{Raw: content}, time.Time{})
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

func (g *ghostIntent) SendMessage(ctx context.Context, roomID id.RoomID, evtType event.Type, content map[string]any) (id.EventID, error) {
	resp, err := g.api.SendMessage(ctx, roomID, evtType, &event.Content{Raw: content}, nil)
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

func (g *ghostIntent) Redact(ctx context.Context, roomID id.RoomID, eventID id.EventID, reason string) error {
	_, err := g.raw.RedactEvent(ctx, roomID, eventID, mautrix.ReqRedact{Reason: reason})
	return err
}

func (g *ghostIntent) RequestOpenIDToken(ctx context.Context) (*core.OpenIDToken, error) {
	r, err := g.raw.RequestOpenIDToken(ctx)
	if err != nil {
		return nil, err
	}
	return &core.OpenIDToken{
		AccessToken:      r.AccessToken,
		TokenType:        r.TokenType,
		MatrixServerName: r.MatrixServerName,
		ExpiresIn:        r.ExpiresIn,
	}, nil
}
