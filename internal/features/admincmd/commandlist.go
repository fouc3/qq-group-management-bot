package admincmd

import (
	"context"
	"strings"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
)

// The command list is written down once, here.
//
// The help text a member can ask for, the instruction panel the platform shows
// and the dispatcher that runs a command are all built from it. Written apart
// they would drift, and the panel is the one that goes quietly out of date:
// nobody re-reads a panel they cannot see.
//
// No other file in this package names a command. A command added here is added
// to the dispatch, the help and the panel at once, and a word that would invoke
// two of them is refused at startup instead of quietly shadowing one.

// panelRemark marks the panel this bot owns in groups.
//
// The panel is found by its remark instead of by an id kept somewhere: the
// remark travels with the panel, so a bot restarted on a fresh machine still
// finds the panel it published rather than leaving a second one behind.
const panelRemark = "qq-group-management-bot 指令面板"

// panelPrivateRemark marks the panel shown in a single chat.
//
// A separate remark from the group panel, because they are separate panels: the
// platform keys them by scope and this bot keeps the two in step by looking each
// one up by its own remark.
const panelPrivateRemark = panelRemark + "（私聊）"

// groupPanel is where most of these commands are offered.
//
// Written as a function so that the table below still reads as what each command
// is, rather than as a list of repeated declarations.
func groupPanel() []command.PanelPlacement {
	return []command.PanelPlacement{{Scene: command.InGroup}}
}

// bothPanels is for the commands a single chat answers as well as a group.
func bothPanels() []command.PanelPlacement {
	return []command.PanelPlacement{
		{Scene: command.InGroup},
		{Scene: command.InPrivate},
	}
}

// commandDefs is what the bot answers, in the order it is shown.
func (h *handler) commandDefs() []command.Def {
	return []command.Def{
		{
			Name:    "菜单",
			Aliases: []string{"menu", "help", "帮助"},
			Usage:   "{prefix}菜单 —— 显示这份列表",
			Desc:    "显示可用命令",
			// The list is help rather than a management action, so it is answered
			// for every member. "你没有权限使用管理命令。" would only puzzle
			// somebody asking what the bot can do, and it reveals nothing a
			// member could not already see when an administrator mistypes a
			// command.
			Audience: command.Everyone,
			// Offered in both panels: a single chat answers it with its own list
			// rather than a line of its own, which is why PrivateUsage is left
			// out -- a line here would be a line added to that list.
			Panels:  bothPanels(),
			Run:     h.menuCommand,
			Private: h.privateMenu,
		},
		{
			Name: "whois",
			Usage: "{prefix}whois —— 查看本群与成员的 openid（用于填配置）；" +
				"私聊里用则显示你自己的 openid",
			Desc: "查看本群与成员 openid",
			// The one command answered in a group the bot's own list does not
			// name yet: running it there is how a group's openid is discovered,
			// and until that is written down nothing else can be configured.
			Audience:            command.Whois,
			InUnconfiguredGroup: true,
			// Both panels, like every other command that is worth offering: a single chat
			// is where somebody reads their own openid, and a command that is only in
			// the help is one they have to be told about first.
			Panels: bothPanels(),
			Run:    h.whoisCommand,
			// A single chat has no group to describe, but it is where somebody can
			// read their own openid before they have said anything in a group. It is
			// the same value a group event carries, so one list entry serves both.
			PrivateUsage: "{prefix}whois —— 查看你自己的 openid（私聊里用）",
			Private:      h.privateWhois,
		},
		{
			Name:    "禁言",
			Aliases: []string{"mute"},
			Usage: "{prefix}禁言 <时长> [@目标]，顺序随意（30s / 10m / 2h / 1d，或 30秒 / 10分 / 2小时 / 1天）" +
				"\n（@ 取不到目标时，可改为回复引用目标的消息）",
			Desc:     "禁言成员，最长 29 天",
			Audience: command.Admins,
			Panels:   groupPanel(),
			Run:      h.muteCommand,
		},
		{
			Name:     "解禁",
			Aliases:  []string{"unmute"},
			Usage:    "{prefix}解禁 [@目标] —— 解除禁言；正在验证中的成员不受此命令影响",
			Desc:     "解除禁言",
			Audience: command.Admins,
			Panels:   groupPanel(),
			Run:      h.unmuteCommand,
		},
		{
			Name:     "重新发送验证",
			Aliases:  []string{"重发验证", "resend"},
			Usage:    "{prefix}重新发送验证 [@目标] —— 给正在验证中的成员重发验证通知",
			Desc:     "重发验证通知",
			Audience: command.Admins,
			Panels:   groupPanel(),
			Run:      h.resendCommand,
		},
		{
			Name:     "重新验证",
			Aliases:  []string{"reverify", "verify"},
			Usage:    "{prefix}重新验证 [@目标]",
			Desc:     "重新发起验证",
			Audience: command.Admins,
			Panels:   groupPanel(),
			Run:      h.reverifyCommand,
		},
		{
			Name:     "黑名单",
			Aliases:  []string{"blacklist"},
			Usage:    "{prefix}黑名单 add|remove|list —— 管理禁止加群名单（只影响加群申请，不踢人）",
			Desc:     "管理禁止加群名单",
			Audience: command.Admins,
			Panels:   groupPanel(),
			Run:      h.blacklistCommand,
		},
		{
			// The list that makes every message of a member cost a judgement. It is
			// offered next to the blacklist because it is written by the same people,
			// about the same kind of member, and does the opposite thing with them:
			// the blacklist keeps somebody out of the group, while this watches
			// somebody who is already in it.
			Name:    "高风险",
			Aliases: []string{"highrisk"},
			Usage: "{prefix}高风险 add <@目标|openid> <时长> [原因] | remove <@目标|openid> | " +
				"list —— 名单内的人每条消息都送 AI 判定，违规自动撤回并禁言（时长必填，不写永久）",
			Desc:     "标记高风险用户（每条消息送检）",
			Audience: command.Admins,
			Panels:   groupPanel(),
			Run:      h.highRiskCommand,
		},
		{
			// Reporting is for every member, which is the point of it: the people
			// who see an advertisement are not only the administrators.
			//
			// That this can be said here at all is the point of the audience being
			// a field. The check used to sit inside the switch that ran a command,
			// so an ordinary member reporting an advertisement was answered
			// "你没有权限使用管理命令。" -- and the tests missed it because every one
			// of them reported as an administrator.
			Name:     "违规举报",
			Aliases:  []string{"违规反馈", "report"},
			Usage:    "{prefix}违规举报 —— 引用一条消息举报（任何成员可用；也可写作{prefix}违规反馈）",
			Desc:     "引用消息举报违规",
			Audience: command.Everyone,
			Panels:   groupPanel(),
			Run:      h.reportCommand,
			// Offered only when there is something behind it. The judge is handed
			// over after this table is built, which is why it is asked rather
			// than read once.
			Available: h.judgingEnabled,
		},
		{
			// The receipt number is what a group is told when somebody is
			// punished, and this is how an administrator finds out what it stood
			// for. It answers in a group -- about that group's records -- and in a
			// private message with the bot, where the model's own words can be
			// read without the group reading them too.
			Name:    "违规查询",
			Aliases: []string{"回执", "receipt", "violation"},
			// Any member may ask. What comes back without the button is the
			// summary -- who was judged, what was found, what was done -- all of
			// which this group already saw. The model's own words about a member
			// are behind a button that only an administrator may press, and that
			// press is checked again when it arrives.
			Usage:        "{prefix}违规查询 <回执单号> —— 查看一条违规回执（任何成员可用；详细内容需管理员点击按钮）",
			Desc:         "查看违规回执",
			Audience:     command.Everyone,
			Panels:       bothPanels(),
			Run:          h.receiptCommand,
			Private:      h.privateReceipt,
			PrivateUsage: "{prefix}违规查询 <回执单号> —— 查看一条违规判定的详细记录",
		},
		{
			Name:  "debug",
			Usage: "{prefix}debug 超时测试 [@目标]（需开启调试）",
			Desc:  "调试用",
			// A tool for whoever runs the bot, not an entry to put in front of a
			// group. It declares no panel, which is the only way a command stays
			// out of one; the help still lists it, because there it can say what
			// it is.
			Audience: command.Admins,
			Run:      h.debug,
		},
	}
}

// judgingEnabled reports whether there is a judge behind the report command.
func (h *handler) judgingEnabled() bool {
	return h.moderation != nil && h.moderation.JudgingEnabled()
}

// usageText is the help a group is answered with.
func (h *handler) usageText() string {
	return "可用命令：" + strings.Join(h.commands().Usage(h.cfg.Prefix), "\n")
}

// privateUsageText is what a single chat shows as available.
func (h *handler) privateUsageText() string {
	return strings.Join(h.commands().PrivateUsage(h.cfg.Prefix), "\n")
}

// panelItems renders one scene's panel from the table.
//
// Which commands appear where is what the table declares, so the group panel and
// the single-chat one are two readings of one list rather than two lists that
// drift. Why the panel is a menu rather than a lock -- only_admin is never sent
// unless a command asks for it -- is documented in the command package.
func (h *handler) panelItems(scene command.Scene) []qqbotsdk.PanelItem {
	return h.commands().Panel(scene, h.cfg.Prefix)
}

// panelPlace is one panel this bot keeps: where it is shown, which scene's
// commands it holds, and the remark it is found by.
type panelPlace struct {
	scope  string
	remark string
	scene  command.Scene
}

// panelPlaces are the panels this bot publishes.
//
// Both, always: a member who never writes in a group can still be shown what the
// bot answers, and the single-chat panel is the only place that says so. Each is
// published on its own, so one failing does not take the other with it.
func (h *handler) panelPlaces() []panelPlace {
	return []panelPlace{
		{scope: qqbotsdk.PanelScopeGroup, remark: panelRemark, scene: command.InGroup},
		{scope: qqbotsdk.PanelScopeC2C, remark: panelPrivateRemark, scene: command.InPrivate},
	}
}

// publishCommands registers the command list as the instruction panel, once for
// each place it is shown: in a group, and in a single chat.
//
// A failure is reported and swallowed. The panel is how a member discovers the
// commands; it is not what makes them work, and a bot that refused to start
// because a panel could not be published would take every command down with it.
func (h *handler) publishCommands(ctx context.Context) {
	if !h.registersCommands() {
		return
	}
	for _, place := range h.panelPlaces() {
		h.publishPanel(ctx, place)
	}
}

// publishPanel brings one panel up to date, creating it when the bot has not
// published it yet.
func (h *handler) publishPanel(ctx context.Context, place panelPlace) {
	items := h.panelItems(place.scene)
	if len(items) == 0 {
		// Nothing to offer there, so there is nothing to publish. A panel with no
		// entries would be an empty menu in front of every member.
		return
	}
	panel := &qqbotsdk.Panel{Remark: place.remark, Items: items}

	existing, err := h.findPanel(ctx, place)
	switch {
	case err != nil:
		h.deps.Logger.Warn("could not look for an existing instruction panel, "+
			"so the commands are not published", "scope", place.scope, "error", err)
		return
	case existing != nil:
		if err := h.updatePanel(ctx, existing, panel); err != nil {
			h.deps.Logger.Warn("could not update the instruction panel",
				"scope", place.scope, "error", err)
			return
		}
		h.deps.Logger.Info("the instruction panel was updated",
			"scope", place.scope, "panel_id", existing.PanelID, "items", len(panel.Items))
	default:
		targetType, groupOpenIDs := h.panelTargets(place.scope)
		panelID, err := h.deps.Client.CreatePanel(ctx, &qqbotsdk.PanelCreateRequest{
			Scope:        place.scope,
			TargetType:   targetType,
			GroupOpenIDs: groupOpenIDs,
			Panel:        panel,
		})
		if err != nil {
			h.deps.Logger.Warn("could not publish the instruction panel",
				"scope", place.scope, "error", err)
			return
		}
		h.deps.Logger.Info("the instruction panel was published",
			"scope", place.scope, "panel_id", panelID, "items", len(panel.Items),
			"groups", len(groupOpenIDs))
	}
}

// panelTargets says what a panel applies to.
//
// The group panel applies to the groups being managed, which is the list that
// changes between starts. The single-chat panel applies to everybody who writes
// to the bot, because there is no list of users to keep -- the people who may use
// it are whoever the platform lets open a conversation with it.
func (h *handler) panelTargets(scope string) (string, []string) {
	if scope != qqbotsdk.PanelScopeGroup {
		return qqbotsdk.PanelTargetAll, nil
	}
	return h.panelTargetType(), h.panelGroupOpenIDs()
}

// findPanel returns the panel this bot owns in one scope, or nil when there is
// none.
func (h *handler) findPanel(ctx context.Context, place panelPlace) (*qqbotsdk.PanelRecord, error) {
	cursor := ""
	// Bounded rather than unbounded: a bot with more panels than this is not a
	// case worth looping forever over, and the one being looked for would have
	// been found in the first pages.
	for page := 0; page < 10; page++ {
		list, err := h.deps.Client.ListPanels(ctx, place.scope, cursor,
			qqbotsdk.MaxPanelPageSize)
		if err != nil {
			return nil, err
		}
		for index := range list.Records {
			record := &list.Records[index]
			if record.Panel != nil && record.Panel.Remark == place.remark {
				return record, nil
			}
		}
		if list.IsEnd || list.NextCursor == "" {
			return nil, nil
		}
		cursor = list.NextCursor
	}
	return nil, nil
}

// updatePanel rewrites the panel's content and brings its groups up to date.
func (h *handler) updatePanel(ctx context.Context, existing *qqbotsdk.PanelRecord,
	panel *qqbotsdk.Panel) error {
	if existing.Panel != nil {
		panel.Version = existing.Panel.Version
	}
	if _, err := h.deps.Client.UpdatePanel(ctx, existing.PanelID, panel); err != nil {
		return err
	}
	return h.syncPanelTargets(ctx, existing)
}

// syncPanelTargets makes the panel apply to exactly the groups being managed.
//
// The list of groups is what changes between starts -- a group is added, one is
// removed -- and a panel that kept applying to a group nobody manages any more
// would advertise commands that do not answer there.
func (h *handler) syncPanelTargets(ctx context.Context, existing *qqbotsdk.PanelRecord) error {
	if existing.TargetType != qqbotsdk.PanelTargetSpecific {
		return nil
	}
	wanted := h.panelGroupOpenIDs()
	if len(wanted) == 0 {
		return nil
	}
	detail, err := h.deps.Client.GetPanel(ctx, existing.PanelID)
	if err != nil {
		return err
	}

	// What the platform says it is holding, not what was sent. A panel that
	// quietly lost an entry looks exactly like one that never had it, and this
	// is the only place that can tell the difference.
	if detail.Panel != nil {
		var held []string
		for _, item := range detail.Panel.Items {
			held = append(held, item.Name)
		}
		h.deps.Logger.Info("the instruction panel is holding",
			"panel_id", existing.PanelID, "items", len(held),
			"names", strings.Join(held, " "))
	}

	have := make(map[string]bool, len(detail.GroupOpenIDs))
	for _, openID := range detail.GroupOpenIDs {
		have[openID] = true
	}
	var add, remove []string
	for _, openID := range wanted {
		if !have[openID] {
			add = append(add, openID)
		}
	}
	for _, openID := range detail.GroupOpenIDs {
		if !containsValue(wanted, openID) {
			remove = append(remove, openID)
		}
	}

	for _, batch := range batchOpenIDs(add) {
		if err := h.deps.Client.UpdatePanelTargets(ctx, existing.PanelID,
			&qqbotsdk.PanelTargetRequest{
				Op:           qqbotsdk.PanelTargetOpAdd,
				GroupOpenIDs: batch,
			}); err != nil {
			return err
		}
	}
	for _, batch := range batchOpenIDs(remove) {
		if err := h.deps.Client.UpdatePanelTargets(ctx, existing.PanelID,
			&qqbotsdk.PanelTargetRequest{
				Op:           qqbotsdk.PanelTargetOpDel,
				GroupOpenIDs: batch,
			}); err != nil {
			return err
		}
	}
	return nil
}

// batchOpenIDs splits openids into the groups one request may carry.
func batchOpenIDs(openIDs []string) [][]string {
	const perRequest = 20 // the documented ceiling per target request
	var batches [][]string
	for start := 0; start < len(openIDs); start += perRequest {
		end := start + perRequest
		if end > len(openIDs) {
			end = len(openIDs)
		}
		batches = append(batches, openIDs[start:end])
	}
	return batches
}

// containsValue reports whether a list holds a value.
func containsValue(list []string, value string) bool {
	for _, entry := range list {
		if entry == value {
			return true
		}
	}
	return false
}

// panelGroupOpenIDs is the groups the panel applies to, or nil for all of them.
//
// nil when the configuration names no group, which means every group the bot is
// in. Applying the panel to none of them would be applying it to nobody.
func (h *handler) panelGroupOpenIDs() []string {
	if len(h.deps.Groups) == 0 {
		return nil
	}
	openIDs := make([]string, 0, len(h.deps.Groups))
	for _, group := range h.deps.Groups {
		openIDs = append(openIDs, group.OpenID)
	}
	return openIDs
}

// panelTargetType says how the panel chooses its groups.
func (h *handler) panelTargetType() string {
	if len(h.deps.Groups) == 0 {
		return qqbotsdk.PanelTargetAll
	}
	return qqbotsdk.PanelTargetSpecific
}

// registersCommands reports whether the panel should be published.
func (h *handler) registersCommands() bool {
	return h.cfg.RegisterCommands == nil || *h.cfg.RegisterCommands
}
