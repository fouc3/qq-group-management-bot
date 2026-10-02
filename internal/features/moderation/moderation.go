package moderation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// Name is this feature's key under features: in the configuration.
const Name = "moderation"

// Defaults for a section that leaves a field out.
const (
	DefaultCacheHours    = 6
	DefaultContextBefore = 10
	DefaultContextAfter  = 10
	DefaultChainMinutes  = 10
)

// The two ways the window around a quoted message is chosen.
const (
	// ScopeGroup follows the group's timeline, which is what judging an
	// advertisement needs: who else said what, and whether anybody objected.
	ScopeGroup = "group"
	// ScopeSender follows the sender alone.
	ScopeSender = "sender"
)

// Config is this feature's section.
type Config struct {
	// Enabled turns the switch on. A section written without it is on.
	Enabled bool `yaml:"enabled"`
	// CacheHours is how long a message stays available to be judged. It is also
	// the latest an advertisement can be reported and still be read.
	CacheHours int `yaml:"cache_hours"`
	// ContextBefore and ContextAfter are how many messages either side of the
	// quoted one may be sent for judging.
	ContextBefore int `yaml:"context_before"`
	ContextAfter  int `yaml:"context_after"`
	// ChainMinutes bounds the window by time as well as by count: the two ends
	// of what is sent may not be further apart than this.
	ChainMinutes int `yaml:"chain_minutes"`
	// ContextScope is ScopeGroup or ScopeSender.
	ContextScope string `yaml:"context_scope"`
}

func (c *Config) applyDefaults() error {
	if c.CacheHours <= 0 {
		c.CacheHours = DefaultCacheHours
	}
	if c.ContextBefore <= 0 {
		c.ContextBefore = DefaultContextBefore
	}
	if c.ContextAfter <= 0 {
		c.ContextAfter = DefaultContextAfter
	}
	if c.ChainMinutes <= 0 {
		c.ChainMinutes = DefaultChainMinutes
	}
	if strings.TrimSpace(c.ContextScope) == "" {
		c.ContextScope = ScopeGroup
	}
	switch c.ContextScope {
	case ScopeGroup, ScopeSender:
	default:
		return fmt.Errorf("context_scope %q is not %s or %s",
			c.ContextScope, ScopeGroup, ScopeSender)
	}
	return nil
}

// handler implements feature.Feature.
type handler struct {
	cfg   Config
	deps  feature.Deps
	cache *Cache
}

// New builds the feature from its configuration section.
func New(section yaml.Node, deps feature.Deps) (feature.Feature, error) {
	var cfg Config
	if err := section.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading the %s section: %w", Name, err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	return &handler{
		cfg:  cfg,
		deps: deps,
		cache: NewCache(deps.Redis, time.Duration(cfg.CacheHours)*time.Hour,
			deps.Logger),
	}, nil
}

// Name implements feature.Feature.
func (h *handler) Name() string { return Name }

// Intents implements feature.Feature.
func (h *handler) Intents() qqbotsdk.Intent {
	// Everything the bot is allowed to receive, because the cache is only as
	// good as what arrives: a group set to deliver only mentions gives this
	// feature almost nothing to judge with.
	return qqbotsdk.IntentGroupAndC2CEvent
}

// Register implements feature.Feature.
func (h *handler) Register(ctx context.Context) error {
	// Said once at startup, because it decides whether a report can be judged
	// with context at all, and a cache that is down is otherwise invisible.
	if err := h.cache.Ping(ctx); err != nil {
		h.deps.Logger.Warn("the message cache is unreachable, so nothing is "+
			"cached and any report will be judged without context",
			"addr", h.deps.Redis.Addr, "error", err)
	} else {
		h.deps.Logger.Info("the message cache is ready",
			"addr", h.deps.Redis.Addr, "retention_hours", h.cfg.CacheHours)
	}

	// The receive setting decides whether this feature can work at all, so it is
	// read and reported rather than assumed. A group that delivers only mentions
	// gives the cache nothing but the messages that mention the bot, and a report
	// about somebody else's advertisement would have nothing to judge it with.
	for _, group := range h.deps.Groups {
		state, err := h.deps.Client.GetGroupBotState(ctx, group.OpenID)
		if err != nil {
			h.deps.Logger.Warn("could not read a group's receive setting",
				"group", group.OpenID, "error", err)
			continue
		}
		h.deps.Logger.Info("a managed group delivers messages this way",
			"group", group.OpenID,
			"recv_msg_setting", state.RecvMsgSetting,
			"proactive_allowed", state.AllowProactiveMsg)
		if state.RecvMsgSetting == qqbotsdk.GroupRecvMsgOnlyMention {
			h.deps.Logger.Warn("this group delivers only messages that mention "+
				"the bot, so the cache will hold almost nothing and a report "+
				"will be judged without context",
				"group", group.OpenID,
				"how_to_change", "the group's bot settings, receive all messages")
		}
	}

	// Both events, so a message is cached whichever way the group delivers it.
	// A message that arrives as both is written twice with the same payload,
	// which the cache treats as one message.
	h.deps.Client.RegisterFunc(qqbotsdk.EventGroupAtMessageCreate, h.onMessage)
	h.deps.Client.RegisterFunc(qqbotsdk.EventGroupMessageCreate, h.onMessage)
	return nil
}

// Close implements feature.Feature.
func (h *handler) Close(context.Context) error { return h.cache.Close() }

// onMessage caches one group message.
//
// Nothing here answers anything: this is the part that makes the cache exist, so
// that a report arriving minutes later can be judged next to what was said
// around it.
func (h *handler) onMessage(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.GroupMessageCreateData)
	if !ok {
		return nil
	}
	if data.Author == nil || data.ID == "" {
		return nil
	}
	if !h.deps.InGroup(data.GroupOpenID) {
		return nil
	}

	// The index the platform puts on a message is what a later quote points at,
	// which is why it is worth storing: without it a report could only be
	// matched to the cached copy by guessing from the text.
	index, _ := data.MessageScene.ExtValue("msg_idx")

	message := CachedMessage{
		ID:   data.ID,
		Idx:  index,
		User: data.Author.MemberOpenID,
		Name: data.Author.Username,
		Type: data.MessageType,
		TS:   sentAt(data),
		Text: data.Content,
	}
	if len(data.Attachments) > 0 {
		// Described rather than stored: the judge needs to know a picture was
		// sent, and downloading it is not this feature's business.
		message.Atts = []string{fmt.Sprintf("%d attachment(s)", len(data.Attachments))}
	}

	if err := h.cache.Record(ctx, data.GroupOpenID, message); err != nil {
		if !errors.Is(err, ErrUnavailable) {
			// Debug, not warn: the cache is allowed to be down, and one line per
			// message would drown everything else in the file.
			h.deps.Logger.Debug("could not cache a group message", "error", err)
		}
	}
	return nil
}

// sentAt is when a message was sent, in Unix milliseconds.
//
// The platform's own timestamp is used when it parses, because the window is
// built on when messages were sent: a message delivered late must keep its own
// place in the timeline rather than jumping to the end of it.
func sentAt(data *qqbotsdk.GroupMessageCreateData) int64 {
	if data.Timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339, data.Timestamp); err == nil {
			return parsed.UnixMilli()
		}
	}
	return time.Now().UnixMilli()
}
