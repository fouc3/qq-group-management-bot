// Package admincmd answers administrator commands sent in a group.
//
// The commands are restricted to a list configured per group, so being a group
// administrator in QQ grants nothing here: only the people named in the
// configuration can drive the bot. A command that names no target is refused
// with an explanation, because the platform gives the bot no way to address a
// member by their QQ number.
//
// What the bot answers, who may run it, where it is shown and what runs it are
// one table, in commandlist.go. Nothing else in this package names a command.
//
// Durations accept a number with a unit: 30s 30秒, 10m 10分 10分钟, 2h 2小时,
// 1d 1天.
package admincmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// Name is the feature's name: its configuration key and its log field.
const Name = "admin_commands"

// DefaultPrefix is the command prefix when the section leaves it out.
const DefaultPrefix = "/"

// DefaultMaxMute is the longest mute a command may ask for: the platform's
// ceiling is thirty days, so this stays just inside it.
const DefaultMaxMute = 29 * 24 * time.Hour

// blacklistListLimit caps how much of the list one command prints. A list long
// enough to be worth searching is a list long enough not to be pasted whole into
// a group.
const blacklistListLimit = 50

// Config is this feature's section in the configuration file.
type Config struct {
	// Enabled turns the switch on. A section written without it is on.
	Enabled bool `yaml:"enabled"`
	// Debug enables the /debug subcommands. Every other debug command is
	// refused while it is off.
	Debug bool `yaml:"debug"`
	// Prefix is the character a command must start with.
	Prefix string `yaml:"prefix"`
	// RegisterCommands publishes the command list as the group instruction
	// panel, which is the menu the platform shows a member.
	//
	// On by default: without it the commands are discovered only by being told
	// about them. Turn it off for a deployment that maintains its own panel, or
	// whose token is not allowed to create one. /debug is never published.
	RegisterCommands *bool `yaml:"register_commands"`
	// WhoisAdminOnly restricts /whois to the administrators of a group.
	//
	// It is on by default. The rule is deliberately relaxed for a group that
	// has no administrators yet: /whois is how the identifiers the list is
	// written with are discovered, so a strict rule there would lock an
	// operator out of the very group they are setting up. Once a group has
	// administrators, only they may ask.
	WhoisAdminOnly *bool `yaml:"whois_admin_only"`
	// RequireMention decides whether a command must mention the bot.
	//
	// It is on by default, and it is not just politeness: the platform tells the
	// bot which messages mentioned it, which is the only reliable signal that a
	// message was meant for the bot. Turning it off makes the bot read every
	// group message, which needs the group's receive setting to be "all", and
	// makes an answer to an unmentioned command a proactive message that counts
	// against the active message quota.
	RequireMention *bool `yaml:"require_mention"`
	// Groups holds one administrator list per group openid. A group that is
	// not listed has no administrators, so nobody can command the bot there.
	Groups map[string]GroupConfig `yaml:"groups"`
}

// GroupConfig is one group's administrator list.
type GroupConfig struct {
	// Admins are the member openids allowed to command the bot in this group.
	Admins []string `yaml:"admins"`
	// MaxMute caps what /禁言 may ask for in this group, as a duration such
	// as 29d. Empty means DefaultMaxMute.
	MaxMute string `yaml:"max_mute"`
}

// longestMute returns the cap for one group.
func (g GroupConfig) longestMute() (time.Duration, error) {
	if strings.TrimSpace(g.MaxMute) == "" {
		return DefaultMaxMute, nil
	}
	limit, err := parseDuration(g.MaxMute)
	if err != nil {
		return 0, fmt.Errorf("max_mute %q: %w", g.MaxMute, err)
	}
	return limit, nil
}

// applyDefaults fills in what the section leaves out, then validates it.
func (c *Config) applyDefaults() error {
	if strings.TrimSpace(c.Prefix) == "" {
		c.Prefix = DefaultPrefix
	}
	if c.RequireMention == nil {
		required := true
		c.RequireMention = &required
	}
	if c.WhoisAdminOnly == nil {
		restricted := true
		c.WhoisAdminOnly = &restricted
	}
	if c.RegisterCommands == nil {
		published := true
		c.RegisterCommands = &published
	}
	for openID, group := range c.Groups {
		if strings.TrimSpace(openID) == "" {
			return errors.New("groups has an entry with an empty group openid")
		}
		if len(group.Admins) == 0 {
			return fmt.Errorf("group %s lists no admins, so nobody can command the bot there", openID)
		}
		if _, err := group.longestMute(); err != nil {
			return fmt.Errorf("group %s: %w", openID, err)
		}
	}
	return nil
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
	h := &handler{cfg: cfg, deps: deps, router: command.NewRouter(Name, deps.Buttons)}
	h.part, h.stopPart = context.WithCancel(context.Background())
	// The table is validated here, so that a word invoking two commands stops the
	// bot at startup rather than leaving one of them answering nothing at all.
	if _, err := command.NewCatalog(h.commandDefs()); err != nil {
		return nil, err
	}
	return h, nil
}

// commands is the table this feature answers.
//
// Built on first use rather than in New, because the sources that contribute to it
// are wired after the feature exists -- and rebuilt rather than kept when they
// change, since a source that was built again hands over new runners.
func (h *handler) commands() *command.Catalog {
	h.tableMu.Lock()
	defer h.tableMu.Unlock()
	if h.table == nil {
		table, err := command.NewCatalog(append(h.commandDefs(), h.tableFrom...))
		if err != nil {
			// New refuses this before the table is ever reached this way, so
			// there is nobody left to report the mistake to.
			panic("admincmd: " + err.Error())
		}
		h.table = table
	}
	return h.table
}

// SetCommandSources takes the commands other features offer for this table.
//
// It is called every time the features are wired, which is after any of them has
// been built again, and what it is handed is the whole of what they offer rather
// than an addition to what they offered before: a source built again offers new
// runners, and a table that merged instead of replacing would keep answering from
// an instance nobody is running.
func (h *handler) SetCommandSources(defs []command.Def) error {
	table, err := command.NewCatalog(append(h.commandDefs(), defs...))
	if err != nil {
		// Refused rather than resolved: a word that invokes two commands leaves
		// one of them reachable by nothing, and which one is decided by the order
		// the features happen to be registered in.
		return err
	}
	h.tableMu.Lock()
	changed := !sameCommands(h.tableFrom, defs)
	h.tableFrom = append([]command.Def(nil), defs...)
	h.table = table
	running := h.registered
	h.tableMu.Unlock()

	// A table that changed while the bot runs has to reach the menu too: the panel
	// is published from this table, and a command that a feature started or stopped
	// offering would otherwise be answered and not offered -- or offered and not
	// answered. Before registration there is nothing to bring up to date: Register
	// publishes this table once it is whole.
	if changed && running {
		go h.republishCommands()
	}
	return nil
}

// republishCommands brings the menu up to date after the table changed.
//
// Under the feature's own context, so that a menu being brought up to date when the
// feature is built again does not outlive it.
func (h *handler) republishCommands() {
	ctx, cancel := context.WithTimeout(h.part, 20*time.Second)
	defer cancel()
	h.publishCommands(ctx)
}

// sameCommands reports whether two sets of offered commands would put the same
// entries in the menu.
//
// It compares what a menu shows -- the word and the explanation beside it -- and
// not the definitions themselves: a definition carries the runner of whichever
// instance offered it, and those differ every time a feature is built again. A
// menu that reads the same is left alone.
func sameCommands(before, after []command.Def) bool {
	if len(before) != len(after) {
		return false
	}
	for index := range before {
		if before[index].Name != after[index].Name || before[index].Desc != after[index].Desc {
			return false
		}
	}
	return true
}

// handler implements feature.Feature.
type handler struct {
	cfg  Config
	deps feature.Deps
	// verifier drives the join verification feature. The app injects it after
	// building, so the two features never import each other.
	verifier feature.Verifier

	// botOpenID is the bot's own member openid, which is what a mention of the
	// bot in a message looks like. Empty when it could not be read.
	botOpenID string

	// router is how the handlers below reach the events a command arrives in,
	// and it holds the record of the messages already acted on.
	router *command.Router

	// part is cancelled when this feature stops, and the work that outlives the
	// event that asked for it runs under it: a judgement of several seconds, an
	// answer to a button that must not be cut short. Without it, a feature built
	// again while the bot runs would leave the old instance still talking -- a
	// receipt posted three minutes after the instance it came from was replaced.
	part     context.Context
	stopPart context.CancelFunc

	// tableMu guards the table and what other features contributed to it, which
	// is rebuilt whenever they are wired.
	tableMu sync.Mutex
	// registered says that this feature has taken up its work, and therefore that
	// the menu is published from the table: a table that changes after this point
	// has to be published again.
	registered bool
	// table is the command table, built on first use.
	table *command.Catalog
	// tableFrom is what the other features offer for it, replaced rather than
	// extended on every wiring.
	tableFrom []command.Def

	// mu guards reports, which is where one member's rate limit is counted.
	mu sync.Mutex

	// receiptsMu guards receipts, which is what a receipt's buttons point at:
	// the detailed message a recall button will take back. It is held separately
	// from mu because the two are never wanted at once, and one lock over both
	// would make a button press wait behind a message dispatch.
	receiptsMu sync.Mutex
	receipts   map[string]receiptRecall

	// blacklist is the list of applicants barred from joining, or nil when there
	// is no data layer behind the command.
	blacklist Blacklist

	// moderation judges reported content, or nil when no such feature is
	// configured. The command says so rather than pretending to judge.
	moderation feature.Moderation

	// reports counts how often each member has reported, so that one person
	// cannot flood the model. Guarded by mu.
	reports map[string][]time.Time
}

// Blacklist is the part of the data layer the /黑名单 command uses.
//
// A narrow interface rather than the whole store: this command adds, removes and
// lists entries and has no business reading a pending hold. The store's own
// blacklist satisfies it without an adapter, because the shapes already match.
type Blacklist interface {
	// Add records an entry, replacing one with the same key.
	Add(ctx context.Context, entry store.Barred) error
	// Remove forgets an entry by ID, member openid or union openid.
	Remove(ctx context.Context, key string) error
	// List returns entries, newest first.
	List(ctx context.Context, limit int) ([]store.Barred, error)
}

// SetBlacklist hands over the list the command manages.
//
// The app calls it before anything is registered, so the command is never
// reachable while it still has no list to work on.
func (h *handler) SetBlacklist(blacklist Blacklist) {
	h.blacklist = blacklist
}

// SetModeration hands over what judges reported content.
//
// It may be nil, and it is called whatever the answer is: a feature that reports
// content has to be able to say "there is nothing behind this command" instead of
// being absent, which is indistinguishable from being broken.
func (h *handler) SetModeration(moderation feature.Moderation) {
	h.moderation = moderation
}

// newBlacklistID returns the key an entry is stored under.
//
// Random rather than derived from the applicant: an entry may name only a union
// openid, so there is no member openid to build a key from, and a key that could
// be guessed would let one entry silently overwrite another.
func newBlacklistID() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generating a blacklist id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// Name implements feature.Feature.
func (h *handler) Name() string { return Name }

// Intents implements feature.Feature.
func (h *handler) Intents() qqbotsdk.Intent {
	// Commands arrive as ordinary group messages, so both the mention event
	// and the full receive event can carry one. Interactions are the buttons
	// under a receipt: a press is delivered on the same connection, and without
	// the intent the button answers nothing at all.
	return qqbotsdk.IntentGroupAndC2CEvent | qqbotsdk.IntentInteraction
}

// SetVerifier implements feature.VerifierAware.
func (h *handler) SetVerifier(verifier feature.Verifier) { h.verifier = verifier }

// IsAdmin implements feature.AdminDirectory.
// LooksLikeACommand implements feature.Commands.
//
// It answers for the table as a whole, which is what a feature waiting for free
// text needs to know: a message carrying the prefix is somebody asking the bot to
// do something, whether or not the table holds the word they typed.
func (h *handler) LooksLikeACommand(content string) bool {
	_, ok := command.Parse(content, h.cfg.Prefix)
	return ok
}

func (h *handler) IsAdmin(groupOpenID, memberOpenID string) bool {
	group, known := h.cfg.Groups[groupOpenID]
	return known && contains(group.Admins, memberOpenID)
}

// Register implements feature.Feature.
func (h *handler) Register(_ context.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	h.botOpenID = h.readBotOpenID(ctx)
	cancel()

	// The command list is published here, before any event can arrive, so the
	// panel a member opens is never a version behind what the bot answers.
	h.tableMu.Lock()
	h.registered = true
	h.tableMu.Unlock()
	panelCtx, panelCancel := context.WithTimeout(context.Background(), 20*time.Second)
	h.publishCommands(panelCtx)
	panelCancel()
	// Which events a command can arrive in is the command layer's knowledge, not
	// this feature's. The router reads both group event types, because which one
	// carries a message depends on the group's receive setting rather than on the
	// message, and it reads a single chat and a button press as well.
	//
	// A single chat carries one command and nothing else: reading a receipt, which
	// is the only way an administrator can see the model's own words about a
	// member without the group reading them too. It is registered whatever the
	// configuration says about receipts, because the handler decides what to
	// answer rather than the registration.
	//
	// A message that arrives twice, as both group types, is handled once: the
	// record the router hands over drops the second delivery, and each path below
	// asks it at the point where dropping is safe.
	//
	// The two keyboards are claimed by name rather than looked for in every
	// press: the button's own data is the only thing that says whose it is, and
	// a press now reaches this feature only when it carries one of these
	// namespaces. Both are answered wherever a press arrives, which is what this
	// feature did before the claim was written down -- a claim says where a press
	// is answered, not where the button is put, and the detail button really is
	// under a summary in a single chat as well as in a group.
	if err := h.router.Register(h.deps.Client, command.Handlers{
		Group:   h.onMessage,
		Private: h.onPrivateMessage,
		Buttons: []command.ButtonClaim{
			{Namespace: receiptDetailPrefix, Scenes: command.InGroup | command.InPrivate,
				Handle: h.showDetailsPress},
			{Namespace: receiptRecallPrefix, Scenes: command.InGroup | command.InPrivate,
				Handle: h.recallDetailsPress},
		},
	}); err != nil {
		return err
	}
	h.deps.Logger.Info("administrator commands are ready",
		"prefix", h.cfg.Prefix, "debug", h.cfg.Debug,
		"require_mention", h.mentionsRequired(),
		"whois_admin_only", h.whoisAdminOnly(),
		"groups", len(h.cfg.Groups))
	return nil
}

// Close implements feature.Feature.
//
// It stops answering and then stops the work that was already running, in that
// order: nothing new can arrive, and what was in flight ends here rather than
// outliving the feature. Both are what lets the app build this feature again with
// new configuration while the bot runs.
func (h *handler) Close(context.Context) error {
	h.router.Stop()
	h.stopPart()
	return nil
}

// onMessage handles one group message that may carry a command.
func (h *handler) onMessage(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.GroupMessageCreateData)
	if !ok {
		return fmt.Errorf("administrator commands got %T for a group message", value)
	}

	// Logged before any filtering. Every filter below is silent by design, so a
	// message that disappears in one of them would otherwise leave no trace at
	// all -- which is how "I sent it and nothing happened" becomes impossible to
	// diagnose. known_group is the one that matters most: a group missing from
	// bot.groups is dropped here, quietly, and never answers.
	h.deps.Logger.Debug("a group message arrived",
		"group", data.GroupOpenID,
		"known_group", h.deps.InGroup(data.GroupOpenID),
		"event", event.Type,
		"message", data.ID,
		"sender", senderOpenID(data),
		"content", data.Content,
		"mentions", len(data.Mentions))

	if data.Author == nil {
		return nil
	}
	if !h.deps.InGroup(data.GroupOpenID) {
		return h.unconfiguredGroup(ctx, event, data)
	}

	if !h.router.Seen.First(data.ID) {
		// The same message arrived as a second event type.
		return nil
	}

	cmd, ok := command.Parse(data.Content, h.cfg.Prefix)
	if !ok {
		return nil
	}
	// With require_mention on, a command counts only when the message reached
	// the bot because it was mentioned.
	if h.mentionsRequired() && !h.botWasMentioned(event, cmd) {
		return nil
	}
	sender := data.Author.MemberOpenID
	if sender == "" {
		return nil
	}

	def, listed := h.commands().Lookup(cmd.Name)
	if !listed {
		// A word the table does not hold still goes through the audience check
		// first: an administrator who mistyped is answered with the list of what
		// the bot does hold, and anybody else is refused like any other management
		// command. Run stays nil, which is what comes back.
		def = command.Def{Audience: command.Admins}
	}
	if !h.allows(def, h.viewerOf(data.GroupOpenID, sender)) {
		h.deny(ctx, data, cmd.Name, sender)
		return nil
	}
	h.deps.Logger.Info("an administrator command arrived",
		"group", data.GroupOpenID, "member", sender, "command", cmd.Name)
	if def.Run == nil {
		h.reply(ctx, data, h.usageText())
		return nil
	}
	return def.Run(ctx, data, cmd)
}

// unconfiguredGroup answers a group that the bot's own group list does not name.
//
// Such a group still has to answer the one command that configures it: running
// it there is how a group gets configured at all, since its openid is the first
// thing the file needs and until then nothing else can be set up. Every other
// command is dropped, because there would be no administrator list to check the
// sender against. The sender is never an administrator here, so they see only
// their own openid.
//
// The record of handled messages is asked after the mention test rather than
// before it, and the order is load-bearing: a group set to receive everything can
// report one message as both a mention event and an ordinary one, and only the
// delivery that mentions the bot is the one to answer. Recording the first
// sighting would drop the second, and the command would answer nothing at all.
func (h *handler) unconfiguredGroup(ctx context.Context, event *qqbotsdk.Event,
	data *qqbotsdk.GroupMessageCreateData) error {
	cmd, ok := command.Parse(data.Content, h.cfg.Prefix)
	if !ok {
		return nil
	}
	def, listed := h.commands().Lookup(cmd.Name)
	if !listed || !def.InUnconfiguredGroup {
		return nil
	}
	if h.mentionsRequired() && !h.botWasMentioned(event, cmd) {
		return nil
	}
	if !h.router.Seen.First(data.ID) {
		return nil
	}
	h.deps.Logger.Info("answering /whois in a group that is not configured yet",
		"group", data.GroupOpenID, "member", data.Author.MemberOpenID)
	return def.Run(ctx, data, cmd)
}

// viewer is who is asking, about one group.
type viewer struct {
	// isAdmin reports whether the sender is on the group's administrator list.
	isAdmin bool
	// settingUp reports whether the group names no administrator yet, which is
	// the state the one command that writes that list stays open for.
	settingUp bool
}

// viewerOf reports who a member is to a group.
func (h *handler) viewerOf(groupOpenID, memberOpenID string) viewer {
	group, known := h.cfg.Groups[groupOpenID]
	return viewer{
		isAdmin:   known && contains(group.Admins, memberOpenID),
		settingUp: !known || len(group.Admins) == 0,
	}
}

// allows reports whether a command may be run by this viewer.
//
// This is the whole of the audience rule, read off the table instead of written
// again for every command: what the table says a command is restricted to is what
// the bot enforces, and the two cannot come apart.
//
// There is deliberately no "is a group administrator" branch in the Whois case.
// The platform refuses to let this application read a member's role or the
// member list at all (40012010 应用无接口访问权限, measured), so such a branch
// could never answer yes and would only look like a check that exists. OneBot can
// read the roles, but it reports QQ numbers while the bot is only ever given an
// openid, and nothing converts one into the other.
func (h *handler) allows(def command.Def, who viewer) bool {
	switch def.Audience {
	case command.Everyone:
		return true
	case command.Whois:
		return who.isAdmin || who.settingUp || !h.whoisAdminOnly()
	default:
		return who.isAdmin
	}
}

// deny refuses a command to a member who may not run it.
//
// reported is the word as it was typed, which is what the log line carries: a
// command that was refused may not be one the table holds, so there is not
// always a name of ours to log.
func (h *handler) deny(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	reported, sender string) {
	if reported == "whois" {
		h.deps.Logger.Warn("refused /whois for a member who is not an administrator",
			"group", data.GroupOpenID, "member", sender)
	} else {
		h.deps.Logger.Warn("refused a command from a member who is not on the administrator list",
			"group", data.GroupOpenID, "member", sender, "command", reported)
	}
	h.reply(ctx, data, "你没有权限使用管理命令。")
}

// whoisAdminOnly reports whether /whois is restricted to administrators.
func (h *handler) whoisAdminOnly() bool {
	return h.cfg.WhoisAdminOnly == nil || *h.cfg.WhoisAdminOnly
}

// botWasMentioned reports whether a command was addressed to the bot.
//
// The two event types need different tests. A mention event means the platform
// delivered the message because the bot was mentioned, whatever the text looks
// like -- and in a group that receives mention-only messages the platform
// removes the mention from the content, so the text carries no trace of it at
// all. An ordinary message has to name the bot.
//
// Naming the bot means comparing the mention against the bot's own openid. An
// earlier version only checked that some mention led the text, and production
// showed what that costs: "@AIRY /菜单" was answered, because AIRY's mention led
// the message just as the bot's would. A command that runs whenever anybody
// mentions anybody is not a command that requires a mention.
func (h *handler) botWasMentioned(event *qqbotsdk.Event, cmd command.Parsed) bool {
	if event.Type == qqbotsdk.EventGroupAtMessageCreate {
		return true
	}
	if h.botOpenID == "" {
		// The bot's own openid could not be read, so the mention cannot be
		// checked against it. Refusing every command would be worse than
		// accepting a leading mention, so that is what is left; the warning is
		// logged once, when the openid is looked for.
		return cmd.BotOpenID != ""
	}
	return cmd.BotOpenID == h.botOpenID
}

// readBotOpenID asks the platform who the bot is, using one of the configured
// groups.
//
// The same openid comes back for every group, because an openid identifies a
// user to this application rather than to a group, and it is the only way to
// tell a mention of the bot from a mention of somebody else.
func (h *handler) readBotOpenID(ctx context.Context) string {
	for groupOpenID := range h.cfg.Groups {
		state, err := h.deps.Client.GetGroupBotState(ctx, groupOpenID)
		if err != nil {
			h.deps.Logger.Warn("could not read the bot's own openid from a group",
				"group", groupOpenID, "error", err)
			continue
		}
		if state != nil && state.MemberOpenID != "" {
			return state.MemberOpenID
		}
	}
	h.deps.Logger.Warn("the bot's own openid is unknown, so in a group that " +
		"receives every message a leading mention is accepted without checking " +
		"that it names the bot")
	return ""
}

// mentionsRequired reports whether a command has to mention the bot.
func (h *handler) mentionsRequired() bool {
	return h.cfg.RequireMention == nil || *h.cfg.RequireMention
}

// whois reports the group, the bot's own standing in it, and the identifiers
// of this message.
//
// It is the one command answered in any group and for anybody, because it is
// how the identifiers the administrator list is written with are discovered. It
// reports openids, which are opaque and scoped to this application, and it
// reads only endpoints that need no extra permission.
func (h *handler) whois(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	isAdmin bool) {
	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	lines := []string{"**本群**", "群 openid：`" + data.GroupOpenID + "`"}
	if info, err := h.deps.Client.GetGroupInfo(queryCtx, data.GroupOpenID); err != nil {
		lines = append(lines, "群信息读取失败："+err.Error())
	} else {
		if info.GroupName != "" {
			lines = append(lines, "群名称："+info.GroupName)
		}
		if info.GroupMemberNum > 0 {
			lines = append(lines, fmt.Sprintf("成员数：%d", info.GroupMemberNum))
		}
		if info.GroupClassText != "" {
			lines = append(lines, "分类："+info.GroupClassText)
		}
		if len(info.GroupTags) > 0 {
			lines = append(lines, "标签："+strings.Join(info.GroupTags, "、"))
		}
	}

	lines = append(lines, "", "**机器人自身**")
	if h.deps.BotQQ != 0 {
		lines = append(lines, fmt.Sprintf("QQ 号：%d", h.deps.BotQQ))
	}
	if state, err := h.deps.Client.GetGroupBotState(queryCtx, data.GroupOpenID); err != nil {
		lines = append(lines, "机器人群内状态读取失败："+err.Error())
	} else {
		lines = append(lines, "member_openid：`"+state.MemberOpenID+"`")
		if state.MemberRole != "" {
			lines = append(lines, "群内角色："+state.MemberRole)
		}
		if state.RecvMsgSetting != "" {
			lines = append(lines, "接收消息设置："+state.RecvMsgSetting)
		}
		lines = append(lines, fmt.Sprintf("允许主动推送：%v", state.AllowProactiveMsg))
		if state.JoinedAt != "" {
			lines = append(lines, "入群时间："+state.JoinedAt)
		}
	}

	lines = append(lines, "", "**本次消息**",
		"发送者 member_openid：`"+data.Author.MemberOpenID+"`")
	// Naming other members is for administrators. A member who runs the command
	// while the group is still being set up gets their own id -- which is what
	// they need in order to be put on the list -- and nothing about anybody else.
	if isAdmin {
		for _, mention := range data.Mentions {
			if mention.MemberOpenID == "" {
				continue
			}
			// The bot itself is in the mention list whenever the command reached
			// it by mentioning it, which is nearly always. Listing it as somebody
			// who was asked about is noise, and the sender's own id already has a
			// line of its own.
			if mention.MemberOpenID == h.botOpenID {
				continue
			}
			line := "被 @ 的 member_openid：`" + mention.MemberOpenID + "`"
			if mention.Username != "" {
				line += "（" + mention.Username + "）"
			}
			lines = append(lines, line)
		}
	} else {
		lines = append(lines, "（只有管理员能用 @ 查看他人的 member_openid）")
	}
	lines = append(lines, "",
		"填入 features.admin_commands.groups：键是群 openid，admins 里写 member_openid。")

	h.reply(ctx, data, strings.Join(lines, "\n"))
}

// The runners below are what the table points at, one per command. They are
// separate functions rather than branches of one switch because the table names
// them: a command's definition and what the command does are written together,
// and neither can be added without the other.
//
// The target is resolved inside the runner that needs one. Resolving it for
// every command made every command answer "请 @ 目标成员" before its own logic
// could run, which is what turned /菜单 into a complaint about a missing target
// instead of the command list.

// menuCommand answers with the list of what the bot does.
func (h *handler) menuCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	_ command.Parsed) error {
	h.reply(ctx, data, h.usageText())
	return nil
}

// whoisCommand reports the group, the bot's own standing in it, and the
// identifiers of this message.
//
// It asks whether the sender administers the group rather than being told, and it
// asks about a group the bot manages: /whois is the one command answered in a
// group that is not configured at all, and nobody administers a group the bot
// does not manage. Without that half, an administrator list written for a group
// the bot is not in would be enough to have this command name other members
// there.
func (h *handler) whoisCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	_ command.Parsed) error {
	isAdmin := h.deps.InGroup(data.GroupOpenID) &&
		h.IsAdmin(data.GroupOpenID, senderOpenID(data))
	h.whois(ctx, data, isAdmin)
	return nil
}

// privateWhois answers /whois in a single chat.
//
// There is no group to describe there, so it reports the one identifier that is
// always about the asker: their own openid. It was added on the belief that a
// single chat carries a different value from the one a group event carries, and
// that belief is wrong -- measured on this application, the same person reads the
// same value in both places, and the reply used to say otherwise.
//
// What it is still for is reading that value without a group. Somebody who has to
// be put on a list before they say anything in a group has no other way to find
// out what to write there.
//
// It answers for anybody, and says only what the asker already owns: their own openid.
// Refusing it to somebody the bot cannot place would make the one value they need
// unreadable, and it reveals nothing about anybody else.
func (h *handler) privateWhois(ctx context.Context, data *qqbotsdk.C2CMessageCreateData,
	_ command.Parsed) error {
	lines := []string{
		"**私聊**",
		"你的 openid：`" + data.Author.UserOpenID + "`",
		"",
		"这就是你在群里被认出的那个 member_openid —— 两个场景取到的是同一个值。" +
			"群的管理员名单、以及私聊用的名单（比如广播内测名单）里填的都是它。",
	}
	h.replyPrivately(ctx, data, strings.Join(lines, "\n"))
	return nil
}

// muteCommand applies a command mute.
func (h *handler) muteCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	cmd command.Parsed) error {
	target, err := targetOf(data, cmd)
	if err != nil {
		h.reply(ctx, data, err.Error())
		return nil
	}
	// The group is known here, because only its administrators reach this and
	// that list is where the check read them from.
	limit, err := h.cfg.Groups[data.GroupOpenID].longestMute()
	if err != nil {
		return err
	}
	return h.mute(ctx, data, cmd, target, limit)
}

// unmuteCommand lifts a member's mute.
func (h *handler) unmuteCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	cmd command.Parsed) error {
	target, err := targetOf(data, cmd)
	if err != nil {
		h.reply(ctx, data, err.Error())
		return nil
	}
	return h.unmute(ctx, data, target)
}

// reverifyCommand holds a member again and sends a fresh prompt.
func (h *handler) reverifyCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	cmd command.Parsed) error {
	target, err := targetOf(data, cmd)
	if err != nil {
		h.reply(ctx, data, err.Error())
		return nil
	}
	if h.verifier == nil {
		h.reply(ctx, data, "本机器人没有启用入群验证功能。")
		return nil
	}
	if err := h.verifier.Reverify(ctx, data.GroupOpenID, target); err != nil {
		h.deps.Logger.Error("re-verification failed", "error", err)
		h.reply(ctx, data, "重新验证失败："+err.Error())
		return nil
	}
	h.reply(ctx, data, "已重新发出验证。")
	return nil
}

// resendCommand posts the verification prompt again for a member already waiting.
func (h *handler) resendCommand(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	cmd command.Parsed) error {
	target, err := targetOf(data, cmd)
	if err != nil {
		h.reply(ctx, data, err.Error())
		return nil
	}
	return h.resendVerification(ctx, data, target)
}

// debug handles the /debug subcommands.
func (h *handler) debug(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	cmd command.Parsed) error {
	if !h.cfg.Debug {
		h.reply(ctx, data, "调试功能未开启。")
		return nil
	}
	if len(cmd.Args) == 0 {
		h.reply(ctx, data, "用法："+h.cfg.Prefix+"debug 超时测试 [@目标]")
		return nil
	}
	sub := cmd.Args[0]
	switch sub {
	case "超时测试", "timeout":
		if h.verifier == nil {
			h.reply(ctx, data, "本机器人没有启用入群验证功能。")
			return nil
		}
		// Only this subcommand needs a target, so it is resolved here rather
		// than for the debug command as a whole.
		target, err := targetOf(data, cmd)
		if err != nil {
			h.reply(ctx, data, err.Error())
			return nil
		}
		if err := h.verifier.SimulateDeadline(ctx, data.GroupOpenID, target); err != nil {
			h.deps.Logger.Error("the timeout simulation failed", "error", err)
			h.reply(ctx, data, "超时测试失败："+err.Error())
			return nil
		}
		// Nothing is answered on success: the run posts its own notices in the
		// group, and an extra "done" line on top of them is noise.
		return nil
	default:
		h.reply(ctx, data, "未知的调试子命令："+sub)
		return nil
	}
}

// durationIn returns the duration among a command's arguments.
//
// It reports what it could not read, so an operator sees their own text back
// rather than a usage line that repeats what they already typed correctly.
func durationIn(args []string) (time.Duration, error) {
	if len(args) == 0 {
		return 0, errors.New("没找到时长。用法：/禁言 <时长> [@目标]，例如 /禁言 30m 或 /禁言 @某人 5d")
	}
	var lastErr error
	for _, arg := range args {
		if _, mentioned := command.FirstMention(arg); mentioned {
			continue
		}
		parsed, err := parseDuration(arg)
		if err != nil {
			lastErr = err
			continue
		}
		return parsed, nil
	}
	if lastErr != nil {
		return 0, lastErr
	}
	return 0, errors.New("没找到时长。用法：/禁言 <时长> [@目标]，例如 /禁言 30m 或 /禁言 @某人 5d")
}

// senderOpenID reports who sent a message, or nothing when the platform left
// the author out, so a diagnostic log can never panic on a nil author.
func senderOpenID(data *qqbotsdk.GroupMessageCreateData) string {
	if data.Author == nil {
		return ""
	}
	return data.Author.MemberOpenID
}

// atTag renders the mention markup for a member, for a reply that names them.
func atTag(memberOpenID string) string {
	return `<qqbot-at-user id="` + memberOpenID + `" />`
}

// unmute lifts a member's mute, unless they are waiting to verify.
//
// The verification wins over the command: a member who is being held has to
// press their own button. Letting a mute command release them as a side effect
// would turn the check into something that quietly stops applying, and the
// verification feature already offers an explicit way for an administrator to
// let somebody through -- pressing that member's button, which is recorded as a
// skip.
func (h *handler) unmute(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	target string) error {
	if h.verifier != nil && h.verifier.IsPending(data.GroupOpenID, target) {
		h.deps.Logger.Warn("refused to lift a mute for a member who is verifying",
			"group", data.GroupOpenID, "member", target)
		h.reply(ctx, data, "该成员正在验证中，不能用解禁放行。\n"+
			"请让他点验证按钮；管理员也可以直接点他的按钮跳过（会被记录）。")
		return nil
	}
	if err := h.deps.Client.SetGroupMemberMute(ctx, data.GroupOpenID,
		&qqbotsdk.SetGroupMemberMuteRequest{
			Members: []qqbotsdk.SetMemberMuteState{{
				Op:           qqbotsdk.MemberMuteDelete,
				MemberOpenID: target,
			}},
		}); err != nil {
		h.deps.Logger.Error("could not lift a mute", "error", err, "member", target)
		h.reply(ctx, data, "解除禁言失败："+err.Error())
		return nil
	}
	h.deps.Logger.Info("an administrator lifted a mute",
		"group", data.GroupOpenID, "member", target)
	h.reply(ctx, data, atTag(target)+" 的禁言已解除。")
	return nil
}

// resendVerification posts the verification prompt again for a member who is
// already waiting.
func (h *handler) resendVerification(ctx context.Context,
	data *qqbotsdk.GroupMessageCreateData, target string) error {
	if h.verifier == nil {
		h.reply(ctx, data, "本机器人没有启用入群验证功能。")
		return nil
	}
	if err := h.verifier.Resend(ctx, data.GroupOpenID, target); err != nil {
		h.reply(ctx, data, err.Error())
		return nil
	}
	h.reply(ctx, data, "已重新发送验证通知。")
	return nil
}

// blacklistTarget returns the identity a command is about, and its reason.
//
// The identity may be written as a mention or as a bare openid. Both are
// accepted because the two cases are genuinely different: somebody in the group
// can be mentioned, while an entry for somebody who never applied here has an
// openid from somewhere else and no way to be mentioned at all.
func blacklistTarget(cmd command.Parsed) (string, string, error) {
	if len(cmd.Args) < 2 {
		return "", "", errors.New("请 @ 目标成员，或直接给出 openid：" +
			cmd.Name + " <@目标|openid> [原因]")
	}
	key := cmd.Args[1]
	if mentioned, found := command.FirstMention(key); found {
		key = mentioned
	}
	if strings.TrimSpace(key) == "" {
		return "", "", errors.New("没有识别出 openid")
	}
	return key, strings.Join(cmd.Args[2:], " "), nil
}

// blacklistCommand manages the list of applicants barred from joining.
//
// It manages a list; it does not police a group. Nothing here removes anybody
// who is already in one, and nothing here undoes anything by itself: the list
// only decides how a join request is answered.
func (h *handler) blacklistCommand(ctx context.Context,
	data *qqbotsdk.GroupMessageCreateData, cmd command.Parsed) error {
	if h.blacklist == nil {
		h.reply(ctx, data, "本机器人没有可用的数据层，黑名单命令不可用。")
		return nil
	}
	prefix := h.cfg.Prefix
	if len(cmd.Args) == 0 {
		h.reply(ctx, data, "用法：\n"+prefix+"黑名单 add <@目标|openid> [原因]\n"+
			prefix+"黑名单 remove <@目标|openid>\n"+prefix+"黑名单 list")
		return nil
	}

	switch cmd.Args[0] {
	case "add", "添加", "加":
		key, reason, err := blacklistTarget(cmd)
		if err != nil {
			h.reply(ctx, data, err.Error())
			return nil
		}
		id, err := newBlacklistID()
		if err != nil {
			h.reply(ctx, data, "生成条目编号失败："+err.Error())
			return nil
		}
		if err := h.blacklist.Add(ctx, store.Barred{
			ID:           id,
			MemberOpenID: key,
			Reason:       reason,
			AddedAt:      time.Now().Unix(),
			AddedBy:      senderOpenID(data),
		}); err != nil {
			h.deps.Logger.Error("could not add a blacklist entry", "error", err)
			h.reply(ctx, data, "加入黑名单失败："+err.Error())
			return nil
		}
		h.deps.Logger.Info("an administrator added a blacklist entry",
			"group", data.GroupOpenID, "subject", key)
		answer := "已加入黑名单：`" + key + "`"
		if reason != "" {
			answer += "（" + reason + "）"
		}
		h.reply(ctx, data, answer)
		return nil

	case "remove", "移除", "删除", "del":
		key, _, err := blacklistTarget(cmd)
		if err != nil {
			h.reply(ctx, data, err.Error())
			return nil
		}
		// Checked first, because removing something that was not there and
		// reporting success would leave an administrator believing a list had
		// been changed when it had not.
		entries, err := h.blacklist.List(ctx, blacklistListLimit)
		if err != nil {
			h.reply(ctx, data, "读取黑名单失败："+err.Error())
			return nil
		}
		if !blacklistHas(entries, key) {
			h.reply(ctx, data, "黑名单里没有 `"+key+"`")
			return nil
		}
		if err := h.blacklist.Remove(ctx, key); err != nil {
			h.deps.Logger.Error("could not remove a blacklist entry", "error", err)
			h.reply(ctx, data, "移除失败："+err.Error())
			return nil
		}
		h.deps.Logger.Info("an administrator removed a blacklist entry",
			"group", data.GroupOpenID, "subject", key)
		h.reply(ctx, data, "已从黑名单移除：`"+key+"`")
		return nil

	case "list", "列表", "列出":
		entries, err := h.blacklist.List(ctx, blacklistListLimit)
		if err != nil {
			h.reply(ctx, data, "读取黑名单失败："+err.Error())
			return nil
		}
		if len(entries) == 0 {
			h.reply(ctx, data, "黑名单是空的。")
			return nil
		}
		now := time.Now().Unix()
		lines := []string{"**黑名单**（" + strconv.Itoa(len(entries)) + " 条）"}
		for _, entry := range entries {
			subject := entry.MemberOpenID
			if subject == "" {
				subject = "union:" + entry.UnionOpenID
			}
			line := "- `" + subject + "`"
			if entry.Reason != "" {
				line += " —— " + entry.Reason
			}
			if entry.ExpiresAt != 0 {
				if entry.ExpiresAt <= now {
					line += "（已过期）"
				} else {
					line += "（至 " + time.Unix(entry.ExpiresAt, 0).Format("2006-01-02") + "）"
				}
			}
			lines = append(lines, line)
		}
		h.reply(ctx, data, strings.Join(lines, "\n"))
		return nil

	default:
		h.reply(ctx, data, "未知的黑名单子命令："+cmd.Args[0]+
			"。可用：add / remove / list")
		return nil
	}
}

// blacklistHas reports whether the list names an identity.
func blacklistHas(entries []store.Barred, key string) bool {
	for _, entry := range entries {
		if entry.ID == key || entry.MemberOpenID == key || entry.UnionOpenID == key {
			return true
		}
	}
	return false
}

// mute applies a command mute to the target.
func (h *handler) mute(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	cmd command.Parsed, target string, limit time.Duration) error {
	// The duration may come before or after the target: both orders read
	// naturally, and the target itself comes from the mention rather than from
	// an argument, so its position carries no meaning. An argument that is a
	// mention is skipped instead of being read as a duration, which is what
	// made "/禁言 @某人 5d" fail with 无法识别的时长 "@某人".
	wanted, err := durationIn(cmd.Args)
	if err != nil {
		h.reply(ctx, data, err.Error())
		return nil
	}
	if wanted > limit {
		h.reply(ctx, data, fmt.Sprintf("时长超出上限：最长 %s。", humanDuration(limit)))
		return nil
	}

	until := time.Now().Add(wanted)
	if err := h.deps.Client.SetGroupMemberMute(ctx, data.GroupOpenID,
		&qqbotsdk.SetGroupMemberMuteRequest{
			Members: []qqbotsdk.SetMemberMuteState{{
				Op:           qqbotsdk.MemberMuteAdd,
				MemberOpenID: target,
				MuteExpireAt: until.Format(time.RFC3339),
			}},
		}); err != nil {
		h.deps.Logger.Error("the command mute failed", "error", err)
		h.reply(ctx, data, "禁言失败："+err.Error())
		return nil
	}
	h.deps.Logger.Info("an administrator muted a member",
		"group", data.GroupOpenID, "member", target, "until", until.Format(time.RFC3339))
	h.reply(ctx, data, fmt.Sprintf("已禁言 %s。", humanDuration(wanted)))
	return nil
}

// targetOf returns the member a command acts on.
//
// The target must be mentioned: the platform exposes a member to the bot only
// as a mention, and never lets a QQ number be used in its place.
//
// Three shapes are tried, because the platform reports a target differently
// depending on the event:
//
//  1. a mention left in the text as markup, which a full receive event keeps;
//  2. the mentions list, which the same event fills in and which includes the
//     bot itself despite the documentation saying otherwise;
//  3. the sender of a quoted message, which is how a mention-only group could
//     name a target at all.
//
// Production settled the third one's importance: a mention event in a
// mention-only group delivered "/重新验证 @某人" as content " /重新验证  " with an
// empty mentions list, so the mention was removed entirely and neither of the
// first two shapes could work.
func targetOf(data *qqbotsdk.GroupMessageCreateData, cmd command.Parsed) (string, error) {
	// The text comes first: the mention of the bot has already been removed
	// from it, so anything still mentioned is the target.
	if mentioned, found := command.FirstMention(cmd.Tail); found {
		return mentioned, nil
	}
	// Then the list, skipping the bot's own mention. A full receive event lists
	// every mention in the order they appear, so the bot is usually first and
	// taking the first entry would try to act on the bot itself, which the
	// platform refuses with 40103004.
	for _, mention := range data.Mentions {
		if mention.MemberOpenID == "" || mention.MemberOpenID == cmd.BotOpenID {
			continue
		}
		return mention.MemberOpenID, nil
	}
	for _, element := range data.MsgElements {
		if element.Author != nil && element.Author.MemberOpenID != "" {
			return element.Author.MemberOpenID, nil
		}
	}
	return "", fmt.Errorf("请 @ 目标成员，或**回复引用**目标的消息。平台限制：机器人无法用 QQ 号"+
		"指定成员；而在本群的“仅 @ 时接收”模式下，@ 目标的名字会被平台抹掉（命令 %s）",
		cmd.Name)
}

// replyWithKeyboard answers in the group with buttons under the answer.
//
// It is a passive reply like every other command answer, so the buttons arrive
// attached to the message the group asked about rather than as a second message
// out of nowhere.
func (h *handler) replyWithKeyboard(ctx context.Context,
	data *qqbotsdk.GroupMessageCreateData, text string, keyboard *qqbotsdk.Keyboard) {
	if _, err := h.sendMessageWithKeyboard(ctx, data.GroupOpenID, text, data.ID,
		keyboard); err != nil {
		h.deps.Logger.Warn("could not answer a command", "error", err)
	}
}

// reply answers in the group, as a passive reply to the command.
func (h *handler) reply(ctx context.Context, data *qqbotsdk.GroupMessageCreateData, text string) {
	// Through the one sender, so that every answer to a command is retried the
	// same way: an answer that never arrives is indistinguishable, from the
	// group's side, from a bot that ignored them.
	if err := h.sendMessage(ctx, data.GroupOpenID, text, data.ID); err != nil {
		h.deps.Logger.Warn("could not answer a command", "error", err)
	}
}

// replyPrivately answers in a single chat, as a passive reply to the command.
func (h *handler) replyPrivately(ctx context.Context, data *qqbotsdk.C2CMessageCreateData, text string) {
	if err := h.sendPrivateMessage(ctx, data.Author.UserOpenID, text, data.ID); err != nil {
		h.deps.Logger.Warn("could not answer a private command",
			"member", data.Author.UserOpenID, "error", err)
	}
}

// replyPrivatelyWithKeyboard is the same with buttons under the answer.
//
// A keyboard in a single chat is a real thing, not a hopeful one: it is what the
// SDK's own production test uses to get a button clicked, and the click comes back
// as an interaction with the c2c scene.
func (h *handler) replyPrivatelyWithKeyboard(ctx context.Context,
	data *qqbotsdk.C2CMessageCreateData, text string, keyboard *qqbotsdk.Keyboard) {
	if _, err := h.sendPrivateMessageWithKeyboard(ctx, data.Author.UserOpenID, text,
		data.ID, keyboard); err != nil {
		h.deps.Logger.Warn("could not answer a private command",
			"member", data.Author.UserOpenID, "error", err)
	}
}

// durationPattern matches a number with a unit, in either script.
var durationPattern = regexp.MustCompile(`^(\d+)\s*(秒|分钟|分|小时|天|s|m|h|d)$`)

// parseDuration reads a duration such as 30s, 10分钟, 2h or 1天.
func parseDuration(text string) (time.Duration, error) {
	trimmed := strings.TrimSpace(strings.ToLower(text))
	match := durationPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return 0, fmt.Errorf("无法识别的时长 %q；请用 30s / 10m / 2h / 1d，"+
			"或 30秒 / 10分 / 10分钟 / 2小时 / 1天", text)
	}
	amount, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("无法识别的时长 %q", text)
	}
	if amount <= 0 {
		return 0, fmt.Errorf("时长必须大于零，收到 %q", text)
	}

	var unit time.Duration
	switch match[2] {
	case "s", "秒":
		unit = time.Second
	case "m", "分", "分钟":
		unit = time.Minute
	case "h", "小时":
		unit = time.Hour
	case "d", "天":
		unit = 24 * time.Hour
	}
	return time.Duration(amount) * unit, nil
}

// humanDuration renders a duration the way the commands accept it.
func humanDuration(duration time.Duration) string {
	switch {
	case duration%(24*time.Hour) == 0:
		return fmt.Sprintf("%d天", int(duration/(24*time.Hour)))
	case duration%time.Hour == 0:
		return fmt.Sprintf("%d小时", int(duration/time.Hour))
	case duration%time.Minute == 0:
		return fmt.Sprintf("%d分钟", int(duration/time.Minute))
	default:
		return fmt.Sprintf("%d秒", int(duration/time.Second))
	}
}

// contains reports whether a list holds a value.
func contains(list []string, value string) bool {
	for _, entry := range list {
		if strings.TrimSpace(entry) == value {
			return true
		}
	}
	return false
}
