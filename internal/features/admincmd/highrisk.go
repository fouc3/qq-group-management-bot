package admincmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// highRiskListLimit caps how much of the list one command prints, for the same
// reason the blacklist is capped: a list long enough to be worth searching is a
// list long enough not to be pasted whole into a group.
const highRiskListLimit = 20

// highRiskCommand manages the list of members whose messages are judged as they
// arrive.
//
// It manages a list and nothing else: the window, the model, the record and the
// punishment belong to the moderation feature, and the only thing this command
// adds is the half a feature cannot do for itself -- somebody deciding that a
// person is worth watching.
//
// Every mark carries the moment it ends, and there is no permanent one. What a
// mark buys is a judgement of every message that member sends, which is a running
// cost and a running decision; a decision that never comes up again is one nobody
// revisits, which is how a list becomes a fact about the past rather than a policy.
func (h *handler) highRiskCommand(ctx context.Context,
	data *qqbotsdk.GroupMessageCreateData, cmd command.Parsed) error {
	prefix := h.cfg.Prefix
	usage := "用法：\n" + prefix + "高风险 add <@目标|openid> <时长> [原因]\n" +
		prefix + "高风险 remove <@目标|openid>\n" + prefix + "高风险 list\n" +
		"时长必填（30s / 10m / 2h / 7d…）：名单内的人每条消息都会送 AI 判定，不写永久。"
	if h.moderation == nil {
		h.reply(ctx, data, "本机器人没有可用的判定模块，高风险名单不可用。")
		return nil
	}
	if len(cmd.Args) == 0 {
		h.reply(ctx, data, usage)
		return nil
	}

	switch cmd.Args[0] {
	case "add", "添加", "加":
		member, duration, reason, err := h.highRiskAdd(data, cmd)
		if err != nil {
			h.reply(ctx, data, err.Error()+"\n"+usage)
			return nil
		}
		until := time.Now().Add(duration)
		id, err := newHighRiskID()
		if err != nil {
			h.reply(ctx, data, "生成条目编号失败："+err.Error())
			return nil
		}
		if err := h.moderation.Watch(ctx, store.Watch{
			ID:           id,
			MemberOpenID: member,
			Reason:       reason,
			AddedAt:      time.Now().Unix(),
			AddedBy:      senderOpenID(data),
			ExpiresAt:    until.Unix(),
		}); err != nil {
			h.deps.Logger.Error("could not mark a member high risk", "error", err)
			h.reply(ctx, data, "标记失败："+err.Error())
			return nil
		}
		h.deps.Logger.Info("an administrator marked a member high risk",
			"group", data.GroupOpenID, "subject", member,
			"minutes", int(duration.Minutes()))
		answer := "已标记高风险：`" + member + "`，至 " +
			until.Format("2006-01-02 15:04") + "。他之后发的每条消息都会送 AI 判定，" +
			"判定违规会自动撤回并按类别禁言（不会在群里提示）。"
		if reason != "" {
			answer += "\n备注：" + reason
		}
		h.reply(ctx, data, answer)
		return nil

	case "remove", "移除", "删除", "del", "解除":
		member, err := targetOf(data, cmd)
		if err != nil {
			h.reply(ctx, data, err.Error()+"\n"+usage)
			return nil
		}
		removed, err := h.moderation.Unwatch(ctx, member)
		if err != nil {
			h.deps.Logger.Error("could not remove a high risk mark", "error", err)
			h.reply(ctx, data, "解除失败："+err.Error())
			return nil
		}
		if !removed {
			// Said rather than reported as done: an operator who is told a list
			// changed when it did not will believe the next thing they are told.
			h.reply(ctx, data, "高风险名单里没有 `"+member+"`。")
			return nil
		}
		h.deps.Logger.Info("an administrator removed a high risk mark",
			"group", data.GroupOpenID, "subject", member)
		h.reply(ctx, data, "已解除高风险：`"+member+"`。他的消息不再逐条送检。")
		return nil

	case "list", "列表", "列出":
		entries, err := h.moderation.Watches(ctx)
		if err != nil {
			h.deps.Logger.Error("could not read the high risk list", "error", err)
			h.reply(ctx, data, "读取高风险名单失败："+err.Error())
			return nil
		}
		if len(entries) == 0 {
			h.reply(ctx, data, "高风险名单是空的。")
			return nil
		}
		if len(entries) > highRiskListLimit {
			entries = entries[:highRiskListLimit]
		}
		lines := []string{"**高风险名单**（前 " + strconv.Itoa(len(entries)) +
			" 条，每条消息送 AI 判定）"}
		for _, entry := range entries {
			line := "- `" + entry.MemberOpenID + "` 至 " +
				time.Unix(entry.ExpiresAt, 0).Format("2006-01-02 15:04")
			if entry.Reason != "" {
				line += " —— " + entry.Reason
			}
			lines = append(lines, line)
		}
		h.reply(ctx, data, strings.Join(lines, "\n"))
		return nil

	default:
		h.reply(ctx, data, "未知的高风险子命令："+cmd.Args[0]+
			"。可用：add / remove / list")
		return nil
	}
}

// newHighRiskID returns the key a mark is stored under.
//
// Random rather than the member's openid, so that a mark which is replaced and then
// made again is not the same row by accident: the id is what the log of the
// decision refers to, and two decisions are two decisions.
func newHighRiskID() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generating a high risk id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// highRiskAdd reads one mark: the member, the duration it must carry, and whatever
// is left of the words as the reason.
//
// The target is resolved the way every command with a target resolves it, because
// the platform delivers a mention in three shapes depending on the group's receive
// setting -- and in one of them the @ name is taken out of the text altogether.
// Failing that, an openid written out by hand is accepted, which is how a group
// being set up names somebody whose mention the platform stripped.
//
// The duration is required rather than defaulted: a mark is a decision with an end,
// and one whose end nobody wrote down would be a permanence nobody chose.
func (h *handler) highRiskAdd(data *qqbotsdk.GroupMessageCreateData,
	cmd command.Parsed) (member string, duration time.Duration, reason string, err error) {
	words := make([]string, 0, len(cmd.Args))
	for _, arg := range cmd.Args[1:] {
		if trimmed := strings.TrimSpace(arg); trimmed != "" {
			words = append(words, trimmed)
		}
	}

	durationAt := -1
	for index, word := range words {
		if _, mentioned := command.FirstMention(word); mentioned {
			continue
		}
		if parsed, parseErr := parseDuration(word); parseErr == nil {
			duration, durationAt = parsed, index
			break
		}
	}
	if durationAt < 0 {
		return "", 0, "", errors.New("标记高风险必须给时长（30s / 10m / 2h / 7d…），" +
			"例如 /高风险 add @某人 7d 广告")
	}

	member, err = targetOf(data, cmd)
	if err != nil {
		// Nothing the platform named, so the target is an openid written out. The
		// error from the resolver is kept for the day there is nothing at all to
		// take: it is the one message that says how the platform delivers a
		// mention and what to do instead.
		fallback := ""
		for index, word := range words {
			if index == durationAt {
				continue
			}
			if _, mentioned := command.FirstMention(word); mentioned {
				continue
			}
			fallback = word
			break
		}
		if fallback == "" {
			return "", 0, "", err
		}
		member = fallback
	}

	var kept []string
	for index, word := range words {
		if index == durationAt || word == member {
			continue
		}
		if _, mentioned := command.FirstMention(word); mentioned {
			continue
		}
		kept = append(kept, word)
	}
	return member, duration, strings.Join(kept, " "), nil
}
