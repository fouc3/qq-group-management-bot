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
		// A card can be opened in a single chat, so the text it is made of can be
		// written in one.
		Private: h.onPrivateMessage,
		Buttons: []command.ButtonClaim{{
			Namespace: buttonPrefix,
			Scenes:    command.InGroup | command.InPrivate,
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
			"（别名 {prefix}群通报；也可在私聊里用）",
		Desc: "匿名群广播",
		// The gate is the table's, and it is the same list the buttons are checked
		// against: a broadcast is something only this group's administrators do.
		Audience: command.Admins,
		// Both panels. In a group it is the group's administrators who may write one;
		// in a single chat it is whoever administers a group somewhere, and the card
		// itself offers only those groups.
		Panels: []command.PanelPlacement{
			{Scene: command.InGroup},
			{Scene: command.InPrivate},
		},
		Run:     h.startCommand,
		Private: h.startPrivately,
	}, {
		// The other half of anonymity: the group is not told who asked, and this is how
		// it can still be found out. In the help rather than in a panel, because a
		// record is something to consult rather than something to put in front of a
		// group.
		Name:     "广播审计",
		Aliases:  []string{"广播记录"},
		Usage:    "{prefix}广播审计 —— 本群最近的广播记录与发起人（只有本群管理员能看）",
		Desc:     "广播记录",
		Audience: command.Admins,
		Run:      h.auditCommand,
	}}
}

// startCommand opens a card for the member who asked in a group.
func (h *handler) startCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	_ command.Parsed) error {
	where := place{groupOpenID: data.GroupOpenID}
	if !h.cfg.allowed(data.Author.MemberOpenID) {
		h.loggerIn(where).Info("a broadcast was asked for by somebody the trial does "+
			"not name", "member", data.Author.MemberOpenID)
		_, err := h.send(ctx, messaging.Message{
			GroupOpenID: data.GroupOpenID,
			Text:        closed,
			ReplyTo:     data.ID,
		})
		return err
	}
	s, err := h.openCard(ctx, where, data.Author.MemberOpenID, data.ID)
	if err != nil {
		h.loggerIn(where).Warn("could not open a broadcast card", "error", err)
		return h.say(ctx, where, data.Author.MemberOpenID, data.ID,
			"广播卡片没能发出来，请稍后再试。")
	}
	h.deps.Logger.Info("a broadcast card was opened", "where", where.String(),
		"member", data.Author.MemberOpenID, "token", s.token)
	return nil
}

// startPrivately opens a card in a single chat.
//
// Offered there because a broadcast is written by one person rather than by a group:
// somebody who administers several groups writes it once and picks where it goes. A
// single chat has no administrator list of its own, so the gate is that this member
// administers a group somewhere -- and the card offers only those groups.
func (h *handler) startPrivately(ctx context.Context, data *qqbotsdk.C2CMessageCreateData,
	_ command.Parsed) error {
	where := place{userOpenID: data.Author.UserOpenID}
	if !h.cfg.allowed(data.Author.UserOpenID) {
		h.loggerIn(where).Info("a broadcast was asked for by somebody the trial does "+
			"not name", "member", data.Author.UserOpenID)
		return h.say(ctx, where, data.Author.UserOpenID, data.ID, closed)
	}
	if len(h.administers(data.Author.UserOpenID)) == 0 {
		return h.say(ctx, where, data.Author.UserOpenID, data.ID,
			"你不在任何群的管理员名单里，广播发不出去。")
	}
	s, err := h.openCard(ctx, where, data.Author.UserOpenID, data.ID)
	if err != nil {
		h.loggerIn(where).Warn("could not open a broadcast card", "error", err)
		return h.say(ctx, where, data.Author.UserOpenID, data.ID,
			"广播卡片没能发出来，请稍后再试。")
	}
	h.deps.Logger.Info("a broadcast card was opened in a single chat",
		"member", data.Author.UserOpenID, "token", s.token)
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

// say answers in one place or the other.
func (h *handler) say(ctx context.Context, where place, memberOpenID, replyTo,
	text string) error {
	message := where.message(text, nil)
	message.ReplyTo = replyTo
	_, err := h.send(ctx, message)
	return err
}

// send puts one message where it goes, through the one sender every feature uses.
func (h *handler) send(ctx context.Context, message messaging.Message) (
	*qqbotsdk.MessageResponse, error) {
	return messaging.Send(ctx, h.deps.Client, message)
}

// loggerIn is the feature's logger, with the group or chat it is working in.
func (h *handler) loggerIn(where place) *slog.Logger {
	if where.inGroup() {
		return h.deps.Logger.With("group", where.groupOpenID)
	}
	return h.deps.Logger.With("chat", where.userOpenID)
}

// logger is the same, for the one question asked before there is a place to ask it of.
func (h *handler) logger(groupOpenID string) *slog.Logger {
	return h.deps.Logger.With("group", groupOpenID)
}

// groupLogLine is what a list of groups is called in a log line.
func groupLogLine(groups []string) string { return strings.Join(groups, " ") }
