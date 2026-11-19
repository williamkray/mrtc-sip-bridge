package widget

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// EventTypeWidget is the widget state event type (the only one Sable, sable-next and gomuks read).
var EventTypeWidget = event.Type{Type: "im.vector.modular.widgets", Class: event.StateEventType}

// Sweep triggers (the trigger attribute of bridge.widget.sweep).
const (
	TriggerStartup  = "startup"
	TriggerLogin    = "login"
	TriggerPeriodic = "periodic"
	TriggerDisable  = "disable"
)

const (
	queueSize     = 256
	sweepEvery    = 10 * time.Minute
	ensureTimeout = 10 * time.Second
	sweepParallel = 4 // rooms a sweep ensures concurrently (bounded so the homeserver is not flooded)
	widgetName    = "Dialpad"
	urlTemplate   = "?widgetId=$matrix_widget_id&theme=$org.matrix.msc2873.client_theme"
)

var _ hostkit.WidgetInstaller = (*Installer)(nil)

// bridgeState is the StateAPI of a real bridge: reads through br.Matrix.GetStateEvent, writes as the bot.
type bridgeState struct{ br *bridgev2.Bridge }

func (b bridgeState) GetWidgetState(ctx context.Context, roomID id.RoomID, stateKey string) (map[string]any, error) {
	mc, ok := b.br.Matrix.(bridgev2.MatrixConnectorWithArbitraryRoomState)
	if !ok {
		return nil, errors.New("widget: the matrix connector cannot read room state")
	}
	evt, err := mc.GetStateEvent(ctx, roomID, EventTypeWidget, stateKey)
	if errors.Is(err, mautrix.MNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if evt == nil {
		return nil, nil
	}
	if evt.Content.Raw == nil && len(evt.Content.VeryRaw) > 0 {
		_ = json.Unmarshal(evt.Content.VeryRaw, &evt.Content.Raw)
	}
	return evt.Content.Raw, nil
}

func (b bridgeState) SetWidgetState(ctx context.Context, roomID id.RoomID, stateKey string, content map[string]any) error {
	_, err := b.br.Bot.SendState(ctx, roomID, EventTypeWidget, stateKey, &event.Content{Raw: content}, time.Time{})
	return err
}

// job is one unit of installer work: a room (Ensure) or a sweep.
type job struct {
	room    id.RoomID
	title   string
	trigger string            // sweep when non-empty
	from    trace.SpanContext // span the sweep request came from (login trigger link)
}

// Installer adds the Dialpad widget state event to rooms (WIDGET-SPEC §6). All Matrix I/O of Enqueue
// and the sweeps happens on one worker goroutine; Ensure runs synchronously.
type Installer struct {
	d         Deps
	log       zerolog.Logger
	clk       clock.Clock
	publicURL string
	secret    []byte
	stateKey  string
	creator   id.UserID

	queue chan job

	mu   sync.Mutex
	done map[id.RoomID][32]byte // room -> hash of the content last made current (or found forbidden)
}

func newInstaller(cfg hostkit.WidgetConfig, d Deps, log zerolog.Logger) *Installer {
	if d.Clock == nil {
		d.Clock = clock.Real()
	}
	return &Installer{
		d:         d,
		log:       log,
		clk:       d.Clock,
		publicURL: cfg.PublicURL,
		secret:    []byte(cfg.Secret),
		stateKey:  StateKey(d.AppserviceID),
		creator:   d.BotMXID,
		queue:     make(chan job, queueSize),
		done:      make(map[id.RoomID][32]byte),
	}
}

// StateKey returns the widget state key of an appservice ("<id>_dialpad").
func StateKey(appserviceID string) string { return appserviceID + "_dialpad" }

// WidgetURL returns the widget URL for room: the template variables in the query, the room and its
// key in the fragment (never sent to the server).
func (in *Installer) WidgetURL(room id.RoomID) string {
	return in.publicURL + urlTemplate + "#room=" + url.QueryEscape(string(room)) + "&k=" + roomKey(in.secret, room)
}

// Content returns the state event content for room with title ("" = no data.title).
func (in *Installer) Content(room id.RoomID, title string) map[string]any {
	data := map[string]any{}
	if title != "" {
		data["title"] = title
	}
	return map[string]any{
		"type":              "m.custom",
		"url":               in.WidgetURL(room),
		"name":              widgetName,
		"id":                in.stateKey,
		"creatorUserId":     string(in.creator),
		"waitForIframeLoad": true,
		"data":              data,
	}
}

// contentHash hashes content (json.Marshal sorts map keys, so it is canonical).
func contentHash(content map[string]any) [32]byte {
	b, _ := json.Marshal(content)
	return sha256.Sum256(b)
}

// matches reports whether cur already has want's url, name and data.
func matches(cur, want map[string]any) bool {
	if cur == nil {
		return false
	}
	norm := func(v any) any { // round-trip so map[string]any vs decoded JSON compare equal
		b, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(b, &out)
		return out
	}
	for _, k := range []string{"url", "name", "data"} {
		if !reflect.DeepEqual(norm(cur[k]), norm(want[k])) {
			return false
		}
	}
	return true
}

// installResult is the outcome of one ensure.
type installResult string

// Ensure installs or updates the widget in roomID now (the widget command). It always reads the current
// state, even for rooms already done. An M_FORBIDDEN failure satisfies errors.Is(err, mautrix.MForbidden).
func (in *Installer) Ensure(ctx context.Context, roomID id.RoomID, title string) error {
	_, err := in.ensure(ctx, roomID, title)
	return err
}

// ensure does one state read and, when needed, one send. It records the room in done on success and on
// M_FORBIDDEN (no retry loop for rooms where the bot lacks power).
func (in *Installer) ensure(ctx context.Context, roomID id.RoomID, title string) (installResult, error) {
	// The room key is in the widget URL: never put it in a room with other members (it is not marked
	// done, so a later sweep installs it once the room is private).
	if ok, err := in.roomPrivate(ctx, roomID); err != nil {
		recordInstall(ctx, outcomeInstallErr)
		return outcomeInstallErr, err
	} else if !ok {
		obs.Logger(ctx, &in.log).Debug().Stringer("room_id", roomID).Msg("Not installing the Dialpad widget: room has other members")
		recordInstall(ctx, outcomeInstallErr)
		return outcomeInstallErr, ErrNotPrivateRoom
	}
	want := in.Content(roomID, title)
	h := contentHash(want)
	cur, err := in.d.State.GetWidgetState(ctx, roomID, in.stateKey)
	if err != nil {
		if errors.Is(err, mautrix.MForbidden) {
			in.markDone(roomID, h)
		}
		recordInstall(ctx, outcomeInstallErr)
		return outcomeInstallErr, err
	}
	if matches(cur, want) {
		in.markDone(roomID, h)
		recordInstall(ctx, outcomeUnchanged)
		return outcomeUnchanged, nil
	}
	res := installResult(outcomeInstalled)
	if len(cur) > 0 {
		res = outcomeUpdated
	}
	if err = in.d.State.SetWidgetState(ctx, roomID, in.stateKey, want); err != nil {
		if errors.Is(err, mautrix.MForbidden) {
			in.markDone(roomID, h)
			obs.Logger(ctx, &in.log).Debug().Stringer("room_id", roomID).
				Msg("Bot may not send the widget state event in this room")
		}
		recordInstall(ctx, outcomeInstallErr)
		return outcomeInstallErr, err
	}
	in.markDone(roomID, h)
	recordInstall(ctx, string(res))
	return res, nil
}

func (in *Installer) markDone(room id.RoomID, h [32]byte) {
	in.mu.Lock()
	in.done[room] = h
	in.mu.Unlock()
}

// isDone reports whether room is current for title.
func (in *Installer) isDone(room id.RoomID, title string) bool {
	h := contentHash(in.Content(room, title))
	in.mu.Lock()
	defer in.mu.Unlock()
	d, ok := in.done[room]
	return ok && d == h
}

// Enqueue schedules Ensure for roomID on the worker. It never blocks: the room is dropped when the queue
// is full (the next sweep catches it) and skipped when already current.
func (in *Installer) Enqueue(roomID id.RoomID, title string) {
	if roomID == "" || in.isDone(roomID, title) {
		return
	}
	select {
	case in.queue <- job{room: roomID, title: title}:
	default:
	}
}

// RequestSweep schedules a sweep of all eligible rooms not yet done (never blocks). trigger is
// TriggerStartup (root span), TriggerLogin (linked root to from's span) or TriggerPeriodic.
func (in *Installer) RequestSweep(from context.Context, trigger string) {
	select {
	case in.queue <- job{trigger: trigger, from: trace.SpanContextFromContext(from)}:
	default:
	}
}

// start runs the worker and the periodic sweep ticker until ctx ends.
func (in *Installer) start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := in.clk.NewTicker(sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C():
				in.sweep(ctx, TriggerPeriodic, trace.SpanContext{})
			case j := <-in.queue:
				if j.trigger != "" {
					in.sweep(ctx, j.trigger, j.from)
					continue
				}
				if in.isDone(j.room, j.title) {
					continue
				}
				jctx, cancel := context.WithTimeout(ctx, ensureTimeout)
				if _, err := in.ensure(jctx, j.room, j.title); err != nil && !errors.Is(err, mautrix.MForbidden) && !errors.Is(err, ErrNotPrivateRoom) {
					in.log.Warn().Stringer("room_id", j.room).Str("error", obs.Scrub(err.Error())).
						Msg("Failed to install the Dialpad widget")
				}
				cancel()
			}
		}
	}()
}

// sweepStats are the counters of one sweep.
type sweepStats struct{ checked, installed, updated, failed, removed int }

// sweep ensures every eligible room not yet done. Periodic passes start a span only when there are
// unseen rooms.
func (in *Installer) sweep(ctx context.Context, trigger string, from trace.SpanContext) {
	rooms, err := in.d.EligibleRooms(ctx)
	if err != nil {
		in.log.Warn().Str("trigger", trigger).Str("error", obs.Scrub(err.Error())).Msg("Widget sweep: listing rooms failed")
		return
	}
	var todo []RoomRef
	for _, r := range rooms {
		if r.RoomID != "" && !in.isDone(r.RoomID, r.Title) {
			todo = append(todo, r)
		}
	}
	if trigger == TriggerPeriodic && len(todo) == 0 {
		return
	}
	sctx, span := in.startSweepSpan(ctx, trigger, from)
	defer span.End()
	var (
		st    sweepStats
		stMu  sync.Mutex
		wg    sync.WaitGroup
		slots = make(chan struct{}, sweepParallel)
		gctx  = obs.CarrySpan(ctx, sctx) // the room goroutines get the span context, not the live span
	)
	for _, r := range todo {
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(r RoomRef) {
			defer func() { <-slots; wg.Done() }()
			rctx, cancel := context.WithTimeout(gctx, ensureTimeout)
			res, err := in.ensure(rctx, r.RoomID, r.Title)
			cancel()
			stMu.Lock()
			defer stMu.Unlock()
			st.checked++
			switch {
			case err != nil:
				st.failed++
				if !errors.Is(err, mautrix.MForbidden) && !errors.Is(err, ErrNotPrivateRoom) {
					obs.Logger(gctx, &in.log).Debug().Stringer("room_id", r.RoomID).Str("error", obs.Scrub(err.Error())).
						Msg("Widget sweep: room failed")
				}
			case res == outcomeInstalled:
				st.installed++
			case res == outcomeUpdated:
				st.updated++
			}
		}(r)
	}
	wg.Wait()
	in.endSweep(sctx, span, trigger, st)
}

// startSweepSpan starts bridge.widget.sweep: a linked root for login sweeps, a root otherwise.
func (in *Installer) startSweepSpan(ctx context.Context, trigger string, from trace.SpanContext) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrTrigger.String(trigger))}
	if trigger == TriggerLogin && from.IsValid() {
		return obs.StartLinkedRoot(ctx, spanSweep, trace.ContextWithSpanContext(context.Background(), from), opts...)
	}
	return obs.Start(ctx, spanSweep, append(opts, trace.WithNewRoot())...)
}

func (in *Installer) endSweep(ctx context.Context, span trace.Span, trigger string, st sweepStats) {
	span.SetAttributes(attrRoomsChecked.Int(st.checked), attrRoomsInstall.Int(st.installed),
		attrRoomsUpdated.Int(st.updated), attrRoomsFailed.Int(st.failed))
	if trigger == TriggerDisable {
		span.SetAttributes(attrRoomsRemoved.Int(st.removed))
	}
	obs.Logger(ctx, &in.log).Info().Str("trigger", trigger).Int("checked", st.checked).Int("installed", st.installed).
		Int("updated", st.updated).Int("removed", st.removed).Int("failed", st.failed).Msg("Widget sweep done")
}

// Remove is the remove_when_disabled sweep: it sends empty content for our state key in every eligible
// room where it is non-empty. Run it once at startup (in a goroutine) when widget.enabled is false.
// Only AppserviceID, State (or Bridge), EligibleRooms and Log are used from d.
func Remove(ctx context.Context, d Deps) error {
	d.fillFromBridge()
	if d.State == nil || d.EligibleRooms == nil || d.AppserviceID == "" {
		return errors.New("widget: Remove needs State (or Bridge), EligibleRooms and AppserviceID")
	}
	in := newInstaller(hostkit.WidgetConfig{}, d, d.Log.With().Str("component", "widget").Logger())
	rooms, err := d.EligibleRooms(ctx)
	if err != nil {
		return fmt.Errorf("widget: list rooms: %w", err)
	}
	sctx, span := in.startSweepSpan(ctx, TriggerDisable, trace.SpanContext{})
	defer span.End()
	var st sweepStats
	for _, r := range rooms {
		if ctx.Err() != nil {
			break
		}
		st.checked++
		rctx, cancel := context.WithTimeout(sctx, ensureTimeout)
		cur, err := d.State.GetWidgetState(rctx, r.RoomID, in.stateKey)
		if err == nil && len(cur) > 0 {
			err = d.State.SetWidgetState(rctx, r.RoomID, in.stateKey, map[string]any{})
			if err == nil {
				st.removed++
				recordInstall(sctx, outcomeRemoved)
			}
		}
		cancel()
		if err != nil {
			st.failed++
			recordInstall(sctx, outcomeInstallErr)
		}
	}
	in.endSweep(sctx, span, TriggerDisable, st)
	return nil
}
