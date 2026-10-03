// Package admincmd answers administrator commands sent in a group.
//
// The commands are restricted to a list configured per group, so being a group
// administrator in QQ grants nothing here: only the people named in the
// configuration can drive the bot. A command that names no target is refused
// with an explanation, because the platform gives the bot no way to address a
// member by their QQ number.
//
// Supported commands, with the English alias beside each:
//
//	/禁言 <时长> [@目标]      /mute
//	/重新验证 [@目标]         /reverify
//	/debug 超时测试 [@目标]   /debug timeout
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
	return &handler{cfg: cfg, deps: deps, seen: map[string]time.Time{}}, nil
}

// firstSight reports whether this message has not been handled yet.
//
// Entries older than a minute are dropped, because the same message is never
// delivered twice after that.
func (h *handler) firstSight(messageID string) bool {
	if messageID == "" {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for id, at := range h.seen {
		if now.Sub(at) > time.Minute {
			delete(h.seen, id)
		}
	}
	if _, already := h.seen[messageID]; already {
		return false
	}
	h.seen[messageID] = now
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

	// mu guards seen, which stops one message being acted on twice.
	mu sync.Mutex
	// seen holds the message ids handled recently. A full receive group can
	// report the same message as both a mention event and an ordinary one, and
	// acting twice would mute twice and answer twice.
	seen map[string]time.Time

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
	panelCtx, panelCancel := context.WithTimeout(context.Background(), 20*time.Second)
	h.publishCommands(panelCtx)
	panelCancel()
	// Both event types are always read, and require_mention is enforced on the
	// message itself instead.
	//
	// Which event carries a mention depends on the group's receive setting, not
	// on the command: a group set to receive everything delivers a message that
	// mentions the bot as GROUP_MESSAGE_CREATE, as production showed, while a
	// mention-only group delivers the same message as GROUP_AT_MESSAGE_CREATE.
	// Registering only one of them made the bot unable to see commands at all
	// in one of the two modes. A message that arrives as both is handled once,
	// because handler.firstSight drops the second delivery.
	h.deps.Client.RegisterFunc(qqbotsdk.EventGroupAtMessageCreate, h.onMessage)
	h.deps.Client.RegisterFunc(qqbotsdk.EventGroupMessageCreate, h.onMessage)
	// A single chat carries one command and nothing else: reading a receipt,
	// which is the only way an administrator can see the model's own words about
	// a member without the group reading them too. It is registered whatever the
	// configuration says about receipts, because the handler decides what to
	// answer rather than the registration.
	h.deps.Client.RegisterFunc(qqbotsdk.EventC2CMessageCreate, h.onPrivateMessage)
	// The buttons under a receipt. Every feature that listens for interactions
	// is handed every press, so the handler starts by asking whether the button
	// is one of ours.
	h.deps.Client.RegisterFunc(qqbotsdk.EventInteractionCreate, h.onInteraction)
	h.deps.Logger.Info("administrator commands are ready",
		"prefix", h.cfg.Prefix, "debug", h.cfg.Debug,
		"require_mention", h.mentionsRequired(),
		"whois_admin_only", h.whoisAdminOnly(),
		"groups", len(h.cfg.Groups))
	return nil
}

// Close implements feature.Feature.
func (h *handler) Close(context.Context) error { return nil }

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
		// A group that is not configured yet still has to answer /whois. Running
		// it there is how a group gets configured at all: its openid is the first
		// thing the file needs, and until then nothing else can be set up.
		//
		// Every other command is dropped, because there would be no
		// administrator list to check the sender against, and the command
		// handling below is built around one. The sender is never an
		// administrator here, so they see only their own openid.
		command, ok := parseCommand(data.Content, h.cfg.Prefix)
		if !ok || command.name != "whois" {
			return nil
		}
		if h.mentionsRequired() && !h.botWasMentioned(event, command) {
			return nil
		}
		if !h.firstSight(data.ID) {
			return nil
		}
		h.deps.Logger.Info("answering /whois in a group that is not configured yet",
			"group", data.GroupOpenID, "member", data.Author.MemberOpenID)
		h.whois(ctx, data, false)
		return nil
	}

	if !h.firstSight(data.ID) {
		// The same message arrived as a second event type.
		return nil
	}

	command, ok := parseCommand(data.Content, h.cfg.Prefix)
	if !ok {
		return nil
	}
	// With require_mention on, a command counts only when the message reached
	// the bot because it was mentioned.
	if h.mentionsRequired() && !h.botWasMentioned(event, command) {
		return nil
	}
	sender := data.Author.MemberOpenID
	if sender == "" {
		return nil
	}

	group, known := h.cfg.Groups[data.GroupOpenID]
	isAdmin := known && contains(group.Admins, sender)

	if command.name == "whois" {
		// A group with no administrators is one being set up, and /whois is how
		// its administrator list gets written, so it stays open until then.
		//
		// There is deliberately no "is a group administrator" branch beside
		// this one. The platform refuses to let this application read a member's
		// role or the member list at all (40012010 应用无接口访问权限, measured),
		// so such a branch could never answer yes and would only look like a
		// check that exists. OneBot can read the roles, but it reports QQ numbers
		// while the bot is only ever given an openid, and nothing converts one
		// into the other. The rule in force is therefore exactly this: the
		// configured administrators, plus the setting-up case.
		settingUp := !known || len(group.Admins) == 0
		if isAdmin || settingUp || !h.whoisAdminOnly() {
			h.whois(ctx, data, isAdmin)
			return nil
		}
		h.deps.Logger.Warn("refused /whois for a member who is not an administrator",
			"group", data.GroupOpenID, "member", sender)
		h.reply(ctx, data, "你没有权限使用管理命令。")
		return nil
	}

	switch command.name {
	case "菜单", "menu", "help", "帮助":
		// The list is help rather than a management action, so it is answered
		// for every member: "你没有权限使用管理命令。" would only puzzle somebody
		// asking what the bot can do, and it reveals nothing a member could not
		// already see when an administrator mistypes a command.
		h.reply(ctx, data, usage(h.cfg.Prefix))
		return nil
	case "违规查询", "回执", "receipt", "violation":
		// Any member may ask. What comes back without the button is the summary:
		// who was judged, what was found, what was done -- all of which this
		// group already saw. The model's own words about a member are behind a
		// button that only an administrator may press, and that press is checked
		// again when it arrives.
		return h.receiptCommand(ctx, data, command)
	case "违规举报", "违规反馈", "report":
		// Reporting is for every member, which is the point of it: the people who
		// see an advertisement are not only the administrators. What follows a
		// report is decided by the judgement and the configuration, never by who
		// raised it, so nothing about managing the group is reachable this way.
		//
		// Dispatched here, above the administrator check, and the placement is the
		// fix for a real bug: the report used to be handled inside the switch behind
		// that gate, so every ordinary member was answered "你没有权限使用管理命令。"
		// The tests missed it because every one of them reported as an administrator.
		return h.reportCommand(ctx, data, command)
	}

	if !isAdmin {
		h.deps.Logger.Warn("refused a command from a member who is not on the administrator list",
			"group", data.GroupOpenID, "member", sender, "command", command.name)
		h.reply(ctx, data, "你没有权限使用管理命令。")
		return nil
	}
	h.deps.Logger.Info("an administrator command arrived",
		"group", data.GroupOpenID, "member", sender, "command", command.name)
	return h.run(ctx, data, group, command)
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
func (h *handler) botWasMentioned(event *qqbotsdk.Event, command parsedCommand) bool {
	if event.Type == qqbotsdk.EventGroupAtMessageCreate {
		return true
	}
	if h.botOpenID == "" {
		// The bot's own openid could not be read, so the mention cannot be
		// checked against it. Refusing every command would be worse than
		// accepting a leading mention, so that is what is left; the warning is
		// logged once, when the openid is looked for.
		return command.botOpenID != ""
	}
	return command.botOpenID == h.botOpenID
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

// run dispatches one command.
func (h *handler) run(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	group GroupConfig, command parsedCommand) error {
	// The target is resolved inside the branches that need one. Resolving it
	// here made every command answer "请 @ 目标成员" before its own logic could
	// run, which is what turned /菜单 into a complaint about a missing target
	// instead of the command list.
	switch command.name {
	case "禁言", "mute":
		target, err := targetOf(data, command)
		if err != nil {
			h.reply(ctx, data, err.Error())
			return nil
		}
		limit, err := group.longestMute()
		if err != nil {
			return err
		}
		return h.mute(ctx, data, command, target, limit)
	case "重新验证", "reverify", "verify":
		target, err := targetOf(data, command)
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
	case "解禁", "unmute":
		target, err := targetOf(data, command)
		if err != nil {
			h.reply(ctx, data, err.Error())
			return nil
		}
		return h.unmute(ctx, data, target)
	case "重新发送验证", "重发验证", "resend":
		target, err := targetOf(data, command)
		if err != nil {
			h.reply(ctx, data, err.Error())
			return nil
		}
		return h.resendVerification(ctx, data, target)
	case "黑名单", "blacklist":
		return h.blacklistCommand(ctx, data, command)
	case "debug":
		return h.debug(ctx, data, command)
	default:
		h.reply(ctx, data, usage(h.cfg.Prefix))
		return nil
	}
}

// debug handles the /debug subcommands.
func (h *handler) debug(ctx context.Context, data *qqbotsdk.GroupMessageCreateData,
	command parsedCommand) error {
	if !h.cfg.Debug {
		h.reply(ctx, data, "调试功能未开启。")
		return nil
	}
	if len(command.args) == 0 {
		h.reply(ctx, data, "用法："+h.cfg.Prefix+"debug 超时测试 [@目标]")
		return nil
	}
	sub := command.args[0]
	switch sub {
	case "超时测试", "timeout":
		if h.verifier == nil {
			h.reply(ctx, data, "本机器人没有启用入群验证功能。")
			return nil
		}
		// Only this subcommand needs a target, so it is resolved here rather
		// than for the debug command as a whole.
		target, err := targetOf(data, command)
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
		if mentionInText.MatchString(arg) {
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
func blacklistTarget(command parsedCommand) (string, string, error) {
	if len(command.args) < 2 {
		return "", "", errors.New("请 @ 目标成员，或直接给出 openid：" +
			command.name + " <@目标|openid> [原因]")
	}
	key := command.args[1]
	if match := mentionInText.FindStringSubmatch(key); match != nil {
		key = match[1]
	}
	if strings.TrimSpace(key) == "" {
		return "", "", errors.New("没有识别出 openid")
	}
	return key, strings.Join(command.args[2:], " "), nil
}

// blacklistCommand manages the list of applicants barred from joining.
//
// It manages a list; it does not police a group. Nothing here removes anybody
// who is already in one, and nothing here undoes anything by itself: the list
// only decides how a join request is answered.
func (h *handler) blacklistCommand(ctx context.Context,
	data *qqbotsdk.GroupMessageCreateData, command parsedCommand) error {
	if h.blacklist == nil {
		h.reply(ctx, data, "本机器人没有可用的数据层，黑名单命令不可用。")
		return nil
	}
	prefix := h.cfg.Prefix
	if len(command.args) == 0 {
		h.reply(ctx, data, "用法：\n"+prefix+"黑名单 add <@目标|openid> [原因]\n"+
			prefix+"黑名单 remove <@目标|openid>\n"+prefix+"黑名单 list")
		return nil
	}

	switch command.args[0] {
	case "add", "添加", "加":
		key, reason, err := blacklistTarget(command)
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
		key, _, err := blacklistTarget(command)
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
		h.reply(ctx, data, "未知的黑名单子命令："+command.args[0]+
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
	command parsedCommand, target string, limit time.Duration) error {
	// The duration may come before or after the target: both orders read
	// naturally, and the target itself comes from the mention rather than from
	// an argument, so its position carries no meaning. An argument that is a
	// mention is skipped instead of being read as a duration, which is what
	// made "/禁言 @某人 5d" fail with 无法识别的时长 "@某人".
	wanted, err := durationIn(command.args)
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

// mentionInText matches a mention left in the message text as markup.
var mentionInText = regexp.MustCompile(`<@!?([0-9A-Za-z_-]{8,})>`)

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
func targetOf(data *qqbotsdk.GroupMessageCreateData, command parsedCommand) (string, error) {
	// The text comes first: the mention of the bot has already been removed
	// from it, so anything still mentioned is the target.
	if match := mentionInText.FindStringSubmatch(command.tail); match != nil {
		return match[1], nil
	}
	// Then the list, skipping the bot's own mention. A full receive event lists
	// every mention in the order they appear, so the bot is usually first and
	// taking the first entry would try to act on the bot itself, which the
	// platform refuses with 40103004.
	for _, mention := range data.Mentions {
		if mention.MemberOpenID == "" || mention.MemberOpenID == command.botOpenID {
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
		command.name)
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

// parsedCommand is one recognised command.
type parsedCommand struct {
	// name is the command word, as written.
	name string
	// args are the words after it.
	args []string
	// tail is the message with the leading mention of the bot removed.
	//
	// The target is searched for in here rather than in the raw content,
	// because a message that starts by mentioning the bot would otherwise offer
	// the bot itself as the first candidate.
	tail string
	// botOpenID is the member the message opened by mentioning, which is the
	// bot itself. It has to be named explicitly because a full receive event
	// lists every mention, the bot included, whatever the documentation says.
	botOpenID string
}

// mentionMarkup matches the mention the platform leaves in the text when the
// bot runs in full receive mode.
var mentionMarkup = regexp.MustCompile(`^\s*<@!?([0-9A-Za-z_-]+)>\s*`)

// parseCommand reads a command out of a message.
func parseCommand(content, prefix string) (parsedCommand, bool) {
	botOpenID := ""
	if match := mentionMarkup.FindStringSubmatch(content); match != nil {
		botOpenID = match[1]
	}
	text := mentionMarkup.ReplaceAllString(content, "")
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, prefix) {
		return parsedCommand{}, false
	}
	words := strings.Fields(strings.TrimPrefix(text, prefix))
	if len(words) == 0 {
		return parsedCommand{}, false
	}
	return parsedCommand{name: words[0], args: words[1:], tail: text, botOpenID: botOpenID}, true
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
