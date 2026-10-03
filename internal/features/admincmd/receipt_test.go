package admincmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// The two groups a receipt test needs: one the asker administers and one they do
// not, so that "wrong group" and "wrong person" can be told apart.
const (
	receiptGroup      = "GROUP-OPENID"
	receiptOtherGroup = "OTHER-GROUP-OPENID"
	// receiptGroupNumber is the number the group is known by, which is what a
	// summary shows instead of the openid nobody has seen.
	receiptGroupNumber = 123456789
)

// receiptHarness builds the feature over a real database holding one judgement.
//
// The record is the whole subject of the command, so it is a real row in a real
// database rather than a stub: a receipt that read the wrong fields would look
// perfectly fine against a map.
func receiptHarness(t *testing.T, entry store.Judgement) (*harness, store.JudgementStore) {
	t.Helper()
	h := newHarness(t, `
enabled: true
groups:
  `+receiptGroup+`:
    admins: ["ADMIN-OPENID"]
  `+receiptOtherGroup+`:
    admins: ["ADMIN-OPENID", "SOMEONE-ELSE"]
`)
	opened, err := store.Open(context.Background(), store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "receipt.db"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { opened.Close() })

	judgements := opened.Judgements()
	if err := judgements.Record(context.Background(), entry); err != nil {
		t.Fatalf("recording the judgement: %v", err)
	}
	h.handler.deps.Store = opened
	// Both groups are managed, which is what makes the wrong-group case a test of
	// the boundary rather than of the bot ignoring an unknown group entirely.
	h.handler.deps.Groups = config.Groups{
		{OpenID: receiptGroup, QQGroupID: receiptGroupNumber},
		{OpenID: receiptOtherGroup, QQGroupID: receiptGroupNumber + 1},
	}
	// The label a receipt shows comes from the moderation feature, which owns the
	// categories; the stub answers with what it was built with.
	h.handler.SetModeration(&stubJudge{label: "广告"})
	return h, judgements
}

// aJudgement is one recorded violation with everything a receipt should show.
func aJudgement(id string) store.Judgement {
	return store.Judgement{
		ID:             id,
		GroupOpenID:    receiptGroup,
		SubjectOpenID:  "SUBJECT-OPENID",
		ReporterOpenID: "REPORTER-OPENID",
		Category:       "ad",
		Verdict:        store.JudgementViolation,
		Model:          "stub-model",
		MessageIDs:     []string{"M-1", "M-2"},
		Reason:         "发布低价 API 中转价目表推广信息，属广告",
		Reasoning:      "它先列了价目，再给出查价命令，这是招揽。",
		Action:         "已撤回 2 条消息 已禁言 10分钟",
		MuteSeconds:    600,
		RecallReason:   "已撤回 2 条消息",
		Recalls: []store.RecallOutcome{
			{ID: "M-1", Number: 1, Text: "💰 本站价目（元 / 百万 tokens）", Recalled: true},
			{ID: "M-2", Number: 2, Text: "加群送皮肤", Recalled: false, Reason: "无操作权限"},
		},
		CreatedAt: time.Now().Unix(),
	}
}

// TestAnyoneReadsTheSummaryOfAReceipt covers the half a group gets, which any
// member may now ask for.
//
// What must not be in it is the point: a reason, a chain of thought and the text
// of a withdrawn message are all a model's or a member's own words about somebody,
// and none of them belongs in a group.
func TestAnyoneReadsTheSummaryOfAReceipt(t *testing.T) {
	entry := aJudgement("a1b2c3d4e5f60718")
	h, _ := receiptHarness(t, entry)

	// An ordinary member, asking in a group, about a record of that group.
	h.send("/违规查询 a1b2c3d4", "ORDINARY-MEMBER", receiptGroup)

	reply := h.lastReply()
	if reply == "" {
		t.Fatal("the command was not answered")
	}
	for _, want := range []string{
		// The title is one bold line, a rule, and then the body.
		"**违规回执 " + entry.ID + "**",
		receiptDivider,
		"违规·广告",
		`<qqbot-at-user id="` + entry.SubjectOpenID + `"/>`,
		`<qqbot-at-user id="` + entry.ReporterOpenID + `"/>`,
		// Every field is labelled and the label is bold: the message is markdown,
		// and eight plain lines of "key：value" read as a wall.
		"**时间**：",
		// The group is named by its number, which is what its members know.
		fmt.Sprintf("**群**：%d", receiptGroupNumber),
		"**判定**：",
		"**被判定人**：",
		"**举报人**：",
		"**模型**：stub-model",
		"**送检消息**：2 条",
		"**处理**：已撤回 2 条消息 已禁言 10分钟",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, reply)
		}
	}
	for _, forbidden := range []string{
		entry.Reason, entry.Reasoning, "本站价目", "加群送皮肤", "思考过程",
		// The duration is already inside the action line, so it gets no line of
		// its own: "已禁言 10分钟" above "禁言时长：10分钟" is the same fact twice,
		// and a reader checks both to see whether they agree.
		"禁言时长",
		// The openid belongs to the details: it is what an administrator needs
		// when they go to the configuration, and nobody in a group has ever seen
		// it.
		entry.GroupOpenID,
	} {
		if strings.Contains(reply, forbidden) {
			t.Errorf("the summary leaks %q:\n%s", forbidden, reply)
		}
	}
	// The label, not the configuration key: the group was told 「广告」.
	if strings.Contains(reply, "违规·ad") {
		t.Errorf("the summary shows the category key rather than its label:\n%s", reply)
	}
	// And the rest is behind a button, which the platform greys out for anybody
	// who is not an administrator.
	if data := h.buttonOf(); data != receiptDetailPrefix+entry.ID {
		t.Errorf("button data = %q, want the details button for this record", data)
	}
}

// TestTheDetailsAreBehindAnAdministratorOnlyButton covers the other half.
//
// The details are the model's own words about a member and the message that was
// withdrawn, so they need an administrator -- and the check is made again when
// the press arrives, because the platform's greyed-out button and this bot's
// administrator list are two different things.
func TestTheDetailsAreBehindAnAdministratorOnlyButton(t *testing.T) {
	entry := aJudgement("a1b2c3d4e5f60718")
	h, _ := receiptHarness(t, entry)
	button := receiptDetailPrefix + entry.ID

	t.Run("an administrator gets the details", func(t *testing.T) {
		before := h.groupReplies()
		h.press("INTERACTION-1", button, testAdmin, receiptGroup)

		if code, ok := h.lastAnswer(); !ok || code != float64(qqbotsdk.InteractionCodeSuccess) {
			t.Fatalf("answer = %v, want success", code)
		}
		if h.groupReplies() != before+1 {
			t.Fatal("no detailed receipt was sent")
		}
		details := h.lastReply()
		for _, want := range []string{
			entry.Reason, entry.Reasoning, "本站价目",
			// The openid is here rather than in the summary: it is what an
			// administrator needs when they go to the configuration.
			"**群 openid**：" + entry.GroupOpenID,
			"**违规回执 " + entry.ID + " 详细信息**",
			receiptDivider,
		} {
			if !strings.Contains(details, want) {
				t.Errorf("the details do not mention %q:\n%s", want, details)
			}
		}
		// The details carry their own button, which is what takes them back.
		if data := h.buttonOf(); !strings.HasPrefix(data, receiptRecallPrefix) {
			t.Errorf("button data = %q, want a recall button on the details", data)
		}
	})

	t.Run("an ordinary member is refused", func(t *testing.T) {
		before := h.groupReplies()
		h.press("INTERACTION-2", button, "ORDINARY-MEMBER", receiptGroup)

		if code, ok := h.lastAnswer(); !ok ||
			code != float64(qqbotsdk.InteractionCodeAdminOnly) {
			t.Fatalf("answer = %v, want the administrator-only code", code)
		}
		if h.groupReplies() != before {
			t.Error("the details were sent to somebody who may not read them")
		}
	})

	t.Run("another group's administrator is refused", func(t *testing.T) {
		before := h.groupReplies()
		h.press("INTERACTION-3", button, "SOMEONE-ELSE", receiptOtherGroup)
		if code, ok := h.lastAnswer(); !ok ||
			code != float64(qqbotsdk.InteractionCodeAdminOnly) {
			t.Fatalf("answer = %v, want the administrator-only code", code)
		}
		if h.groupReplies() != before {
			t.Error("the details were sent into the wrong group")
		}
	})
}

// TestTheRecallButtonTakesTheDetailsBack covers the button that cleans up after
// itself.
//
// A detailed receipt is the one message in a group that must not stay: it holds
// the model's reasoning and the text of a message that was withdrawn. The button
// exists so that whoever read it can make it go away, and nothing is announced
// afterwards -- the button said what it would do.
func TestTheRecallButtonTakesTheDetailsBack(t *testing.T) {
	entry := aJudgement("a1b2c3d4e5f60718")
	h, _ := receiptHarness(t, entry)

	h.press("INTERACTION-1", receiptDetailPrefix+entry.ID, testAdmin, receiptGroup)
	recall := h.buttonOf()
	if !strings.HasPrefix(recall, receiptRecallPrefix) {
		t.Fatalf("button data = %q, want a recall button on the details", recall)
	}
	details := h.lastSentID()
	if details == "" {
		t.Fatal("the detailed receipt was never sent")
	}

	before := h.groupReplies()
	h.press("INTERACTION-2", recall, testAdmin, receiptGroup)

	if code, ok := h.lastAnswer(); !ok || code != float64(qqbotsdk.InteractionCodeSuccess) {
		t.Fatalf("answer = %v, want success", code)
	}
	taken := h.recalledMessages()
	if len(taken) != 1 || taken[0] != details {
		t.Fatalf("recalled = %v, want the detailed receipt %q itself", taken, details)
	}
	// Silent: the group is told nothing, because the message disappearing is the
	// answer and an announcement would be the bot talking to itself.
	if h.groupReplies() != before {
		t.Errorf("a message was sent into the group about the recall:\n%s", h.lastReply())
	}
	// The button is spent. Pressing it again must not take back whatever message
	// happens to hold the same token next.
	h.press("INTERACTION-3", recall, testAdmin, receiptGroup)
	if code, _ := h.lastAnswer(); code != float64(qqbotsdk.InteractionCodeFailed) {
		t.Errorf("answer = %v for a spent button, want failure", code)
	}
	if len(h.recalledMessages()) != 1 {
		t.Errorf("a second message was taken back: %v", h.recalledMessages())
	}
}

// TestARecallButtonOnlyForItsOwnAdministrator covers the second button's
// permission, which is checked the same way as the first.
func TestARecallButtonOnlyForItsOwnAdministrator(t *testing.T) {
	entry := aJudgement("a1b2c3d4e5f60718")
	h, _ := receiptHarness(t, entry)
	h.press("INTERACTION-1", receiptDetailPrefix+entry.ID, testAdmin, receiptGroup)
	recall := h.buttonOf()

	h.press("INTERACTION-2", recall, "ORDINARY-MEMBER", receiptGroup)
	if code, _ := h.lastAnswer(); code != float64(qqbotsdk.InteractionCodeNoPermission) {
		t.Errorf("answer = %v, want the no-permission code", code)
	}
	if len(h.recalledMessages()) != 0 {
		t.Errorf("a message was taken back for somebody who may not: %v",
			h.recalledMessages())
	}
}

// TestAReceiptSaysWhatWasDecided covers the line a reader looks at first.
//
// The column holds ok, violation and error, which is what the code acts on and
// not something to put in front of a person: "判定：ok" is a key out of a
// database. The distinction it has to keep is the one the column exists for -- a
// judgement nobody reached must never read as "nothing was found".
func TestAReceiptSaysWhatWasDecided(t *testing.T) {
	cases := map[string]struct {
		entry store.Judgement
		want  string
	}{
		"a violation": {
			entry: aJudgement("a1b2c3d4e5f60718"),
			want:  "违规·广告",
		},
		"nothing was found": {
			entry: func() store.Judgement {
				entry := aJudgement("a1b2c3d4e5f60718")
				entry.Verdict = store.JudgementOK
				entry.Category = ""
				entry.Action = ""
				return entry
			}(),
			want: "无违规",
		},
		"nobody looked": {
			entry: func() store.Judgement {
				entry := aJudgement("a1b2c3d4e5f60718")
				entry.Verdict = store.JudgementError
				entry.Category = ""
				entry.Action = ""
				return entry
			}(),
			want: "未能判定",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := receiptHarness(t, testCase.entry)
			h.send("/违规查询 a1b2c3d4", testAdmin, receiptGroup)

			reply := h.lastReply()
			if !strings.Contains(reply, "**判定**："+testCase.want) {
				t.Errorf("the receipt says something else:\n%s", reply)
			}
			// And never the raw value behind it.
			for _, raw := range []string{"：ok", "：violation", "：error", "违规·ad"} {
				if strings.Contains(reply, raw) {
					t.Errorf("the receipt shows %q:\n%s", raw, reply)
				}
			}
		})
	}
}

// TestAReceiptIsRefusedOutsideItsOwnGroup covers the boundary a group needs.
//
// A member of one group has no business reading another group's punishments, and
// the refusal deliberately does not say where the record really is: that would
// tell them the number exists somewhere.
func TestAReceiptIsRefusedOutsideItsOwnGroup(t *testing.T) {
	h, _ := receiptHarness(t, aJudgement("a1b2c3d4e5f60718"))

	h.send("/违规查询 a1b2c3d4", testAdmin, receiptOtherGroup)

	reply := h.lastReply()
	if !strings.Contains(reply, "本群没有这条记录") {
		t.Errorf("reply = %q, want a refusal that does not name the other group", reply)
	}
	if strings.Contains(reply, "价目") || strings.Contains(reply, "招揽") ||
		strings.Contains(reply, "被判定人") {
		t.Errorf("the refusal leaked the record:\n%s", reply)
	}
}

// TestAReceiptIsReadInPrivate covers the second way in: the details can be read
// without the group reading them too, which is the whole reason a single chat is
// wired up at all.
func TestAReceiptIsReadInPrivate(t *testing.T) {
	entry := aJudgement("a1b2c3d4e5f60718")
	h, _ := receiptHarness(t, entry)

	// The administrator of the record's own group.
	h.deliverPrivate("/违规查询 a1b2c3d4", testAdmin)

	reply := h.lastPrivateReply()
	if reply == "" {
		t.Fatal("the private command was not answered")
	}
	// The same two layers as a group: the summary, and the details behind the
	// button rather than in the same message.
	if !strings.Contains(reply, "违规回执 "+entry.ID) {
		t.Fatalf("reply = %q, want the summary", reply)
	}
	for _, forbidden := range []string{entry.Reason, entry.Reasoning, "本站价目"} {
		if strings.Contains(reply, forbidden) {
			t.Errorf("the private summary leaks %q:\n%s", forbidden, reply)
		}
	}
	if data := h.buttonOf(); data != receiptDetailPrefix+entry.ID {
		t.Fatalf("button data = %q, want the details button", data)
	}
	// A single chat has no administrators for the platform to grey the button out
	// for, so it is shown to the one reader and the handler is what decides.
	if permission := h.buttonPermissionOf(); permission != float64(qqbotsdk.PermissionTypeEveryone) {
		t.Errorf("permission = %v, want the button pressable in a single chat", permission)
	}
	// The answer goes into the single chat and nowhere else: a receipt in the
	// group would publish exactly what this path exists to keep quiet.
	if h.groupReplies() != 0 {
		t.Errorf("%d messages reached a group, want none", h.groupReplies())
	}

	// Pressing it sends the details into the single chat, with no button of their
	// own: there is nobody to hide them from, and nothing to take back.
	h.pressPrivate("INTERACTION-1", receiptDetailPrefix+entry.ID, testAdmin)
	if code, ok := h.lastAnswer(); !ok || code != float64(qqbotsdk.InteractionCodeSuccess) {
		t.Fatalf("answer = %v, want success", code)
	}
	details := h.lastPrivateReply()
	for _, want := range []string{
		entry.Reason, entry.Reasoning, "本站价目", "已撤回 1 条，未撤回 1 条",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("the private details are missing %q:\n%s", want, details)
		}
	}
	if h.lastMessageCarriedKeyboard() {
		t.Error("the private details carry a keyboard, which only a group's may")
	}
	if h.groupReplies() != 0 {
		t.Errorf("%d messages reached a group, want none", h.groupReplies())
	}
}

// TestAPrivateReceiptShowsDetailsOnlyToItsOwnAdministrator covers the private
// permission, which is per record rather than per person.
//
// Everybody may read the summary, here as in a group; the model's words are for
// the administrator of the group the record belongs to. Being an administrator of
// some other group is not enough, and the refusal says the same thing either way,
// because a different one would say which group the record belongs to.
func TestAPrivateReceiptShowsDetailsOnlyToItsOwnAdministrator(t *testing.T) {
	entry := aJudgement("a1b2c3d4e5f60718")
	h, _ := receiptHarness(t, entry)

	for name, member := range map[string]string{
		"an administrator of another group": "SOMEONE-ELSE",
		"a member who administers nothing":  "ORDINARY-MEMBER",
	} {
		t.Run(name, func(t *testing.T) {
			h.deliverPrivate("/违规查询 a1b2c3d4", member)
			reply := h.lastPrivateReply()
			if !strings.Contains(reply, "违规回执 "+entry.ID) {
				t.Fatalf("reply = %q, want the summary", reply)
			}
			for _, forbidden := range []string{
				entry.Reason, entry.Reasoning, "本站价目", "思考过程",
			} {
				if strings.Contains(reply, forbidden) {
					t.Errorf("the summary leaked %q:\n%s", forbidden, reply)
				}
			}

			// And the button does not open for them. The platform cannot grey it
			// out here -- a single chat has no administrators -- so the handler
			// is the whole check rather than the second half of one.
			sent := h.privateReplies()
			h.pressPrivate("INTERACTION-PRIVATE", receiptDetailPrefix+entry.ID, member)
			if code, ok := h.lastAnswer(); !ok ||
				code != float64(qqbotsdk.InteractionCodeAdminOnly) {
				t.Errorf("answer = %v, want the administrator-only code", code)
			}
			if h.privateReplies() != sent {
				t.Errorf("the details were sent to somebody who may not read them:\n%s",
					h.lastPrivateReply())
			}
		})
	}
}

// TestAReceiptThatCannotBeFoundSaysSo covers the ordinary mistake, and the two
// ways a lookup can fail to answer.
func TestAReceiptThatCannotBeFoundSaysSo(t *testing.T) {
	h, judgements := receiptHarness(t, aJudgement("a1b2c3d4e5f60718"))

	t.Run("a number nothing was recorded under", func(t *testing.T) {
		h.send("/违规查询 zzzzzzzz", testAdmin, receiptGroup)
		if reply := h.lastReply(); !strings.Contains(reply, "没有找到回执单号") {
			t.Errorf("reply = %q, want a miss", reply)
		}
	})
	t.Run("a prefix that means more than one record", func(t *testing.T) {
		if err := judgements.Record(context.Background(),
			aJudgement("a1b2c3ffffffffff")); err != nil {
			t.Fatalf("recording a second judgement: %v", err)
		}
		h.send("/违规查询 a1b2c3", testAdmin, receiptGroup)
		reply := h.lastReply()
		if !strings.Contains(reply, "匹配到不止一条记录") {
			t.Errorf("reply = %q, want an ambiguous prefix refused", reply)
		}
		// Refused rather than guessed: showing the wrong punishment's reason to
		// an administrator is worse than showing none.
		if strings.Contains(reply, "价目") {
			t.Errorf("a guess was made:\n%s", reply)
		}
	})
	t.Run("no number at all", func(t *testing.T) {
		h.send("/违规查询", testAdmin, receiptGroup)
		if reply := h.lastReply(); !strings.Contains(reply, "用法") {
			t.Errorf("reply = %q, want the usage line", reply)
		}
	})
}

// TestAPrivateReceiptIsQuietWithoutACommand covers what a single chat must not do:
// answer everything typed into it. A bot that replies to any private message is a
// bot that talks to itself and to anybody who says hello.
func TestAPrivateReceiptIsQuietWithoutACommand(t *testing.T) {
	h, _ := receiptHarness(t, aJudgement("a1b2c3d4e5f60718"))

	h.deliverPrivate("你好", testAdmin)
	if reply := h.lastPrivateReply(); reply != "" {
		t.Errorf("an ordinary private message was answered with %q", reply)
	}
	h.deliverPrivate("/菜单", testAdmin)
	if reply := h.lastPrivateReply(); !strings.Contains(reply, "违规查询") {
		t.Errorf("reply = %q, want what a single chat can be asked for", reply)
	}
}

// TestTheGroupReplyCarriesTheReceiptNumber covers the half of the feature a group
// actually sees.
//
// The number is what makes a punishment answerable later, so it has to be in the
// sentence the group gets -- and it is only a number: nothing about what was
// found or why belongs in a group.
func TestTheGroupReplyCarriesTheReceiptNumber(t *testing.T) {
	judge := &stubJudge{verdict: feature.ModerationVerdict{
		JudgementID:   "a1b2c3d4e5f60718",
		Category:      "ad",
		Label:         "广告",
		MuteSeconds:   600,
		SubjectOpenID: "SUBJECT-OPENID",
		RecallMessages: []feature.JudgedMessage{
			{ID: "M-1", Index: "IDX-1", Number: 1, Text: "加群送皮肤"},
		},
		Reason: "卖号广告",
	}}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-QUOTED"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	waitFor(t, func() bool { return h.lastReply() != reportWaiting })

	reply := h.lastReply()
	assertChip(t, reply, "/违规查询 a1b2c3d4", "a1b2c3d4")
	if strings.Contains(reply, judge.verdict.Reason) {
		t.Errorf("the model's words reached the group: %q", reply)
	}

	// And what was recorded is the act, per message, with the reason it did or
	// did not happen: that is what the receipt is read back from.
	outcome := judge.lastOutcome
	if len(outcome.Recalls) != 1 || !outcome.Recalls[0].Recalled {
		t.Fatalf("outcome = %+v, want one message taken back", outcome)
	}
	if outcome.Recalls[0].Text != "加群送皮肤" {
		t.Errorf("recalled text = %q, want the message itself", outcome.Recalls[0].Text)
	}
	if outcome.RecallReason == "" {
		t.Error("nothing was recorded about why the message was taken back")
	}
	// Only a message that was really withdrawn is marked as dealt with, and it is
	// marked by its index, which is what the cache is keyed by.
	if len(judge.punished) != 1 || judge.punished[0] != "IDX-1" {
		t.Errorf("punished = %v, want the message that was withdrawn", judge.punished)
	}
	if judge.punishedGroup != testGroupOpenID {
		t.Errorf("punished group = %q, want the group it happened in", judge.punishedGroup)
	}
}

// TestAMessageThatCouldNotBeWithdrawnIsNotMarked covers the other side of the
// mark.
//
// A message that stayed in the group is still there to be read, so it is still
// something a later report may legitimately be about. Marking it would hide an
// advertisement from the judge forever, which is worse than the problem the mark
// was added to solve.
func TestAMessageThatCouldNotBeWithdrawnIsNotMarked(t *testing.T) {
	judge := &stubJudge{verdict: feature.ModerationVerdict{
		JudgementID:   "a1b2c3d4e5f60718",
		Category:      "ad",
		Label:         "广告",
		MuteSeconds:   600,
		SubjectOpenID: "SUBJECT-OPENID",
		RecallMessages: []feature.JudgedMessage{
			{ID: "M-1", Index: "IDX-1", Number: 1, Text: "别人发的广告"},
		},
	}}
	// The platform refuses the delete, as it does for a message this application
	// may not take down.
	h := reportHarness(t, judge)
	h.failDeletes = true
	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-QUOTED"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	waitFor(t, func() bool { return h.lastReply() != reportWaiting })

	if len(judge.punished) != 0 {
		t.Errorf("punished = %v, want nothing marked when nothing was withdrawn",
			judge.punished)
	}
	outcome := judge.lastOutcome
	if len(outcome.Recalls) != 1 || outcome.Recalls[0].Recalled {
		t.Fatalf("outcome = %+v, want the message recorded as not taken back", outcome)
	}
	if outcome.Recalls[0].Reason == "" {
		t.Error("nothing was recorded about why the message stayed")
	}
}

// TestADryRunCarriesTheSameTappableReceipt covers the other sentence that carries
// a number.
//
// A dry run is where the feature is tried out, so it is the one place the chip is
// most likely to be looked at -- and a chip there and a bare number after a real
// violation would look like two different features.
func TestADryRunCarriesTheSameTappableReceipt(t *testing.T) {
	judge := &stubJudge{dryRun: true, verdict: feature.ModerationVerdict{
		JudgementID:   "a1b2c3d4e5f60718",
		Category:      "ad",
		Label:         "广告",
		MuteSeconds:   600,
		SubjectOpenID: "SUBJECT-OPENID",
	}}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-QUOTED"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	waitFor(t, func() bool { return h.lastReply() != reportWaiting })

	reply := h.lastReply()
	if !strings.Contains(reply, "试运行") {
		t.Fatalf("reply = %q, want the dry-run wording", reply)
	}
	assertChip(t, reply, "/违规查询 a1b2c3d4", "a1b2c3d4")
}

// TestTheChipCarriesAnEncodedCommand covers the encoding, which is the part the
// platform fails silently on.
//
// A value that is not percent-encoded arrives cut short at its first space or
// slash, so the chip would look right in the source and insert half a command.
func TestTheChipCarriesAnEncodedCommand(t *testing.T) {
	chip := commandInput("/违规查询 a1b2c3d4", "a1b2c3d4")
	if !strings.HasPrefix(chip, `<qqbot-cmd-input text="`) ||
		!strings.HasSuffix(chip, `" reference="false" />`) {
		t.Fatalf("chip = %q, want the platform's embedded-text tag", chip)
	}
	if strings.Contains(chip, " ") != strings.Contains(chip, " />") {
		// The only space allowed is the one before the closing slash.
		t.Errorf("chip = %q, want no raw spaces inside the values", chip)
	}
	assertAttribute(t, chip, "text", "/违规查询 a1b2c3d4")
	assertAttribute(t, chip, "show", "a1b2c3d4")

	// A plus is a plus in a tag value: QueryEscape writes a space as one, and the
	// platform would read it literally.
	spaced := commandInput("/违规查询 ab cd", "show")
	if strings.Contains(spaced, "+") {
		t.Errorf("chip = %q, want no plus for a space", spaced)
	}
	assertAttribute(t, spaced, "text", "/违规查询 ab cd")
}

// assertChip checks that text contains one chip whose value decodes to the
// command, and whose label is what the group reads.
func assertChip(t *testing.T, text, wantCommand, wantShow string) {
	t.Helper()
	if !strings.Contains(text, "<qqbot-cmd-input ") {
		t.Fatalf("no chip in %q", text)
	}
	assertAttribute(t, text, "text", wantCommand)
	assertAttribute(t, text, "show", wantShow)
	if !strings.Contains(text, "回执单号 ") {
		t.Errorf("the chip is not introduced as a receipt: %q", text)
	}
}

// assertAttribute decodes one attribute of the first tag in text.
func assertAttribute(t *testing.T, text, name, want string) {
	t.Helper()
	opening := `text="`
	if name == "show" {
		opening = `show="`
	}
	start := strings.Index(text, opening)
	if start < 0 {
		t.Fatalf("no %s attribute in %q", name, text)
	}
	rest := text[start+len(opening):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("the %s attribute is not closed in %q", name, text)
	}
	decoded, err := url.QueryUnescape(rest[:end])
	if err != nil {
		t.Fatalf("the %s attribute does not decode: %v", name, err)
	}
	if decoded != want {
		t.Errorf("%s = %q, want %q", name, decoded, want)
	}
}

// deliverPrivate hands one single-chat event to the dispatcher.
func (h *harness) deliverPrivate(content, sender string) {
	h.t.Helper()
	h.mu.Lock()
	h.sent++
	messageID := "PRIVATE-" + string(rune('0'+h.sent))
	h.mu.Unlock()
	body := `{
		"id": "` + messageID + `",
		"author": {"user_openid": "` + sender + `"},
		"content": ` + jsonString(content) + `
	}`
	payload := &qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: qqbotsdk.EventC2CMessageCreate,
		Data: json.RawMessage(body),
	}
	if err := h.client.Dispatcher().DispatchSync(context.Background(),
		qqbotsdk.NewEvent(payload, "test")); err != nil {
		h.t.Fatalf("dispatching a private message: %v", err)
	}
}

// waitFor gives a goroutine a moment to finish what the command started.
//
// The judgement runs off the event path on purpose, so a test that read the reply
// immediately would be reading the previous one.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the command did not answer in time")
}
