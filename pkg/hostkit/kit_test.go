package hostkit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
)

// TestExampleYAMLUnchanged asserts the moved example config is byte-identical to the standalone's
// previous pkg/connector/example-config.yaml (the standalone -e output must not change).
func TestExampleYAMLUnchanged(t *testing.T) {
	golden, err := os.ReadFile("testdata/example-config.golden.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if ExampleYAML != string(golden) {
		t.Fatal("ExampleYAML differs from the previous standalone example-config.yaml")
	}
}

func TestValidatePrefix(t *testing.T) {
	// Every rule of TestValidate, with the jabber prefix: the message names the full key path.
	for _, prefix := range []string{"network.", "network.calls."} {
		for _, tt := range validateCases {
			c := defaultConfig()
			tt.mod(&c)
			err := c.Validate(prefix)
			if err == nil {
				t.Errorf("%s %s: expected error", prefix, tt.name)
				continue
			}
			if !strings.HasPrefix(err.Error(), prefix+tt.key) {
				t.Errorf("%s %s: error %q does not start with %q", prefix, tt.name, err, prefix+tt.key)
			}
			if prefix == "network." && strings.Contains(err.Error(), "network.calls.") {
				t.Errorf("standalone message mentions network.calls.: %q", err)
			}
		}
	}
}

func TestNetConfig(t *testing.T) {
	c := defaultConfig()
	c.SIPBindIP, c.ExternalIP = "10.0.0.1", "203.0.113.9"
	nc := c.NetConfig()
	want := sipaccount.NetConfig{
		SIPBindIP: "10.0.0.1", SIPPortMin: 5060, SIPPortMax: 5069, ExternalIP: "203.0.113.9",
		STUNServer: defaultSTUNServer, RTPPortMin: 40000, RTPPortMax: 40100,
		RegisterExpiry: c.RegisterExpiry(), RegisterRefresh: c.RegisterRefresh(),
	}
	if nc != want {
		t.Fatalf("got %+v want %+v", nc, want)
	}
}

// TestCopyKeysNested runs CopyKeys under a "calls" path (jabber's network.calls) and checks that
// non-default user values survive the upgrade.
func TestCopyKeysNested(t *testing.T) {
	indented := "calls:\n    enabled: false\n"
	for _, l := range strings.SplitAfter(ExampleYAML, "\n") {
		if l != "" {
			indented += "    " + l
		}
	}
	userYAML := "calls:\n    enabled: true\n    external_ip: 203.0.113.9\n    sip_port_min: 5070\n    sip_port_max: 5079\n" +
		"    rtp_timeout_seconds: 90\n    default_country_code: \"44\"\n    dial_allowed_prefixes: []\n" +
		"    allow_self_dial: true\n    dtmf_redact_command: false\n    livekit_service_url: https://lk.example.com/jwt\n"
	out, err := configupgrade.DoBytes([]byte(userYAML), &configupgrade.StructUpgrader{
		SimpleUpgrader: configupgrade.SimpleUpgrader(func(h configupgrade.Helper) {
			h.Copy(configupgrade.Bool, "calls", "enabled")
			CopyKeys(h, "calls")
		}),
		Base: indented,
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Calls struct {
			Enabled bool `yaml:"enabled"`
			Config  `yaml:",inline"`
		} `yaml:"calls"`
	}
	if err = yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	want := defaultConfig()
	no := false
	want.ExternalIP, want.SIPPortMin, want.SIPPortMax, want.RTPTimeoutSeconds = "203.0.113.9", 5070, 5079, 90
	want.DefaultCountryCode, want.DialAllowedPrefixes, want.AllowSelfDial, want.DTMFRedactCommand = "44", []string{}, true, &no
	want.LiveKitServiceURL = "https://lk.example.com/jwt"
	want.Widget.Secret = doc.Calls.Config.Widget.Secret // generated on upgrade
	if len(want.Widget.Secret) != 64 {
		t.Fatalf("nested widget secret not generated: %q", want.Widget.Secret)
	}
	if !doc.Calls.Enabled || !reflect.DeepEqual(doc.Calls.Config, want) {
		t.Fatalf("got enabled=%v %+v\nwant %+v\n%s", doc.Calls.Enabled, doc.Calls.Config, want, out)
	}
}

func TestWithCallOverrides(t *testing.T) {
	member, alias := matrixrtc.CallMember.String(), matrixrtc.CallMemberAlias.String()
	slot, slotAlias := matrixrtc.RTCSlot.String(), matrixrtc.RTCSlotAlias.String()
	if WithCallOverrides(nil) {
		t.Fatal("nil content changed")
	}
	pl := &event.PowerLevelsEventContent{}
	if !WithCallOverrides(pl) || pl.Events[member] != 0 || pl.Events[alias] != 0 || pl.Events[slot] != 0 ||
		pl.Events[slotAlias] != 0 || len(pl.Events) != 4 {
		t.Fatalf("nil map: %v", pl.Events)
	}
	if WithCallOverrides(pl) {
		t.Fatal("already set: reported a change")
	}
	pl = &event.PowerLevelsEventContent{Events: map[string]int{"m.room.name": 50, member: 50, slot: 100}}
	if !WithCallOverrides(pl) || pl.Events[member] != 0 || pl.Events[alias] != 0 || pl.Events[slot] != 0 ||
		pl.Events[slotAlias] != 0 || pl.Events["m.room.name"] != 50 {
		t.Fatalf("other events: %v", pl.Events)
	}
	ov := CallPowerLevelOverrides()
	if len(ov.Events) != 4 || ov.Events[matrixrtc.CallMember] != 0 || ov.Events[matrixrtc.CallMemberAlias] != 0 ||
		ov.Events[matrixrtc.RTCSlot] != 0 || ov.Events[matrixrtc.RTCSlotAlias] != 0 {
		t.Fatalf("overrides = %v", ov.Events)
	}
	if CallPowerLevelOverrides().Events == nil || &ov.Events == &CallPowerLevelOverrides().Events {
		t.Fatal("overrides must be a fresh value")
	}
}

func TestResolveRTCLogin(t *testing.T) {
	portals := map[id.RoomID]*bridgev2.Portal{
		"!a:x": {Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "p", Receiver: "login-a"}}},
		"!b:x": {Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "p", Receiver: "login-b"}}},
		"!c:x": {Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "p"}}},
	}
	lookups := 0
	lookup := func(_ context.Context, room id.RoomID) (*bridgev2.Portal, error) {
		lookups++
		if room == "!err:x" {
			return nil, errors.New("db down")
		}
		return portals[room], nil
	}
	drop := func(sender id.UserID) bool { return sender == "@bot:x" || strings.HasPrefix(string(sender), "@ghost_") }
	accept := func(l networkid.UserLoginID) bool { return l == "login-a" }
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		accept func(networkid.UserLoginID) bool
		sender id.UserID
		room   id.RoomID
		want   core.LoginRef
		ok     bool
	}{
		{"accepted", accept, "@alice:x", "!a:x", "login-a", true},
		{"not accepted", accept, "@alice:x", "!b:x", "", false},
		{"nil accept = all", nil, "@alice:x", "!b:x", "login-b", true},
		{"bot", nil, "@bot:x", "!a:x", "", false},
		{"ghost", nil, "@ghost_1:x", "!a:x", "", false},
		{"no portal", nil, "@alice:x", "!none:x", "", false},
		{"no receiver", nil, "@alice:x", "!c:x", "", false},
		{"lookup error", nil, "@alice:x", "!err:x", "", false},
	} {
		before := lookups
		got, ok := resolveRTCLogin(drop, lookup, tt.accept)(ctx, &event.Event{Sender: tt.sender, RoomID: tt.room})
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: got %q %v", tt.name, got, ok)
		}
		if (tt.name == "bot" || tt.name == "ghost") && lookups != before {
			t.Errorf("%s: dropped sender caused a lookup", tt.name)
		}
	}
}

// fakeLoginDeps returns CommandDeps whose Login accepts the test login (or refuses with refusal).
func fakeLoginDeps(api CallAPI, refusal string) CommandDeps {
	ul := &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login1"}}
	return CommandDeps{
		Calls: func() CallAPI { return api },
		Login: func(*commands.Event) (*bridgev2.UserLogin, string) {
			if refusal != "" {
				return nil, refusal
			}
			return ul, ""
		},
		RoomNumber: func(p *bridgev2.Portal) (string, bool) {
			if strings.HasPrefix(string(p.ID), "+") {
				return string(p.ID), true
			}
			return "", false
		},
		RedactDTMF: true,
	}
}

func portalEvent(ce *commands.Event, portalID networkid.PortalID) *commands.Event {
	ce.Portal = &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: portalID, Receiver: "login1"}, MXID: "!portal:x"}}
	ce.RoomID, ce.OrigRoomID = "!portal:x", "!portal:x"
	return ce
}

func TestCallCommandHandlers(t *testing.T) {
	log := zerolog.Nop()
	user := &bridgev2.User{User: &database.User{MXID: "@alice:x"}}

	// Number argument.
	bot, f := &fakeBot{}, &fakeCalls{e164: "+15551234567"}
	h := &callCommands{d: fakeLoginDeps(f, "")}
	h.cmdCall(newEvent(bot, user, &log, "$c1", "555", "123", "4567"))
	if f.gotReq.Number != "555 123 4567" || !strings.HasPrefix(bot.lastReply(), "Calling +15551234567.") {
		t.Fatalf("number form: req %+v reply %q", f.gotReq, bot.lastReply())
	}

	// Room form in a number room.
	bot, f = &fakeBot{}, &fakeCalls{}
	h = &callCommands{d: fakeLoginDeps(f, "")}
	h.cmdCall(portalEvent(newEvent(bot, user, &log, "$c2"), "+15559876543"))
	if f.gotReq.RoomID != "!portal:x" || !strings.HasPrefix(bot.lastReply(), "Calling +15559876543.") {
		t.Fatalf("room form: req %+v reply %q", f.gotReq, bot.lastReply())
	}

	// Room form refused by RoomNumber: usage, nothing placed.
	bot, f = &fakeBot{}, &fakeCalls{}
	h = &callCommands{d: fakeLoginDeps(f, "")}
	h.cmdCall(portalEvent(newEvent(bot, user, &log, "$c3"), "room@conference.example.org"))
	if f.gotReq != (calls.PlaceCallRequest{}) || !strings.HasPrefix(bot.lastReply(), "Usage: `!jc call <number>`") {
		t.Fatalf("refused room: req %+v reply %q", f.gotReq, bot.lastReply())
	}

	// Destination check fails in the room form.
	bot, f = &fakeBot{}, &fakeCalls{checkErr: calls.ErrNotAllowed}
	h = &callCommands{d: fakeLoginDeps(f, "")}
	h.cmdCall(portalEvent(newEvent(bot, user, &log, "$c4"), "+445551234567"))
	if f.gotReq != (calls.PlaceCallRequest{}) || !strings.Contains(bot.lastReply(), "dial_allowed_prefixes") {
		t.Fatalf("check: req %+v reply %q", f.gotReq, bot.lastReply())
	}

	// Login refusal and not-ready manager.
	bot = &fakeBot{}
	h = &callCommands{d: fakeLoginDeps(&fakeCalls{}, "Log in to Jabber first.")}
	h.cmdHangup(newEvent(bot, user, &log, "$c5"))
	if bot.lastReply() != "Log in to Jabber first." {
		t.Fatalf("refusal reply %q", bot.lastReply())
	}
	bot = &fakeBot{}
	d := fakeLoginDeps(nil, "")
	d.Calls = func() CallAPI { return nil }
	h = &callCommands{d: d}
	h.cmdCall(newEvent(bot, user, &log, "$c6", "+15551234567"))
	if bot.lastReply() != msgNotReady {
		t.Fatalf("not ready reply %q", bot.lastReply())
	}

	// Hangup in a portal passes the room.
	bot, f = &fakeBot{}, &fakeCalls{}
	h = &callCommands{d: fakeLoginDeps(f, "")}
	h.cmdHangup(portalEvent(newEvent(bot, user, &log, "$c7"), "+15559876543"))
	if f.gotRoom != "!portal:x" || bot.lastReply() != "Hanging up." {
		t.Fatalf("hangup: room %q reply %q", f.gotRoom, bot.lastReply())
	}
}

// TestDTMFHandlerRedactsAndHidesDigits checks that the dtmf command message is redacted on success, on
// error and on refusal, and that the digits appear in no reply, span or log line.
func TestDTMFHandlerRedactsAndHidesDigits(t *testing.T) {
	env := obstest.Setup(t)
	var logBuf bytes.Buffer
	log := zerolog.New(&logBuf)
	user := &bridgev2.User{User: &database.User{MXID: "@alice:x"}}
	for _, tt := range []struct {
		name    string
		api     *fakeCalls
		refusal string
	}{
		{"ok", &fakeCalls{n: 5}, ""},
		{"error", &fakeCalls{err: calls.ErrNoActiveCall}, ""},
		{"refused", &fakeCalls{}, "You are not logged in."},
	} {
		bot := &fakeBot{}
		d := fakeLoginDeps(tt.api, tt.refusal)
		h := &callCommands{d: d}
		ce := newEvent(bot, user, &log, "$dtmf-"+id.EventID(tt.name), "98", "76#")
		WithSpan("dtmf", h.cmdDTMF)(ce)
		if !bot.redacted(ce.EventID) {
			t.Errorf("%s: command message not redacted", tt.name)
		}
		if tt.refusal == "" && tt.api.gotDigits != "9876#" {
			t.Errorf("%s: digits = %q", tt.name, tt.api.gotDigits)
		}
		for _, r := range bot.replies() {
			if strings.Contains(r, "9876") || strings.Contains(r, "76#") {
				t.Errorf("%s: reply echoes the digits: %q", tt.name, r)
			}
		}
	}
	// Redaction off: nothing redacted.
	bot := &fakeBot{}
	d := fakeLoginDeps(&fakeCalls{n: 1}, "")
	d.RedactDTMF = false
	(&callCommands{d: d}).cmdDTMF(newEvent(bot, user, &log, "$keep", "1"))
	if bot.redacted("$keep") {
		t.Error("redacted with RedactDTMF=false")
	}
	if n := len(env.SpansByName("cmd.dtmf")); n != 3 {
		t.Errorf("cmd.dtmf spans = %d", n)
	}
	obstest.AssertNoSecrets(t, env.Rec, &logBuf, "9876", "76#")
}

func TestRegisterCallCommands(t *testing.T) {
	br := &bridgev2.Bridge{Log: zerolog.Nop()}
	proc := commands.NewProcessor(br).(*commands.Processor)
	RegisterCallCommands(proc, fakeLoginDeps(&fakeCalls{}, "")) // must not panic

	hs := callHandlers(fakeLoginDeps(&fakeCalls{}, ""))
	var lines []string
	for _, h := range hs {
		if !h.RequiresLogin || h.Help.Section != commands.HelpSectionChats {
			t.Errorf("%s: RequiresLogin=%v section=%v", h.Name, h.RequiresLogin, h.Help.Section)
		}
		help := h.GetHelp()
		lines = append(lines, help.String())
	}
	got := strings.Join(lines, "\n")
	want := "**call** [number] - Call a phone number: your devices ring first, answer to connect the call. Send it without a number inside a number's room to call that number. Usage: `$cmdprefix call <number>`\n" +
		"**dtmf** <digits> - Send keypad tones (0-9 * # A-D) on the active call. Usage: `$cmdprefix dtmf <digits>`\n" +
		"**hangup** - Hang up the active call. Usage: `$cmdprefix hangup`\n" +
		"**widget** - Add the Dialpad widget to this room's widget list. Usage: `$cmdprefix widget`"
	if got != want {
		t.Fatalf("help:\n%s\nwant:\n%s", got, want)
	}
	// The help command replies through ce.Reply, which expands $cmdprefix to the bridge's prefix: the
	// rendered text equals the previous fmt.Sprintf(..., prefix) help.
	bot := &fakeBot{}
	log := zerolog.Nop()
	newEvent(bot, nil, &log, "$help").Reply(got)
	if r := bot.lastReply(); !strings.Contains(r, "Usage: `!jc call <number>`") || strings.Contains(r, "$cmdprefix") {
		t.Errorf("rendered help: %q", r)
	}
}

func TestLoginFields(t *testing.T) {
	fields := LoginFields("44")
	ids := []string{sipaccount.FieldUsername, sipaccount.FieldPassword, sipaccount.FieldRegistrar, sipaccount.FieldTransport, sipaccount.FieldDisplayNumber}
	if len(fields) != len(ids) {
		t.Fatalf("%d fields", len(fields))
	}
	for i, f := range fields {
		if f.ID != ids[i] {
			t.Errorf("field %d = %q, want %q", i, f.ID, ids[i])
		}
	}
	if fields[1].Type != bridgev2.LoginInputFieldTypePassword || !fields[3].Optional || fields[3].DefaultValue != "udp" {
		t.Fatalf("field types: %+v", fields)
	}
	if v, err := fields[4].Validate("7911 123456"); err != nil || v != "+447911123456" {
		t.Fatalf("phone with default CC: %q %v", v, err)
	}
	if v, err := fields[2].Validate("Reg.Example.com"); err != nil || v != "reg.example.com:5060" {
		t.Fatalf("registrar: %q %v", v, err)
	}
}

// TestPromptFields runs the prompt flow through the processor's command state like bridgev2 does.
func TestPromptFields(t *testing.T) {
	var logBuf bytes.Buffer
	log := zerolog.New(&logBuf)
	bot := &fakeBot{}
	user := &bridgev2.User{User: &database.User{MXID: "@alice:x"}}
	var got map[string]string
	doneCalls := 0
	cancelled := false
	start := newEvent(bot, user, &log, "$start")
	PromptFields(start, "SIP login", LoginFields("1"), func(ce *commands.Event, input map[string]string) {
		doneCalls++
		got = input
	}, func() { cancelled = true })

	st := commands.LoadCommandState(user)
	if st == nil || st.Action != "SIP login" || st.Cancel == nil {
		t.Fatalf("state = %+v", st)
	}
	if r := bot.lastReply(); !strings.HasPrefix(r, "Please enter your SIP username\nThe SIP account username.") {
		t.Fatalf("first prompt %q", r)
	}
	answer := func(evt id.EventID, msg string) {
		t.Helper()
		st := commands.LoadCommandState(user)
		if st == nil {
			t.Fatalf("no command state for %s", evt)
		}
		st.Next.Run(newEvent(bot, user, &log, evt, strings.Fields(msg)...))
	}
	answer("$user", "12345678")
	if r := bot.lastReply(); !strings.HasPrefix(r, "Please enter your SIP password") {
		t.Fatalf("password prompt %q", r)
	}
	// The raw message is used, so passwords may contain spaces.
	st = commands.LoadCommandState(user)
	pwEvt := newEvent(bot, user, &log, "$pw")
	pwEvt.RawArgs = "hunter2 secret pw"
	st.Next.Run(pwEvt)
	if !bot.redacted("$pw") {
		t.Fatal("password message not redacted")
	}
	if bot.redacted("$user") {
		t.Fatal("username message redacted")
	}
	answer("$reg-bad", "user@host")
	if r := bot.lastReply(); !strings.HasPrefix(r, "Invalid value: ") {
		t.Fatalf("invalid registrar reply %q", r)
	}
	if st := commands.LoadCommandState(user); st == nil {
		t.Fatal("state cleared after invalid input")
	}
	answer("$reg", "Reg.Example.com:5008")
	if r := bot.lastReply(); !strings.Contains(r, "Please enter your Transport") || !strings.Contains(r, "Options: `udp`, `tcp`") ||
		!strings.Contains(r, "Enter `skip` to skip this field") {
		t.Fatalf("transport prompt %q", r)
	}
	answer("$tr", "skip")
	if doneCalls != 0 {
		t.Fatal("done called early")
	}
	answer("$num", "(555) 123-4567")
	if doneCalls != 1 || commands.LoadCommandState(user) != nil {
		t.Fatalf("done=%d state=%v", doneCalls, commands.LoadCommandState(user))
	}
	want := map[string]string{
		sipaccount.FieldUsername: "12345678", sipaccount.FieldPassword: "hunter2 secret pw",
		sipaccount.FieldRegistrar: "reg.example.com:5008", sipaccount.FieldTransport: "",
		sipaccount.FieldDisplayNumber: "+15551234567",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("input = %v", got)
	}
	creds, err := sipaccount.ParseInput(got, "1")
	if err != nil || creds.Transport != "udp" {
		t.Fatalf("ParseInput: %+v %v", creds, err)
	}
	for _, r := range bot.replies() {
		if strings.Contains(r, "hunter2") {
			t.Fatalf("reply leaks the password: %q", r)
		}
	}
	if strings.Contains(logBuf.String(), "hunter2") {
		t.Fatal("log leaks the password")
	}

	// Cancel callback is stored; a password redaction happens before validation.
	PromptFields(start, "X", []bridgev2.LoginInputDataField{{Type: bridgev2.LoginInputFieldTypePassword, ID: "pw", Name: "pw",
		Validate: func(string) (string, error) { return "", errors.New("bad") }}}, func(*commands.Event, map[string]string) {}, func() { cancelled = true })
	commands.LoadCommandState(user).Cancel()
	if !cancelled {
		t.Fatal("cancel callback not stored")
	}
	answer("$pw2", "x")
	if !bot.redacted("$pw2") || !strings.HasPrefix(bot.lastReply(), "Invalid value: bad") {
		t.Fatal("password not redacted before validation")
	}

	// No fields: done at once.
	n := 0
	PromptFields(start, "Y", nil, func(*commands.Event, map[string]string) { n++ }, nil)
	if n != 1 || commands.LoadCommandState(user) != nil {
		t.Fatal("empty field list")
	}
}

func TestReplyTextExpandsCmdPrefix(t *testing.T) {
	bot := &fakeBot{}
	log := zerolog.Nop()
	replyText(newEvent(bot, nil, &log, "$x"), "Run `$cmdprefix widget` now")
	if r := bot.lastReply(); !strings.Contains(r, "`!jc widget`") || strings.Contains(r, "$cmdprefix") {
		t.Errorf("reply: %q", r)
	}
}
