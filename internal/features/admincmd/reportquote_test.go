package admincmd

import (
	"context"
	"strings"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// The platform names the message a report is about in two places, and a quote of a
// message that is itself a quote is the case that tells them apart: the scene falls
// back to a temporary index the cache can never hold, while the element that carried
// the quoted content may still name the message properly. What these cover is the
// hand-over: both names travel, the sender travels when the platform gives one, and a
// report that names nothing usable is answered in its own words rather than with the
// answer for a judgement that failed.

// quotedNestedText is the quoted rendering of a message that is itself a quote, taken
// from a real report: the first [消息内容] is the reported message's own words, and
// everything under the 关联消息 marker is what it quoted.
const quotedNestedText = "=== 消息 1 ===\n" +
	"[消息内容]  4.1 模型自带破甲系统提示词\n" +
	"[消息类型] 引用消息\n" +
	"[关联消息]\n" +
	"--- 第1条 ---\n" +
	"    [消息内容] 意见箱：\n" +
	"    [消息类型] 引用消息\n"

// quotingReport builds the report a member sends by quoting a message, in the shape
// the platform uses: the scene names the quoted message in one place and the element
// carrying its content names it in another.
func quotingReport(sceneIndex, elementIndex, text, author string) *qqbotsdk.GroupMessageCreateData {
	data := &qqbotsdk.GroupMessageCreateData{
		ID:          "REPORT-MESSAGE",
		GroupOpenID: testGroupOpenID,
		Content:     "/违规举报",
		Author:      &qqbotsdk.User{MemberOpenID: testAdmin},
	}
	if sceneIndex != "" {
		data.MessageScene = &qqbotsdk.MessageScene{
			Ext: []string{"msg_idx=REPORT-MESSAGE", "ref_msg_idx=" + sceneIndex},
		}
	}
	element := qqbotelement(text, elementIndex, author)
	if element != nil {
		data.MsgElements = []qqbotsdk.MsgElement{*element}
	}
	return data
}

// qqbotelement is one quoted-content element, or nil when there is nothing to carry.
func qqbotelement(text, index, author string) *qqbotsdk.MsgElement {
	if text == "" && index == "" && author == "" {
		return nil
	}
	element := &qqbotsdk.MsgElement{Content: text, MsgIdx: index}
	if author != "" {
		element.Author = &qqbotsdk.User{MemberOpenID: author}
	}
	return element
}

// TestTheQuotedMessageIsNamedTwice covers what the command hands over: both names the
// platform gave, and the sender when it named one.
//
// The element's name is the point of the change. A quote of a message that is itself a
// quote comes with a temporary index in the scene, which the cache can never hold; if
// the element carries the message's real index, the judgement becomes an exact lookup
// instead of a search -- and a search is the thing that cannot say *whose* message it
// found.
func TestTheQuotedMessageIsNamedTwice(t *testing.T) {
	judge := &stubJudge{
		verdict: feature.ModerationVerdict{Category: "ad", Label: "广告"},
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	h := reportHarness(t, judge)
	if err := h.handler.reportCommand(context.Background(), quotingReport(
		"TMP_beaff358-d6dd-4821-8e0c-24b47241c1d1",
		"REFIDX_zrXK+theRealOne", quotedNestedText, "MEMBER-1"),
		command.Parsed{}); err != nil {
		t.Fatalf("reporting: %v", err)
	}

	<-judge.entered
	if judge.lastQuoted != "TMP_beaff358-d6dd-4821-8e0c-24b47241c1d1" {
		t.Errorf("scene index = %q, want the temporary one the platform put there",
			judge.lastQuoted)
	}
	if judge.lastQuotedElement != "REFIDX_zrXK+theRealOne" {
		t.Errorf("element index = %q, want the other name the platform gave",
			judge.lastQuotedElement)
	}
	if judge.lastQuotedAuthor != "MEMBER-1" {
		t.Errorf("author = %q, want the sender the platform named",
			judge.lastQuotedAuthor)
	}
	if len(judge.quotedText) == 0 {
		t.Error("the quoted text did not travel, so a search would have nothing to go on")
	}
	close(judge.release)
}

// TestAReportAboutAnUnnamedQuoteIsAnsweredPlainly covers the answer when nothing the
// platform gave identifies the message.
//
// It is not the answer for a failed judgement: the report was fine, and reporting the
// same message again the same way would fail the same way. So the group is told what
// actually happened, with the receipt so an administrator can look at what the
// platform sent.
func TestAReportAboutAnUnnamedQuoteIsAnsweredPlainly(t *testing.T) {
	judge := &stubJudge{err: feature.ErrUnidentifiedQuote}
	h := reportHarness(t, judge)
	if err := h.handler.reportCommand(context.Background(), quotingReport(
		"TMP_beaff358-d6dd-4821-8e0c-24b47241c1d1", "", quotedNestedText, ""),
		command.Parsed{}); err != nil {
		t.Fatalf("reporting: %v", err)
	}

	reply := waitForReply(t, h, "拿不到可用的消息编号")
	if strings.Contains(reply, "送检失败") {
		t.Errorf("reply = %q, want the answer for an unnamed quote rather than the "+
			"one for a judgement that failed", reply)
	}
	if !strings.Contains(reply, "普通消息") {
		t.Errorf("reply = %q, want it to say what to do instead", reply)
	}
}

// TestAReportWithoutAnyNameAsksForAQuote covers the half the element cannot rescue:
// with no name anywhere there is nothing to look up, and the command says so instead
// of asking the moderation feature to guess at a message.
func TestAReportWithoutAnyNameAsksForAQuote(t *testing.T) {
	judge := &stubJudge{}
	h := reportHarness(t, judge)
	if err := h.handler.reportCommand(context.Background(),
		quotingReport("", "", "", ""), command.Parsed{}); err != nil {
		t.Fatalf("reporting: %v", err)
	}

	if judge.judged != 0 {
		t.Errorf("the judgement was reached %d time(s) with no name at all",
			judge.judged)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "引用") {
		t.Errorf("reply = %q, want it to ask for a quote", reply)
	}
}
