package moderation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	openai "github.com/sashabaranov/go-openai"
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

	// Model is how the judge is reached. It is deliberately optional: a
	// deployment that only fills the cache is a legitimate stage to run in, and
	// a judgement with no model configured is a failure rather than a guess.
	Model Model `yaml:"model"`
	// Categories are the kinds of violation the bot may punish, by name. The
	// model may only choose among these; anything else is not acted on.
	Categories map[string]Category `yaml:"categories"`
	// MinConfidence is how sure the model has to be before anything happens.
	// A violation below it is treated as no judgement at all.
	MinConfidence float64 `yaml:"min_confidence"`
	// MaxChars bounds what is sent for judgement. A longer window is truncated,
	// with the truncation said out loud in the prompt.
	MaxChars int `yaml:"max_chars"`
	// DefaultMute is used for a category that names no duration of its own, which
	// is what "different times per type, or one fixed time" comes to. Empty means
	// the finding is reported without silencing anybody.
	DefaultMute string `yaml:"default_mute"`
	// ReportPenalty silences the reporter when a report finds nothing. Off unless
	// the section turns it on, because it punishes somebody for being wrong rather
	// than for doing wrong.
	ReportPenalty *Penalty `yaml:"report_penalty"`
	// Groups holds one group's differences from the section above it.
	Groups map[string]GroupOverride `yaml:"groups"`
	// DryRun judges and reports without muting or recalling anything.
	//
	// On unless it is turned off: the first days of a moderation prompt are for
	// reading, not for silencing people.
	DryRun *bool `yaml:"dry_run"`
}

// Model is the OpenAI-compatible endpoint the judge asks.
type Model struct {
	BaseURL        string  `yaml:"base_url"`
	APIKey         string  `yaml:"api_key"`
	Name           string  `yaml:"name"`
	TimeoutSeconds int     `yaml:"timeout_seconds"`
	Stream         bool    `yaml:"stream"`
	Temperature    float32 `yaml:"temperature"`
	// Thinking says what to do about the model's chain of thought.
	//
	// The switch is worth a configuration field of its own because of what its
	// absence costs: measured against DeepSeek's own endpoint, a request without it
	// came back in five output tokens with an empty reasoning_content -- the model
	// answered a moderation question without reasoning about it at all, which is
	// the shallow judgement this field exists to stop.
	//
	//	hide  (default) thinking on, and the chain is thrown away
	//	show            thinking on, and the chain goes to the log, which is how a
	//	                surprising verdict gets explained afterwards
	//	off             thinking off: the fastest and cheapest answer
	//	""              nothing is sent, and the provider's own default applies
	Thinking string `yaml:"thinking"`
	// ReasoningEffort is how hard it thinks: low, medium, high or max. Empty sends
	// nothing, and the provider's own default -- high -- applies.
	ReasoningEffort string `yaml:"reasoning_effort"`
	// MaxTokens caps the answer, and the chain of thought is paid for out of it.
	// Too small a cap leaves the model mid-sentence with no JSON to read, which the
	// judgement treats as no judgement at all -- and since the chain of thought is
	// paid for out of the same budget, a cap that looks generous for one small JSON
	// object can still be tight.
	MaxTokens int `yaml:"max_tokens"`
	// Retries is how many times an answer that could not be read is asked for again,
	// at most three. Nil means the default; zero means never, which is a real choice
	// for somebody paying per call.
	Retries *int `yaml:"retries"`
}

// defaultJudgeRetries is how many times an unreadable answer is asked for again when
// the configuration says nothing.
const defaultJudgeRetries = 2

// judgeRetries is how many retries are in force.
func (c *Config) judgeRetries() int {
	if c.Model.Retries == nil {
		return defaultJudgeRetries
	}
	return *c.Model.Retries
}

// Category is one kind of violation.
type Category struct {
	// Label is what the group is told when this category is found. It comes from
	// the configuration and never from the model, so the public reply cannot
	// carry text a member talked the model into writing.
	Label string `yaml:"label"`
	// Mute is how long the member is silenced for it.
	Mute string `yaml:"mute"`
}

// Penalty is a punishment the configuration asks for, in the same duration syntax
// as a category.
type Penalty struct {
	// Enabled turns it on. A block written without it is off.
	Enabled bool `yaml:"enabled"`
	// Mute is how long it lasts.
	Mute string `yaml:"mute"`
}

// GroupOverride is one group's differences from the section above it.
//
// Every field is optional, so a group that appears here only to carry an allow
// list keeps every other setting from the feature itself.
type GroupOverride struct {
	// Enabled turns judging off for one group while it stays on elsewhere. Nil
	// follows the feature.
	Enabled *bool `yaml:"enabled"`
	// Allow is what this group calls its own: its site, its announcement page.
	//
	// It is not a veto, and it cannot be one. A rule about text is satisfied by
	// the text, so an exemption keyed on it is handed to whoever writes the
	// message: both attempts at one were defeated by the obvious trick, and both
	// are written down in allowedIn. What the list can do is tell the judge what
	// this group considers its own, so that the judge can weigh a message which
	// names it next to something else -- which is a judgement, and belongs there.
	Allow []string `yaml:"allow"`
	// AllowSenders are the openids whose messages are never judged at all.
	//
	// This is the one exemption the code makes, because it is about identity
	// rather than content: an announcement from the group's own account cannot be
	// imitated by writing something convincing.
	AllowSenders []string `yaml:"allow_senders"`
	// Categories overrides durations for this group. A category named here takes
	// this duration; every other category is unchanged.
	Categories map[string]string `yaml:"categories"`
}

// groupFor is one group's overrides, or an empty set.
func (c *Config) groupFor(groupOpenID string) GroupOverride {
	if c.Groups == nil {
		return GroupOverride{}
	}
	return c.Groups[groupOpenID]
}

// judgingEnabledFor reports whether judging happens in one group at all.
func (c *Config) judgingEnabledFor(groupOpenID string) bool {
	if enabled := c.groupFor(groupOpenID).Enabled; enabled != nil {
		return *enabled
	}
	return true
}

// allowedIn reports whether a message is one this group has declared legitimate.
//
// It always says no, and that is deliberate: the content exemption it used to
// provide has been removed rather than repaired, because every version of it was
// unsound for the same reason -- it was a rule about text, applied to text the
// person being judged writes.
//
// Two versions, both defeated by the obvious trick:
//
//   - matching a word anywhere in the message. The advertisement that carries a
//     whitelisted word as a shield -- "deepseek0.01x https://q1.1110103.xyz/
//     防屏蔽：api.mcapple.top" -- was never sent to the judge at all;
//   - matching domains, and exempting only when every domain in the message is
//     allowed. Write the advertisement's own domain so the extractor cannot see
//     it, by breaking it up the way spammers already do, and the only domain left
//     visible is the allowed one: exempt again.
//
// The lesson is not that the matching needed to be better. It is that an
// exemption keyed on what a message says can always be satisfied by what the
// message says. What a group means by "legitimate" is a judgement, and judgements
// belong to the judge: the group's list now goes into the prompt as context, and
// the only exemption left in code is about who sent the message.
func (c *Config) allowedIn(groupOpenID, text string) (string, bool) {
	return "", false
}

// senderExempt reports whether a group has put one sender beyond judging.
//
// Identity, not content, which is what makes it the one exemption worth having:
// there is no sentence somebody can write to become the group's own account.
func (c *Config) senderExempt(groupOpenID, memberOpenID string) bool {
	if strings.TrimSpace(memberOpenID) == "" {
		return false
	}
	for _, entry := range c.groupFor(groupOpenID).AllowSenders {
		if strings.TrimSpace(entry) == memberOpenID {
			return true
		}
	}
	return false
}

// allowText is the group's own list as a sentence for the prompt.
func (c *Config) allowText(groupOpenID string) string {
	entries := c.groupFor(groupOpenID).Allow
	var kept []string
	for _, entry := range entries {
		if strings.TrimSpace(entry) != "" {
			kept = append(kept, strings.TrimSpace(entry))
		}
	}
	if len(kept) == 0 {
		return "（该群没有声明任何属于它自己的内容）"
	}
	return strings.Join(kept, "、")
}

// MuteForGroup is how long a member is silenced for one category in one group,
// falling back to the section's own setting.
func (c *Config) MuteForGroup(groupOpenID, category string) (int64, bool) {
	if text := strings.TrimSpace(c.groupFor(groupOpenID).Categories[category]); text != "" {
		parsed, err := time.ParseDuration(text)
		if err != nil {
			// Refused at startup, so this cannot happen in a running bot.
			return 0, false
		}
		return int64(parsed.Seconds()), true
	}
	return c.MuteFor(category)
}

// modelClient builds the client the judge uses.
func (h *handler) modelClient() *openai.Client {
	config := openai.DefaultConfig(h.cfg.Model.APIKey)
	if strings.TrimSpace(h.cfg.Model.BaseURL) != "" {
		config.BaseURL = h.cfg.Model.BaseURL
	}
	return openai.NewClientWithConfig(config)
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
	if c.Model.TimeoutSeconds <= 0 {
		c.Model.TimeoutSeconds = 30
	}
	if c.Model.MaxTokens <= 0 {
		// Generous on purpose: the chain of thought is paid for out of the same budget
		// as the answer, so a cap that looks roomy for one small JSON object can still
		// cut the model off mid-sentence. Measured chains run to a few hundred tokens,
		// and the answer after them is tiny -- 1024 left no room to spare.
		c.Model.MaxTokens = 4096
	}
	if retries := c.judgeRetries(); retries < 0 || retries > 3 {
		return fmt.Errorf("model retries %d: want 0 to 3", retries)
	}

	switch c.Model.Thinking {
	case "hide", "show", "off":
	case "":
		// Nothing is sent, so the provider's own default applies. Left alone
		// deliberately: it is a real choice for somebody running against a model
		// whose default they know.
	default:
		return fmt.Errorf("model thinking %q: want hide, show, off or empty",
			c.Model.Thinking)
	}
	switch c.Model.ReasoningEffort {
	case "", "low", "medium", "high", "max":
	default:
		return fmt.Errorf("model reasoning_effort %q: want low, medium, high or max",
			c.Model.ReasoningEffort)
	}
	if c.Model.Thinking == "" && c.Model.ReasoningEffort != "" {
		return errors.New("model reasoning_effort is set but thinking is empty, so " +
			"the effort would be sent without the switch that turns reasoning on")
	}
	if c.Model.Stream && c.Model.Thinking != "" {
		// Refused rather than silently ignored. The streaming request goes through
		// the OpenAI-compatible client, which cannot carry the thinking switch, so
		// the two settings together would promise reasoning and quietly not ask for
		// it -- and a configuration that lies about that is worse than one that
		// refuses.
		return errors.New("model stream is on together with thinking: streaming " +
			"cannot carry the thinking switch, so set stream: false, or thinking: \"\" " +
			"to send whatever the provider defaults to")
	}
	if c.MinConfidence <= 0 {
		// A threshold rather than a share of the decision: it decides whether an
		// answer is acted on at all, so a model that is unsure changes nothing.
		c.MinConfidence = 0.6
	}
	if c.MaxChars <= 0 {
		c.MaxChars = 12000
	}
	if c.DryRun == nil {
		dryRun := true
		c.DryRun = &dryRun
	}

	// The durations are parsed here rather than where they are used, so that a
	// typo is refused at startup instead of becoming a silent default on the day
	// somebody is actually silenced.
	//
	// Go's own syntax, unlike a command argument: this is a file a person edits
	// deliberately, and 10m is shorter to read than 10分.
	if strings.TrimSpace(c.DefaultMute) != "" {
		if _, err := time.ParseDuration(c.DefaultMute); err != nil {
			return fmt.Errorf("default_mute %q: %w", c.DefaultMute, err)
		}
	}
	if c.ReportPenalty != nil && c.ReportPenalty.Enabled {
		// Enabled with nothing to serve would silence the reporter for no length
		// of time, which reads as a bug rather than as a policy.
		if strings.TrimSpace(c.ReportPenalty.Mute) == "" {
			return errors.New("report_penalty is enabled but names no mute duration")
		}
		if _, err := time.ParseDuration(c.ReportPenalty.Mute); err != nil {
			return fmt.Errorf("report_penalty mute %q: %w", c.ReportPenalty.Mute, err)
		}
	}
	for name, category := range c.Categories {
		if strings.TrimSpace(name) == "" {
			return errors.New("categories has an entry with an empty name")
		}
		if strings.TrimSpace(category.Label) == "" {
			return fmt.Errorf("category %q has no label, so the group could not be "+
				"told what was found", name)
		}
		if strings.TrimSpace(category.Mute) == "" {
			continue
		}
		if _, err := time.ParseDuration(category.Mute); err != nil {
			return fmt.Errorf("category %q mute %q: %w", name, category.Mute, err)
		}
	}
	for openID, group := range c.Groups {
		if strings.TrimSpace(openID) == "" {
			return errors.New("groups has an entry with an empty group openid")
		}
		for _, duration := range group.Categories {
			// Checked here for the same reason as the others: a duration that
			// cannot be read must not be discovered on the day somebody is
			// silenced for it.
			if strings.TrimSpace(duration) == "" {
				continue
			}
			if _, err := time.ParseDuration(duration); err != nil {
				return fmt.Errorf("group %s: %w", openID, err)
			}
		}
		for _, entry := range group.Allow {
			if strings.TrimSpace(entry) == "" {
				return fmt.Errorf("group %s has an empty allow entry, which would "+
					"match every message", openID)
			}
		}
		for _, entry := range group.AllowSenders {
			if strings.TrimSpace(entry) == "" {
				return fmt.Errorf("group %s has an empty allow_senders entry, which "+
					"is an openid that belongs to nobody", openID)
			}
		}
	}
	return nil
}

// JudgingEnabled reports whether a model is configured to judge with.
//
// A deployment that only fills the cache is a stage worth being able to run in,
// so a missing model is not a configuration error; it just means every report
// ends in "could not judge" rather than in a verdict.
func (c *Config) JudgingEnabled() bool {
	return strings.TrimSpace(c.Model.Name) != ""
}

// MuteFor is how long a member is silenced for one category, and whether the
// category is one this configuration knows.
//
// A category with no duration of its own falls back to the default, which is
// what "set no per-type duration, use a fixed one" means in practice.
func (c *Config) MuteFor(category string) (int64, bool) {
	entry, known := c.Categories[category]
	if !known {
		return 0, false
	}
	text := strings.TrimSpace(entry.Mute)
	if text == "" {
		text = strings.TrimSpace(c.DefaultMute)
	}
	if text == "" {
		// Nothing was configured, so nothing is served: the caller reports the
		// finding without silencing anybody.
		return 0, true
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		// Refused at startup, so this cannot happen in a running bot.
		return 0, true
	}
	return int64(parsed.Seconds()), true
}

// LabelFor is what a group is told about a category, from the configuration.
func (c *Config) LabelFor(category string) string {
	entry, known := c.Categories[category]
	if !known {
		return category
	}
	if strings.TrimSpace(entry.Label) == "" {
		return category
	}
	return entry.Label
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

// DryRun implements feature.Moderation.
//
// On unless it is turned off, and asked for from the outside: what it holds back
// belongs to whoever acts on a verdict.
func (h *handler) DryRun() bool { return h.cfg.DryRun == nil || *h.cfg.DryRun }

// ReportPenaltySeconds implements feature.Moderation.
//
// Zero means the configuration does not ask for a penalty, and it also means a
// penalty of no length: to a caller those are the same thing, because neither
// silences anybody.
func (h *handler) ReportPenaltySeconds() int64 {
	if h.cfg.ReportPenalty == nil || !h.cfg.ReportPenalty.Enabled {
		return 0
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(h.cfg.ReportPenalty.Mute))
	if err != nil {
		// Refused at startup, so this cannot happen in a running bot.
		return 0
	}
	return int64(parsed.Seconds())
}

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

	// Said out loud because these are what decide whether a judgement can hurt
	// somebody. An operator reading the log should be able to see whether the bot
	// is armed without opening the configuration file -- and "is it still in
	// dry_run" is the first question anybody asks when something goes wrong.
	h.deps.Logger.Info("the judge is configured",
		"dry_run", h.DryRun(),
		"model", h.cfg.Model.Name,
		"categories", len(h.cfg.Categories),
		"default_mute", h.cfg.DefaultMute,
		"report_penalty_seconds", h.ReportPenaltySeconds(),
		"thinking", h.cfg.Model.Thinking,
		"max_tokens", h.cfg.Model.MaxTokens,
		"retries", h.cfg.judgeRetries(),
		"context", fmt.Sprintf("%d before, %d after, %d minutes",
			h.cfg.ContextBefore, h.cfg.ContextAfter, h.cfg.ChainMinutes))

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
