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
	"fmt"
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
// What a broadcast says about itself is written where it is said -- the header, the
// divider, the buttons -- and where it may go is decided by who administers what.
type Config struct {
	// Beta gates the feature while it is being tried out.
	//
	// A gate of its own rather than a switch, because the two questions are
	// different: whether the feature runs at all, and who may use it while it does.
	Beta Beta `yaml:"beta"`
}

// Beta is the closed-trial gate.
type Beta struct {
	// Enabled turns the gate on: only the members it names may use the feature.
	Enabled bool `yaml:"enabled"`
	// Whitelist are the members who may use it while the gate is on, by openid.
	//
	// Somebody is named twice when they are wanted in both places, because a member's
	// openid in a group is not the openid they have in a single chat: what a group
	// carries and what the private panel carries are different values.
	Whitelist []string `yaml:"whitelist"`
}

// allowed reports whether a member may use the feature now.
func (c Config) allowed(memberOpenID string) bool {
	if !c.Beta.Enabled {
		return true
	}
	for _, named := range c.Beta.Whitelist {
		if named != "" && named == memberOpenID {
			return true
		}
	}
	return false
}

// closed is what a member the trial does not name is told.
//
// Said rather than left silent: a command that answers nothing looks like a bot that
// is broken, and the fact that the feature exists and is not open yet is not a secret.
const closed = "功能正在内测阶段，暂无法使用。"

// New builds the feature from its own configuration section.
func New(section yaml.Node, deps feature.Deps) (feature.Feature, error) {
	if deps.Client == nil {
		return nil, errors.New("broadcast: no client to send with")
	}
	var cfg Config
	if err := section.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading the %s section: %w", Name, err)
	}
	h := &handler{
		cfg:    cfg,
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
	cfg  Config
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
// A button press carries the choices, and a single-chat message carries the text of the
// broadcast -- the second is why this feature reads messages at all, and it reads them
// for one member at a time rather than reading anything it was not asked for. The group
// intent is in here because the platform delivers a single-chat message under it, not
// because this feature reads anything said in a group.
func (h *handler) Intents() qqbotsdk.Intent {
	return qqbotsdk.IntentGroupAndC2CEvent | qqbotsdk.IntentInteraction
}

// Register implements feature.Feature.
//
// A card lives in a single chat, so that is where both the presses and the text come
// from. Nothing of this feature reads a group: a broadcast is written away from the group
// it is for.
func (h *handler) Register(context.Context) error {
	return h.router.Register(h.deps.Client, command.Handlers{
		Private: h.onPrivateMessage,
		Buttons: []command.ButtonClaim{{
			Namespace: buttonPrefix,
			Scenes:    command.InPrivate,
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
		// The word somebody is as likely to say, on the same command: nothing about it
		// changes with the name it is called by.
		Aliases: []string{"群通报"},
		Usage:   "{prefix}群广播 —— 私聊里打开广播卡片，选好参数后写下要发的内容",
		Desc:    "匿名群广播",
		// The gate is the table's, and it is the same list the card's buttons are
		// checked against: broadcasting into a group is something its administrators do.
		Audience: command.Admins,
		// The card is opened in a single chat, so that is the panel it belongs in: a notice
		// being drafted -- its options, and the preview of it -- is not something the group
		// it is for should be reading.
		Panels: []command.PanelPlacement{{Scene: command.InPrivate}},
		// Typed in a group it opens nothing and says where a broadcast is written instead,
		// because nothing of this flow happens in a group: the card, the choices and the
		// text are all in a single chat, and a group only ever receives the notice itself.
		Run:     h.startInGroup,
		Private: h.startPrivately,
	}, {
		// The other half of anonymity: the group is not told who asked, and this is how
		// it can still be found out. In the help rather than in a panel, because a record
		// is something to consult rather than something to put in front of a group.
		Name:    "广播审计",
		Aliases: []string{"广播记录"},
		Usage: "{prefix}广播审计 —— 本群最近的广播记录与发起人（只有本群管理员能看）；" +
			"在私聊里用则列出你管理的群",
		PrivateUsage: "{prefix}广播审计 —— 你管理的群最近发过哪些广播、各是谁发起的",
		Desc:         "广播记录",
		Audience:     command.Admins,
		Run:          h.auditCommand,
		Private:      h.auditPrivately,
	}}
}

// startInGroup says where a broadcast is written, for a group that asked for one.
//
// What it must not say is that anything of this flow happens here: nothing does. The card,
// the choices and the text are all in a single chat, and the group only receives the
// notice they add up to -- so a group that was told "the draft would go here" would be
// waiting for something that never comes.
func (h *handler) startInGroup(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	_ command.Parsed) error {
	if !h.cfg.allowed(data.Author.MemberOpenID) {
		h.logger(data.GroupOpenID).Info("a broadcast was asked for by somebody the "+
			"trial does not name", "member", data.Author.MemberOpenID)
		return h.say(ctx, data.GroupOpenID, data.ID, closed)
	}
	h.logger(data.GroupOpenID).Info("a broadcast was asked for in a group, so it was "+
		"answered where to write one", "member", data.Author.MemberOpenID)
	return h.say(ctx, data.GroupOpenID, data.ID,
		"广播在私聊里写：请私聊机器人发送 /群广播。参数和正文都在私聊里填，"+
			"本群只会收到最后那条广播。")
}

// startPrivately opens a card in a single chat.
//
// It is only opened here: a broadcast is one person writing a notice for groups to read,
// and the group is exactly who is not supposed to watch it being written. A single chat
// has no administrator list of its own, so the gate is that this member administers a
// group somewhere -- and the card offers only those groups.
func (h *handler) startPrivately(ctx context.Context, data *qqbotsdk.C2CMessageCreateData,
	_ command.Parsed) error {
	chat := data.Author.UserOpenID
	if !h.cfg.allowed(chat) {
		h.loggerIn(chat).Info("a broadcast was asked for by somebody the trial does "+
			"not name", "member", chat)
		return h.say(ctx, chat, data.ID, closed)
	}
	if len(h.administers(chat)) == 0 {
		return h.say(ctx, chat, data.ID, "你不在任何群的管理员名单里，广播发不出去。")
	}
	s, err := h.openCard(ctx, chat, data.ID)
	if err != nil {
		h.loggerIn(chat).Warn("could not open a broadcast card", "error", err)
		return h.say(ctx, chat, data.ID, "广播卡片没能发出来，请稍后再试。")
	}
	h.deps.Logger.Info("a broadcast card was opened", "member", chat, "token", s.token)
	return nil
}

// administers are the groups this member is on the administrator list of.
func (h *handler) administers(memberOpenID string) []string {
	var groups []string
	for _, group := range h.deps.Groups {
		if h.adminsOf(group.OpenID, memberOpenID) {
			groups = append(groups, group.OpenID)
		}
	}
	return groups
}

// say answers in a group or in a single chat, as a passive reply where there is one.
//
// The destination is told apart by which of the two identifiers is filled in, because
// that is what the single sender takes: a group and a single chat are two endpoints
// rather than one with a flag.
func (h *handler) say(ctx context.Context, to, replyTo, text string) error {
	message := messaging.Message{Text: text, ReplyTo: replyTo}
	if h.isChat(to) {
		message.UserOpenID = to
	} else {
		message.GroupOpenID = to
	}
	_, err := h.send(ctx, message)
	return err
}

// isChat reports whether an identifier is somebody's, rather than a group's.
//
// The two come from the same set of calls and never overlap in practice: a group's openid
// is what the bot's own configuration names, and a member's is what an event carries.
func (h *handler) isChat(openID string) bool {
	for _, group := range h.deps.Groups {
		if group.OpenID == openID {
			return false
		}
	}
	return true
}

// send puts one message where it goes, through the one sender every feature uses.
func (h *handler) send(ctx context.Context, message messaging.Message) (
	*qqbotsdk.MessageResponse, error) {
	return messaging.Send(ctx, h.deps.Client, message)
}

// loggerIn is the feature's logger, with the single chat it is working in.
func (h *handler) loggerIn(chat string) *slog.Logger {
	return h.deps.Logger.With("chat", chat)
}

// logger is the same, with the group a question is about.
func (h *handler) logger(groupOpenID string) *slog.Logger {
	return h.deps.Logger.With("group", groupOpenID)
}

// groupLogLine is what a list of groups is called in a log line.
func groupLogLine(groups []string) string { return strings.Join(groups, " ") }
