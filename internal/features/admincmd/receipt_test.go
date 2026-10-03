package admincmd

import (
	"context"
	"encoding/json"
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
		{OpenID: receiptGroup},
		{OpenID: receiptOtherGroup},
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

// TestAnAdministratorReadsAReceipt covers the whole point of the number the group
// is shown: what it stood for, including the parts that are gone.
//
// The two things that cannot be found anywhere else afterwards are here -- the
// text of a message that was withdrawn, and the model's own reasoning -- so the
// test asserts on them rather than on the shape of the reply.
func TestAnAdministratorReadsAReceipt(t *testing.T) {
	entry := aJudgement("a1b2c3d4e5f60718")
	h, _ := receiptHarness(t, entry)

	// Half a receipt, the way somebody types one back off a screen.
	h.send("/违规查询 a1b2c3d4", testAdmin, receiptGroup)

	reply := h.lastReply()
	if reply == "" {
		t.Fatal("the command was not answered")
	}
	for _, want := range []string{
		"违规回执 " + entry.ID,
		receiptGroup,
		"违规·广告",
		entry.Reason,
		entry.Reasoning,
		"已撤回 2 条消息",
		"💰 本站价目",     // the message that is gone, kept
		"未撤回（无操作权限）", // and the one that stayed, with why
		"加群送皮肤",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("the receipt does not mention %q:\n%s", want, reply)
		}
	}
	// The label, not the configuration key: the group was told 「广告」, so the
	// administrator reads the same word back.
	if strings.Contains(reply, "违规·ad") {
		t.Errorf("the receipt shows the category key rather than its label:\n%s", reply)
	}
}

// TestAReceiptIsRefusedOutsideItsOwnGroup covers the boundary a group needs.
//
// An administrator of one group has no business reading another group's
// punishments, and the refusal deliberately does not say where the record really
// is: that would tell an ordinary member who happened to type the command that
// the number exists somewhere.
func TestAReceiptIsRefusedOutsideItsOwnGroup(t *testing.T) {
	h, _ := receiptHarness(t, aJudgement("a1b2c3d4e5f60718"))

	// The same administrator, asking in a group that is not the record's.
	h.send("/违规查询 a1b2c3d4", testAdmin, receiptOtherGroup)

	reply := h.lastReply()
	if !strings.Contains(reply, "本群没有这条记录") {
		t.Errorf("reply = %q, want a refusal that does not name the other group", reply)
	}
	if strings.Contains(reply, "价目") || strings.Contains(reply, "招揽") {
		t.Errorf("the refusal leaked the record:\n%s", reply)
	}
}

// TestAReceiptIsRefusedForAMemberWhoIsNotAnAdministrator covers the group side of
// the permission: the command is behind the administrator list, and unlike
// /whois there is no setting that opens it.
func TestAReceiptIsRefusedForAMemberWhoIsNotAnAdministrator(t *testing.T) {
	h, _ := receiptHarness(t, aJudgement("a1b2c3d4e5f60718"))

	h.send("/违规查询 a1b2c3d4", "ORDINARY-MEMBER", receiptGroup)

	reply := h.lastReply()
	if !strings.Contains(reply, "你没有权限使用管理命令") {
		t.Errorf("reply = %q, want the ordinary refusal", reply)
	}
	if strings.Contains(reply, "价目") {
		t.Errorf("an ordinary member was shown the record:\n%s", reply)
	}
}

// TestAReceiptIsReadInPrivate covers the second way in, which is the point of
// allowing it at all: the model's words about a member can be read without the
// group reading them too.
func TestAReceiptIsReadInPrivate(t *testing.T) {
	entry := aJudgement("a1b2c3d4e5f60718")
	h, _ := receiptHarness(t, entry)

	h.deliverPrivate("/违规查询 a1b2c3d4", testAdmin)

	reply := h.lastPrivateReply()
	if reply == "" {
		t.Fatal("the private command was not answered")
	}
	if !strings.Contains(reply, entry.Reasoning) ||
		!strings.Contains(reply, "💰 本站价目") {
		t.Errorf("the private receipt is missing what it is for:\n%s", reply)
	}
	// The answer goes into the single chat and nowhere else: a receipt in the
	// group would publish exactly what this path exists to keep quiet.
	if h.groupReplies() != 0 {
		t.Errorf("%d messages reached a group, want none", h.groupReplies())
	}
}

// TestAPrivateReceiptIsOnlyForItsOwnGroup covers the permission on the private
// path, which is per record rather than per person.
//
// Being an administrator of some group is not enough, and the refusal says the
// same thing either way -- an administrator of a different group and somebody who
// administers nothing are told the same sentence, because a different one would
// say which group the record belongs to.
func TestAPrivateReceiptIsOnlyForItsOwnGroup(t *testing.T) {
	h, _ := receiptHarness(t, aJudgement("a1b2c3d4e5f60718"))

	for name, member := range map[string]string{
		"an administrator of another group": "SOMEONE-ELSE",
		"a member who administers nothing":  "ORDINARY-MEMBER",
	} {
		t.Run(name, func(t *testing.T) {
			h.deliverPrivate("/违规查询 a1b2c3d4", member)
			reply := h.lastPrivateReply()
			if !strings.Contains(reply, "你没有权限查看这条记录") {
				t.Errorf("reply = %q, want the refusal", reply)
			}
			if strings.Contains(reply, "价目") || strings.Contains(reply, "招揽") {
				t.Errorf("the refusal leaked the record:\n%s", reply)
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
	if !strings.Contains(reply, "回执单号 a1b2c3d4") {
		t.Errorf("reply = %q, want the receipt number", reply)
	}
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
