// Package broadcast is the feature behind /群广播: an administrator writes one
// message, the bot posts it in the groups they choose, and the group it came from
// is not told which of its administrators asked for it.
//
// The flow is a card rather than a command line. What a broadcast needs to know
// before it is sent -- whether the text is markdown, whether the sender is named,
// which groups it goes to -- is three decisions rather than three arguments, and a
// button cannot be mistyped.
package broadcast

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/messaging"
)

// Name is the feature's name: its key under features: in the file.
const Name = "broadcast"

// Config is the broadcast section.
//
// There is nothing in it beyond the switch every feature has. What a broadcast says
// about itself is written where it is said -- the header, the divider, the buttons
// -- and where it may go is the list of groups this bot manages.
type Config struct{}

// New builds the feature from its own configuration section.
func New(_ yaml.Node, deps feature.Deps) (feature.Feature, error) {
	if deps.Client == nil {
		return nil, errors.New("broadcast: no client to send with")
	}
	h := &handler{
		deps:   deps,
		open:   map[string]*session{},
		router: command.NewRouter(Name, command.ButtonsOrOwn(deps.Buttons)),
	}
	// Work that outlives the event that asked for it runs under this: a card sent
	// from a press, a broadcast posted after the card is gone. Without it, an
	// instance built again while the bot runs would leave the old one still posting.
	h.part, h.stopPart = context.WithCancel(context.Background())
	return h, nil
}

// handler implements feature.Feature.
type handler struct {
	deps feature.Deps

	// admins is the administrator list, injected after building. Who may work a card
	// is the same question as who may run the command, and the answer is the
	// configuration of the feature that owns the table rather than a second list
	// kept here.
	admins feature.AdminDirectory
	// commands is the command table, injected after building. It is how a command
	// typed in the middle of writing a broadcast is told from the text the broadcast
	// is made of.
	commands feature.Commands

	router *command.Router

	part     context.Context
	stopPart context.CancelFunc

	// mu guards open, which is the cards members are looking at.
	mu   sync.Mutex
	open map[string]*session
}

// Name implements feature.Feature.
func (h *handler) Name() string { return Name }

// Intents implements feature.Feature.
//
// A button press carries the choices, and a group message carries the text of the
// broadcast -- the second is why this feature reads messages at all, and it reads
// them for one member at a time rather than reading anything it was not asked for.
func (h *handler) Intents() qqbotsdk.Intent {
	return qqbotsdk.IntentGroupAndC2CEvent | qqbotsdk.IntentInteraction
}

// Register implements feature.Feature.
func (h *handler) Register(context.Context) error {
	return h.router.Register(h.deps.Client, command.Handlers{
		Group: h.onGroupMessage,
		Buttons: []command.ButtonClaim{{
			Namespace: buttonPrefix,
			Scenes:    command.InGroup,
			Handle:    h.onPress,
		}},
	})
}

// Close implements feature.Feature.
//
// The cards go with the instance that opened them: they are held in memory and
// nothing outside can finish one, so a card left behind would answer "过期" to every
// press. The buttons, the handlers and the context are given back the way every
// feature does it.
func (h *handler) Close(context.Context) error {
	h.router.Stop()

	h.mu.Lock()
	h.open = map[string]*session{}
	h.mu.Unlock()

	h.stopPart()
	return nil
}

// SetAdminDirectory implements feature.AdminAware.
func (h *handler) SetAdminDirectory(admins feature.AdminDirectory) { h.admins = admins }

// SetCommands implements feature.CommandAware.
func (h *handler) SetCommands(commands feature.Commands) { h.commands = commands }

// CommandDefs implements feature.CommandSource: the commands this feature answers,
// which the table's owner keeps in the one table beside its own.
func (h *handler) CommandDefs() []command.Def {
	return []command.Def{{
		Name: "群广播",
		// The word an administrator is as likely to say, on the same command:
		// nothing about it changes with the name it is called by.
		Aliases: []string{"群通报"},
		Usage: "{prefix}群广播 —— 打开广播卡片，选好参数后写下要发的内容" +
			"（别名 {prefix}群通报）",
		Desc: "匿名群广播",
		// The gate is the table's, and it is the same list the buttons are checked
		// against: a broadcast is something only this group's administrators do.
		Audience: command.Admins,
		Panels:   []command.PanelPlacement{{Scene: command.InGroup}},
		Run:      h.startCommand,
	}}
}

// startCommand opens a card for the member who asked.
func (h *handler) startCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	_ command.Parsed) error {
	s, err := h.openCard(ctx, data.GroupOpenID, data.Author.MemberOpenID, data.ID)
	if err != nil {
		h.logger(data.GroupOpenID).Warn("could not open a broadcast card", "error", err)
		_, replyErr := h.send(ctx, messaging.Message{
			GroupOpenID: data.GroupOpenID,
			Text:        "广播卡片没能发出来，请稍后再试。",
			ReplyTo:     data.ID,
		})
		return replyErr
	}
	h.deps.Logger.Info("a broadcast card was opened",
		"group", data.GroupOpenID, "member", data.Author.MemberOpenID, "token", s.token)
	return nil
}

// send puts one message where it goes, through the one sender every feature uses.
func (h *handler) send(ctx context.Context, message messaging.Message) (
	*qqbotsdk.MessageResponse, error) {
	return messaging.Send(ctx, h.deps.Client, message)
}

// logger is the feature's logger, with the group it is working in.
func (h *handler) logger(groupOpenID string) *slog.Logger {
	return h.deps.Logger.With("group", groupOpenID)
}

// groupLogLine is what a list of groups is called in a log line.
func groupLogLine(groups []string) string { return strings.Join(groups, " ") }
