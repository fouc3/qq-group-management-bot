package broadcast

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

const (
	hereGroup    = "GROUP-HERE"
	otherGroup   = "GROUP-OTHER"
	thirdGroup   = "GROUP-THIRD"
	theAdmin     = "ADMIN-OPENID"
	somebodyElse = "MEMBER-OPENID"
)

// admins is the administrator list, which this feature reads but does not keep.
//
// groups names where the member administers: empty means everywhere, which is what
// most tests want.
type admins struct {
	who    string
	groups []string
}

func (a admins) IsAdmin(groupOpenID, memberOpenID string) bool {
	if a.who == "" || memberOpenID != a.who {
		return false
	}
	if len(a.groups) == 0 {
		return true
	}
	for _, group := range a.groups {
		if group == groupOpenID {
			return true
		}
	}
	return false
}

// commands is the command table, as another feature sees it.
type commands struct{ looks bool }

func (c commands) LooksLikeACommand(string) bool { return c.looks }

// platform is a stand-in for the platform: it records what was sent and taken back,
// and answers the questions the feature asks about a group.
type platform struct {
	handler   *handler
	sent      []map[string]any
	recalls   []string
	answered  []qqbotsdk.InteractionCode
	proactive bool
	// section is the configuration this feature is built from, admins is the list it is
	// handed, and store is the data layer it records into, so that a test can set what
	// it wants before building it.
	section string
	admins  feature.AdminDirectory
	store   store.Store
}

// card is the last card sent, and its buttons.
func (p *platform) card() map[string]any {
	if len(p.sent) == 0 {
		return nil
	}
	return p.sent[len(p.sent)-1]
}

// cardButtons returns the buttons of the card, by label.
//
// Read from the last message that carries a keyboard rather than from the last
// message: what the feature says after a press is a sentence with no buttons on it,
// and the card the member is looking at is still the one being pressed.
func (p *platform) cardButtons() map[string]string {
	buttons := map[string]string{}
	var rows []any
	for index := len(p.sent) - 1; index >= 0 && rows == nil; index-- {
		keyboard, _ := p.sent[index]["keyboard"].(map[string]any)
		content, _ := keyboard["content"].(map[string]any)
		rows, _ = content["rows"].([]any)
	}
	for _, rawRow := range rows {
		row, _ := rawRow.(map[string]any)
		items, _ := row["buttons"].([]any)
		for _, rawItem := range items {
			item, _ := rawItem.(map[string]any)
			render, _ := item["render_data"].(map[string]any)
			action, _ := item["action"].(map[string]any)
			label, _ := render["label"].(string)
			data, _ := action["data"].(string)
			buttons[label] = data
		}
	}
	return buttons
}

// press delivers a button press the way the command layer does: the payload is the
// button's own data with its namespace taken off, and it comes from the single chat the
// card lives in.
func (p *platform) press(t *testing.T, label, member string) {
	t.Helper()
	data, ok := p.cardButtons()[label]
	if !ok {
		t.Fatalf("no button labelled %q on the card:\n%s", label, p.lastText())
	}
	press := command.Press{
		Data: &qqbotsdk.InteractionCreateData{
			ID:         "INTERACTION-" + label,
			Scene:      qqbotsdk.InteractionSceneC2C,
			UserOpenID: member,
		},
		EventID: "EVENT-" + label,
		Payload: strings.TrimPrefix(data, buttonPrefix),
	}
	if err := p.handler.onPress(context.Background(), press); err != nil {
		t.Fatalf("pressing %q: %v", label, err)
	}
}

// notices are the messages that came from this start and are a posted broadcast.
//
// Told apart from everything else the flow says by carrying a divider, which a card, a
// refusal and the answer afterwards do not: whether it is the platform's rule or a run of
// characters depends on how the notice was rendered.
func (p *platform) notices() []string {
	var posted []string
	for _, message := range p.sent {
		markdown, _ := message["markdown"].(map[string]any)
		text, _ := markdown["content"].(string)
		if strings.HasPrefix(text, "**预览**") {
			// What the writer looked at, not what a group read.
			continue
		}
		if strings.Contains(text, "\n"+markdownRule+"\n") ||
			strings.Contains(text, "\n"+plainDivider+"\n") {
			posted = append(posted, text)
		}
	}
	return posted
}

// lastText is what the last message sent says.
func (p *platform) lastText() string {
	markdown, _ := p.card()["markdown"].(map[string]any)
	text, _ := markdown["content"].(string)
	return text
}

// newPlatform builds the feature over a stand-in platform.
//
// The options run before the feature is built, so that a test can say what its
// configuration is or who administers what.
func newPlatform(t *testing.T, options ...func(*platform)) *platform {
	t.Helper()
	p := &platform{
		proactive: true,
		section:   "{}",
		admins:    admins{who: theAdmin},
	}
	for _, option := range options {
		option(p)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			p.sent = append(p.sent, body)
			_, _ = w.Write([]byte(`{"id":"SENT-` + itoa(len(p.sent)) + `"}`))
		case r.Method == http.MethodDelete:
			p.recalls = append(p.recalls, r.URL.Path)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/bot_state"):
			answer := map[string]any{
				"member_openid": "BOT-OPENID", "recv_msg_setting": "all",
				"allow_proactive_msg": p.proactive,
			}
			encoded, _ := json.Marshal(answer)
			_, _ = w.Write(encoded)
		case strings.HasSuffix(r.URL.Path, "/info"):
			// One name per group, so that a test can press the button it means.
			parts := strings.Split(r.URL.Path, "/")
			openID := "?"
			if len(parts) >= 4 {
				openID = parts[3]
			}
			name := "群-" + strings.TrimPrefix(openID, "GROUP-")
			encoded, _ := json.Marshal(map[string]any{"group_name": name})
			_, _ = w.Write(encoded)
		case strings.Contains(r.URL.Path, "/interactions/"):
			code, _ := body["code"].(float64)
			p.answered = append(p.answered, qqbotsdk.InteractionCode(code))
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)

	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     server.URL,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	database, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	p.store = database

	built, err := New(sectionNode(t, p.section), feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Groups: config.Groups{
			{OpenID: hereGroup}, {OpenID: otherGroup}, {OpenID: thirdGroup},
		},
		Buttons: command.NewButtons(),
		Store:   database,
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	p.handler = built.(*handler)
	p.handler.SetAdminDirectory(p.admins)
	p.handler.SetCommands(commands{looks: false})
	if err := p.handler.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return p
}

// sectionNode decodes the configuration a test wrote, the way the registry hands it
// over.
func sectionNode(t *testing.T, text string) yaml.Node {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		t.Fatalf("decoding %q: %v", text, err)
	}
	if len(document.Content) == 0 {
		return yaml.Node{}
	}
	return *document.Content[0]
}

// start opens a card, the way the command does: in a single chat, which is the only
// place a broadcast is written.
func (p *platform) start(t *testing.T, member string) {
	t.Helper()
	data := &qqbotsdk.C2CMessageCreateData{
		ID:      "PRIVATE-MESSAGE",
		Content: "/群广播",
		Author:  &qqbotsdk.User{UserOpenID: member},
	}
	if err := p.handler.startPrivately(context.Background(), data, command.Parsed{}); err != nil {
		t.Fatalf("opening a card: %v", err)
	}
}

// askInGroup is the same command typed in a group, where the draft would be read by
// everyone in it.
func (p *platform) askInGroup(t *testing.T, member string) {
	t.Helper()
	data := &qqbotsdk.GroupMessageCreateData{
		ID:          "COMMAND-MESSAGE",
		GroupOpenID: hereGroup,
		Content:     "/群广播",
		Author:      &qqbotsdk.User{MemberOpenID: member},
	}
	if err := p.handler.startInGroup(context.Background(), data, command.Parsed{}); err != nil {
		t.Fatalf("asking in a group: %v", err)
	}
}

// choose sets the two switches to what the test wants and picks the group the card was
// opened in.
//
// A switch that should end up off is pressed twice: the card starts with neither
// chosen, and one press is what turns it on.
func (p *platform) choose(t *testing.T, markdown, anonymous bool) {
	t.Helper()
	p.press(t, "MD：未选", theAdmin)
	if !markdown {
		p.press(t, "MD：开", theAdmin)
	}
	p.press(t, "匿名：未选", theAdmin)
	if !anonymous {
		p.press(t, "匿名：开", theAdmin)
	}
	p.press(t, "群-HERE", theAdmin)
}

// say delivers what a member wrote, the way a single-chat message arrives.
func (p *platform) say(t *testing.T, member, content string) {
	t.Helper()
	event := qqbotsdk.NewEvent(&qqbotsdk.Payload{
		Op:   qqbotsdk.OpDispatch,
		Type: qqbotsdk.EventC2CMessageCreate,
		Data: json.RawMessage(`{"id":"TEXT-MESSAGE","content":` + quote(content) +
			`,"author":{"user_openid":"` + member + `"}}`),
	}, "test")
	if err := p.handler.onPrivateMessage(context.Background(), event); err != nil {
		t.Fatalf("taking the text: %v", err)
	}
}

// quote is a JSON string literal for the events a test builds by hand.
func quote(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}
