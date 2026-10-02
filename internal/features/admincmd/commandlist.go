package admincmd

import (
	"context"
	"strings"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// The command list is written down once, here.
//
// The help text a member can ask for and the instruction panel the platform
// shows are both built from it. Two lists would drift, and the panel is the one
// that goes quietly out of date: nobody re-reads a panel they cannot see.

// panelRemark marks the panel this bot owns.
//
// The panel is found by its remark instead of by an id kept somewhere: the
// remark travels with the panel, so a bot restarted on a fresh machine still
// finds the panel it published rather than leaving a second one behind.
const panelRemark = "qq-group-management-bot 指令面板"

// command is one thing the bot answers.
type command struct {
	// name invokes it. It is written with the prefix wherever it is shown, so
	// what a member copies is what the bot accepts.
	name string
	// usage is the help line, which may explain more than one line's worth.
	usage string
	// desc is the panel's explanation. The platform caps an entry at thirty
	// characters counting a Chinese character as two, so this stays short.
	desc string
	// adminOnly hides the entry from ordinary members in the panel. It mirrors
	// the check the bot makes when the command arrives; it does not replace it,
	// and it is only a convenience: anybody can still type the command.
	adminOnly bool
	// unregistered keeps a command out of the panel while leaving it in the
	// help. /debug is one -- a tool for whoever runs the bot, not an entry to
	// put in front of a group.
	unregistered bool
}

// commands lists what the bot answers, in the order it is shown.
func commands(prefix string) []command {
	return []command{
		{
			name:  "菜单",
			usage: prefix + "菜单 —— 显示这份列表",
			desc:  "显示可用命令",
		},
		{
			name:      "whois",
			usage:     prefix + "whois —— 查看本群与成员的 openid（用于填配置）",
			desc:      "查看本群与成员 openid",
			adminOnly: true,
		},
		{
			name: "禁言",
			usage: prefix + "禁言 <时长> [@目标]，顺序随意（30s / 10m / 2h / 1d，或 30秒 / 10分 / 2小时 / 1天）" +
				"\n（@ 取不到目标时，可改为回复引用目标的消息）",
			desc:      "禁言成员，最长 29 天",
			adminOnly: true,
		},
		{
			name:      "解禁",
			usage:     prefix + "解禁 [@目标] —— 解除禁言；正在验证中的成员不受此命令影响",
			desc:      "解除禁言",
			adminOnly: true,
		},
		{
			name:      "重新发送验证",
			usage:     prefix + "重新发送验证 [@目标] —— 给正在验证中的成员重发验证通知",
			desc:      "重发验证通知",
			adminOnly: true,
		},
		{
			name:      "重新验证",
			usage:     prefix + "重新验证 [@目标]",
			desc:      "重新发起验证",
			adminOnly: true,
		},
		{
			name:      "黑名单",
			usage:     prefix + "黑名单 add|remove|list —— 管理禁止加群名单（只影响加群申请，不踢人）",
			desc:      "管理禁止加群名单",
			adminOnly: true,
		},
		{
			name:         "debug",
			usage:        prefix + "debug 超时测试 [@目标]（需开启调试）",
			desc:         "调试用",
			unregistered: true,
		},
	}
}

// usage is the help text a reply carries.
func usage(prefix string) string {
	lines := []string{"可用命令："}
	for _, entry := range commands(prefix) {
		lines = append(lines, entry.usage)
	}
	return strings.Join(lines, "\n")
}

// panelItems renders the table as panel entries.
//
// only_admin is deliberately left unset, even for the commands that really are
// administrative. The platform reads that flag as "the group or channel
// administrators", which is its own notion of the role -- the group owner and
// whoever they appointed. What this bot obeys is the administrator list in the
// configuration, and the two are different sets.
//
// Sending the flag would therefore hide management commands from exactly the
// people the configuration names, whenever one of them holds no platform role.
// The bot refuses an ordinary member when the command arrives, which is the
// check that matters; the panel is a menu, not a lock.
//
// adminOnly in the table is kept anyway: it records which commands are
// administrative, so the next person can see the intent next to the command.
func (h *handler) panelItems() []qqbotsdk.PanelItem {
	var items []qqbotsdk.PanelItem
	for _, entry := range commands(h.cfg.Prefix) {
		if entry.unregistered {
			continue
		}
		items = append(items, qqbotsdk.PanelItem{
			Name: h.cfg.Prefix + entry.name,
			Desc: entry.desc,
			Type: qqbotsdk.PanelItemCommand,
		})
	}
	return items
}

// publishCommands registers the command list as the group instruction panel.
//
// A failure is reported and swallowed. The panel is how a member discovers the
// commands; it is not what makes them work, and a bot that refused to start
// because a panel could not be published would take every command down with it.
func (h *handler) publishCommands(ctx context.Context) {
	if !h.registersCommands() {
		return
	}

	panel := &qqbotsdk.Panel{Remark: panelRemark, Items: h.panelItems()}
	existing, err := h.findPanel(ctx)
	switch {
	case err != nil:
		h.deps.Logger.Warn("could not look for an existing instruction panel, "+
			"so the commands are not published", "error", err)
		return
	case existing != nil:
		if err := h.updatePanel(ctx, existing, panel); err != nil {
			h.deps.Logger.Warn("could not update the instruction panel", "error", err)
			return
		}
		h.deps.Logger.Info("the instruction panel was updated",
			"panel_id", existing.PanelID, "items", len(panel.Items))
	default:
		openIDs := h.panelGroupOpenIDs()
		panelID, err := h.deps.Client.CreatePanel(ctx, &qqbotsdk.PanelCreateRequest{
			Scope:        qqbotsdk.PanelScopeGroup,
			TargetType:   h.panelTargetType(),
			GroupOpenIDs: openIDs,
			Panel:        panel,
		})
		if err != nil {
			h.deps.Logger.Warn("could not publish the instruction panel", "error", err)
			return
		}
		h.deps.Logger.Info("the instruction panel was published",
			"panel_id", panelID, "items", len(panel.Items), "groups", len(openIDs))
	}
}

// findPanel returns the panel this bot owns, or nil when there is none.
func (h *handler) findPanel(ctx context.Context) (*qqbotsdk.PanelRecord, error) {
	cursor := ""
	// Bounded rather than unbounded: a bot with more panels than this is not a
	// case worth looping forever over, and the one being looked for would have
	// been found in the first pages.
	for page := 0; page < 10; page++ {
		list, err := h.deps.Client.ListPanels(ctx, qqbotsdk.PanelScopeGroup,
			cursor, qqbotsdk.MaxPanelPageSize)
		if err != nil {
			return nil, err
		}
		for index := range list.Records {
			record := &list.Records[index]
			if record.Panel != nil && record.Panel.Remark == panelRemark {
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
