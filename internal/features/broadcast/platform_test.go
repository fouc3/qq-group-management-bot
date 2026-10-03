package broadcast

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

const (
	hereGroup    = "GROUP-HERE"
	otherGroup   = "GROUP-OTHER"
	thirdGroup   = "GROUP-THIRD"
	theAdmin     = "ADMIN-OPENID"
	somebodyElse = "MEMBER-OPENID"
)

// admins is the administrator list, which this feature reads but does not keep.
type admins struct{ who string }

func (a admins) IsAdmin(_, memberOpenID string) bool { return memberOpenID == a.who }

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

// lastText is what the last message sent says.
func (p *platform) lastText() string {
	markdown, _ := p.card()["markdown"].(map[string]any)
	text, _ := markdown["content"].(string)
	return text
}

// press delivers a button press the way the command layer does: the payload is the
// button's own data with its namespace taken off.
func (p *platform) press(t *testing.T, label, member string) {
	t.Helper()
	data, ok := p.cardButtons()[label]
	if !ok {
		t.Fatalf("no button labelled %q on the card:\n%s", label, p.lastText())
	}
	payload := strings.TrimPrefix(data, buttonPrefix)
	press := command.Press{
		Data: &qqbotsdk.InteractionCreateData{
			ID:                "INTERACTION-" + label,
			Scene:             qqbotsdk.InteractionSceneGroup,
			GroupOpenID:       hereGroup,
			GroupMemberOpenID: member,
		},
		EventID: "EVENT-" + label,
		Payload: payload,
	}
	if err := p.handler.onPress(context.Background(), press); err != nil {
		t.Fatalf("pressing %q: %v", label, err)
	}
}

// newPlatform builds the feature over a stand-in platform.
func newPlatform(t *testing.T, options ...func(*platform)) *platform {
	t.Helper()
	p := &platform{proactive: true}
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

	built, err := New(yaml.Node{}, feature.Deps{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Groups: config.Groups{
			{OpenID: hereGroup}, {OpenID: otherGroup}, {OpenID: thirdGroup},
		},
		Buttons: command.NewButtons(),
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	p.handler = built.(*handler)
	p.handler.SetAdminDirectory(admins{who: theAdmin})
	p.handler.SetCommands(commands{looks: false})
	for _, option := range options {
		option(p)
	}
	if err := p.handler.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return p
}

// start opens a card, the way the command does.
func (p *platform) start(t *testing.T, member string) {
	t.Helper()
	data := &qqbotsdk.GroupMessageCreateData{
		ID:          "COMMAND-MESSAGE",
		GroupOpenID: hereGroup,
		Content:     "/群广播",
		Author:      &qqbotsdk.User{MemberOpenID: member},
	}
	if err := p.handler.startCommand(context.Background(), data, command.Parsed{}); err != nil {
		t.Fatalf("opening a card: %v", err)
	}
}

// choose sets the two switches to what the test wants and picks the group the card is
// in.
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

// say delivers what a member wrote, the way a group message arrives.
func (p *platform) say(t *testing.T, member, content string) {
	t.Helper()
	event := qqbotsdk.NewEvent(&qqbotsdk.Payload{
		Op:   qqbotsdk.OpDispatch,
		Type: qqbotsdk.EventGroupMessageCreate,
		Data: json.RawMessage(`{"id":"TEXT-MESSAGE","group_openid":"` + hereGroup +
			`","content":` + quote(content) + `,"author":{"member_openid":"` + member +
			`"}}`),
	}, "test")
	if err := p.handler.onGroupMessage(context.Background(), event); err != nil {
		t.Fatalf("taking the text: %v", err)
	}
}

// quote is a JSON string literal for the events a test builds by hand.
func quote(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}
