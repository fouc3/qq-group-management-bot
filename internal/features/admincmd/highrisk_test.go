package admincmd

import (
	"strings"
	"testing"
	"time"
)

// The high risk command writes a list that makes every message of a member cost a
// judgement. What these cover is the command's own half: that a mark carries the
// person, the reason and the moment it ends, that it can be taken off again, and
// that a mark nobody gave an end to is refused rather than made permanent.

// TestTheHighRiskCommandMarksSomebody covers a mark being made: the reply, and the
// entry the moderation feature was handed.
func TestTheHighRiskCommandMarksSomebody(t *testing.T) {
	judge := &stubJudge{}
	h := reportHarness(t, judge)
	// The mention as a full receive group delivers it: left in the text as markup.
	h.send("/高风险 add <@!MEMBER-OPENID> 7d 广告", testAdmin, testGroupOpenID)

	if reply := h.lastReply(); !strings.Contains(reply, "已标记高风险") {
		t.Errorf("reply = %q, want it to say the mark was made", reply)
	}
	if len(judge.watched) != 1 {
		t.Fatalf("marks made = %d, want 1", len(judge.watched))
	}
	got := judge.watched[0]
	if got.MemberOpenID != "MEMBER-OPENID" {
		t.Errorf("marked %q, want the member that was named", got.MemberOpenID)
	}
	if got.Reason != "广告" {
		t.Errorf("reason = %q, want what was written after the duration", got.Reason)
	}
	if got.AddedBy != testAdmin {
		t.Errorf("added by %q, want the administrator who asked", got.AddedBy)
	}
	if got.ID == "" {
		t.Error("the mark has no id, so the log of the decision refers to nothing")
	}
	// Seven days, from now: the moment it ends is the whole reason there is no
	// permanent entry.
	remaining := time.Until(time.Unix(got.ExpiresAt, 0))
	if remaining < 6*24*time.Hour || remaining > 7*24*time.Hour {
		t.Errorf("the mark ends in %s, want the seven days that were asked for",
			remaining)
	}
}

// TestAMarkIsWrittenTheOtherTwoWays covers the shapes a group delivers a mention
// in, because the command has to work in whichever one the group is set to: the
// mention as its own list entry, and an openid written out by hand for a member
// whose mention the platform strips.
func TestAMarkIsWrittenTheOtherTwoWays(t *testing.T) {
	t.Run("the mention as a list entry", func(t *testing.T) {
		judge := &stubJudge{}
		h := reportHarness(t, judge)
		h.send("/高风险 add 2h 原因", testAdmin, testGroupOpenID, "MEMBER-OPENID")

		if len(judge.watched) != 1 {
			t.Fatalf("marks made = %d, want 1", len(judge.watched))
		}
		if got := judge.watched[0]; got.MemberOpenID != "MEMBER-OPENID" ||
			got.Reason != "原因" {
			t.Errorf("marked %+v, want the mentioned member with the reason", got)
		}
	})

	t.Run("an openid written out", func(t *testing.T) {
		judge := &stubJudge{}
		h := reportHarness(t, judge)
		h.send("/高风险 add MEMBER-OPENID 30m", testAdmin, testGroupOpenID)

		if len(judge.watched) != 1 {
			t.Fatalf("marks made = %d, want 1", len(judge.watched))
		}
		if got := judge.watched[0]; got.MemberOpenID != "MEMBER-OPENID" ||
			got.Reason != "" {
			t.Errorf("marked %+v, want the openid with no reason", got)
		}
	})
}

// TestAHighRiskMarkNeedsTheMemberAndAnEnd covers what the command refuses. Both
// refusals are the point of the list: a mark with no member is about nobody, and a
// mark with no end is a decision nobody will make again.
func TestAHighRiskMarkNeedsTheMemberAndAnEnd(t *testing.T) {
	judge := &stubJudge{}
	h := reportHarness(t, judge)

	// No duration: refused, and told what a duration looks like.
	h.send("/高风险 add <@!MEMBER-OPENID> 广告", testAdmin, testGroupOpenID)
	if reply := h.lastReply(); !strings.Contains(reply, "必须给时长") {
		t.Errorf("reply = %q, want the refusal to name the missing duration", reply)
	}
	if len(judge.watched) != 0 {
		t.Errorf("a mark with no end was made anyway: %+v", judge.watched)
	}

	// No member: refused, and told how to name one.
	h.send("/高风险 add 7d", testAdmin, testGroupOpenID)
	if reply := h.lastReply(); !strings.Contains(reply, "请 @ 目标成员") {
		t.Errorf("reply = %q, want the refusal to say how to name somebody", reply)
	}
	if len(judge.watched) != 0 {
		t.Errorf("a mark about nobody was made: %+v", judge.watched)
	}
}

// TestTheHighRiskListListsAndRemoves covers the other two subcommands, including
// the answer for a member who was not on the list: reporting a change that did not
// happen is what makes the next report believable.
func TestTheHighRiskListListsAndRemoves(t *testing.T) {
	judge := &stubJudge{}
	h := reportHarness(t, judge)

	h.send("/高风险 list", testAdmin, testGroupOpenID)
	if reply := h.lastReply(); !strings.Contains(reply, "空的") {
		t.Errorf("reply = %q, want it to say the list is empty", reply)
	}

	h.send("/高风险 add <@!MEMBER-OPENID> 2h 试一下", testAdmin, testGroupOpenID)
	h.send("/高风险 list", testAdmin, testGroupOpenID)
	listed := h.lastReply()
	if !strings.Contains(listed, "MEMBER-OPENID") || !strings.Contains(listed, "试一下") {
		t.Errorf("list = %q, want the mark and its reason", listed)
	}

	h.send("/高风险 remove <@!MEMBER-OPENID>", testAdmin, testGroupOpenID)
	if reply := h.lastReply(); !strings.Contains(reply, "已解除") {
		t.Errorf("reply = %q, want the removal to be reported", reply)
	}
	if len(judge.watched) != 0 {
		t.Errorf("the mark is still there after a removal: %+v", judge.watched)
	}

	// Removing it again is not a change, and is not reported as one.
	h.send("/高风险 remove <@!MEMBER-OPENID>", testAdmin, testGroupOpenID)
	if reply := h.lastReply(); !strings.Contains(reply, "没有") {
		t.Errorf("reply = %q, want it to say there was nothing to remove", reply)
	}
}

// TestAHighRiskMarkWithNoJudgeIsRefused covers a deployment without the
// moderation feature: the command says so rather than pretending a list exists
// that nothing enforces.
func TestAHighRiskMarkWithNoJudgeIsRefused(t *testing.T) {
	h := newHarness(t, baseSection)
	h.send("/高风险 add <@!MEMBER-OPENID> 7d", testAdmin, testGroupOpenID)
	if reply := h.lastReply(); !strings.Contains(reply, "不可用") {
		t.Errorf("reply = %q, want it to say the list is unavailable", reply)
	}
}
