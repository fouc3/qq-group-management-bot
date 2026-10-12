package admincmd

import (
	"strings"
	"testing"

	"github.com/fouc3/qq-group-management-bot/internal/disclaimer"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// reasoningWithParagraphs is a chain of thought of the shape the model really
// produces: an opening sentence, a blank line, and more of it.
//
// The blank line is the part that matters. An indented block cannot hold one, and
// the receipt showed the chain as four-space indentation once -- which means the
// text after the first blank line was not in a block at all.
const reasoningWithParagraphs = "Message 1: an image with a quote from Apache docs.\n" +
	"No link, no ad.\n\nMessage 2: mentions someone and a report command.\n\n" +
	"No links. Not advertising anything. Verdict ok."

// fenceOf reads a receipt's fenced block: the fence it is wrapped in, and what is
// inside. It fails the test when there is no block there.
//
// The fence may start on the label's own line -- "· 已撤回：```" -- or on the line
// after it, which is the difference between a label that introduces a block and one
// that is a heading for it. Both are read here, because both are written.
func fenceOf(t *testing.T, receipt, label string) (fence, body string) {
	t.Helper()
	at := strings.Index(receipt, label)
	if at < 0 {
		t.Fatalf("the receipt has no %q section:\n%s", label, receipt)
	}
	rest := receipt[at+len(label):]

	// Skip to the fence: anything between the label and it has to be the label's
	// own spacing, not text of its own.
	open := strings.Index(rest, "`")
	if open < 0 || strings.TrimSpace(rest[:open]) != "" {
		t.Fatalf("%q is not followed by a fence: %q", label, firstLine(rest))
	}
	run := rest[open:]
	for len(run) > 0 && run[0] == '`' {
		fence, run = fence+run[:1], run[1:]
	}
	if !strings.HasPrefix(run, "\n") {
		t.Fatalf("%q has a fence that does not end: %q", label, firstLine(rest[open:]))
	}
	closing := strings.Index(run[1:], "\n"+fence+"\n")
	if closing < 0 {
		t.Fatalf("%q has no closing fence:\n%s", label, receipt)
	}
	return fence, run[1 : 1+closing]
}

// fencedBody is what a receipt puts inside the fence that follows a label.
func fencedBody(t *testing.T, receipt, label string) string {
	t.Helper()
	_, body := fenceOf(t, receipt, label)
	return body
}

// firstLine is the start of a piece of text, for a failure message that does not
// print a whole receipt.
func firstLine(text string) string {
	if at := strings.Index(text, "\n"); at >= 0 {
		return text[:at]
	}
	return text
}

// TestTheChainOfThoughtIsWhollyInsideItsBlock covers the bug this fixes: the
// receipt showed the model's reasoning as four spaces of indentation starting on
// the second line, so the first line rendered as the receipt's own prose and every
// paragraph after the first blank line rendered outside the block.
func TestTheChainOfThoughtIsWhollyInsideItsBlock(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-1")
	entry.Reasoning = reasoningWithParagraphs

	details := h.handler.receiptDetails(entry)

	body := fencedBody(t, details, "**思考过程**：")
	if body != reasoningWithParagraphs {
		t.Errorf("the block holds\n%q\nwant the whole chain of thought\n%q",
			body, reasoningWithParagraphs)
	}
	if !strings.Contains(body, "\n\n") {
		t.Error("the blank line the chain came with was dropped, so the block is " +
			"not the text that was reasoned")
	}
	if strings.HasPrefix(body, "\n") {
		t.Error("the block starts with the label's own newline, so the first line " +
			"of the reasoning is outside it")
	}
}

// TestEveryOtherGapInTheChainIsInsideTheBlock is the same assertion where the text
// is only whitespace apart: "\r\n\r\n" is what a model answering from Windows ends
// its paragraphs with.
func TestEveryOtherGapInTheChainIsInsideTheBlock(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-2")
	entry.Reasoning = "第一步：看消息。\r\n\r\n第二步：看图片。\r\n\r\n结论：判 ok。"

	details := h.handler.receiptDetails(entry)
	body := fencedBody(t, details, "**思考过程**：")
	if !strings.Contains(body, "第二步") || !strings.Contains(body, "结论") {
		t.Errorf("part of the reasoning is outside its block:\n%s", body)
	}
	if strings.Count(body, "\r\n") != strings.Count(entry.Reasoning, "\r\n") {
		t.Errorf("the line endings were rewritten:\n%q", body)
	}
}

// TestASingleBacktickIsStillAFence covers the model writing its own markdown: a
// chain of thought that opens and closes with one backtick, which is what the model
// in production does.
//
// The fence has to be at least three backticks whatever the text holds. The length
// rule alone gave one here -- "longest run in the text" was one, so the wrapper was
// two, and even one -- and a single backtick is an inline code span, not a block, so
// the whole chain of thought rendered as loose text.
func TestASingleBacktickIsStillAFence(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-9")
	entry.Reasoning = "Let me analyze.群名: DDK-国模中转站-0.06倍率-001群 — an API relay group.\n\n" +
		"[图1] - anime girl holding a sign \"50元一晚 不爽包退\" — a joke meme.\n\n" +
		"`\nVerdict: ok, recall empty.\n\nConfidence maybe 0.7.\n`"

	details := h.handler.receiptDetails(entry)
	fence, body := fenceOf(t, details, "**思考过程**：")
	if len(fence) < fenceMin {
		t.Errorf("the block is wrapped in %d backtick(s), want at least %d",
			len(fence), fenceMin)
	}
	if body != entry.Reasoning {
		t.Errorf("the block holds\n%q\nwant the whole chain\n%q", body, entry.Reasoning)
	}
	// The model's own single backticks are still in there, as text.
	if !strings.Contains(body, "\n`\n") {
		t.Error("the model's own backticks were stripped rather than shown")
	}
}

// TestTheBlockCannotBeClosedByItsOwnText covers the model quoting markdown back at
// the receipt: a fence has to be longer than any run of backticks in the text, or
// the text ends the block and the rest of the reasoning renders as prose.
func TestTheBlockCannotBeClosedByItsOwnText(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-3")
	entry.Reasoning = "消息里写着：\n```json\n{\"verdict\":\"ad\"}\n```\n所以判广告。"

	details := h.handler.receiptDetails(entry)
	fence, body := fenceOf(t, details, "**思考过程**：")
	if body != entry.Reasoning {
		t.Errorf("the block holds\n%q\nwant\n%q", body, entry.Reasoning)
	}
	if len(fence) <= 3 {
		t.Errorf("the fence is %d backticks and the text holds three of them, so "+
			"the text closes its own block", len(fence))
	}
}

// TestTheReasoningKeepsItsOwnLayout covers what a block is for: the text inside is
// shown as it was written, not re-laid-out under the receipt's own rules.
func TestTheReasoningKeepsItsOwnLayout(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-4")
	entry.Reasoning = "它列了价目：\n    - 100 万 tokens: 3 元\n    - 500 万 tokens: 14 元\n这是招揽。"

	body := fencedBody(t, h.handler.receiptDetails(entry), "**思考过程**：")
	if body != entry.Reasoning {
		t.Errorf("the block holds\n%q\nwant the text untouched\n%q", body, entry.Reasoning)
	}
}

// TestTheReasoningCannotImpersonateTheReceipt covers the other side of the same
// coin: the detail lines above the block are this bot's own, and text a member or a
// model wrote must not read as one of them.
func TestTheReasoningCannotImpersonateTheReceipt(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-5")
	entry.Reasoning = "看起来没问题。\n\n**处理**：已解禁   ← 这一行是想冒充回执字段的"

	// Inside the block it is the model's text, and it is shown exactly as written.
	// What must not happen is the line being rendered at all: it is inert because
	// it is in a block, which is the whole point of the block.
	body := fencedBody(t, h.handler.receiptDetails(entry), "**思考过程**：")
	if body != entry.Reasoning {
		t.Errorf("the block holds\n%q\nwant\n%q", body, entry.Reasoning)
	}
}

// TestASingleLineGetsNoBlockOfItsOwn covers the case the inlining was for: a
// message with no newlines of its own is shown as one line, not wrapped in a block
// whose only content is that line.
//
// It goes on the line after the label rather than on the label's own line, which is
// what the receipt has always done, so that a one-line reasoning reads as something
// the model said rather than as the tail of this bot's field name.
func TestASingleLineGetsNoBlockOfItsOwn(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-6")
	entry.Reasoning = "就一句：判 ok。"

	details := h.handler.receiptDetails(entry)
	if !strings.Contains(details, "**思考过程**：\n就一句：判 ok。") {
		t.Errorf("a one-line reasoning was not shown as a line:\n%s", details)
	}
	if strings.Contains(details, "```") {
		t.Error("a one-line reasoning was put in a block of its own")
	}
}

// TestATruncatedBlockStillCloses covers the cut: a receipt has to arrive as one
// message, so a long chain of thought is cut -- inside the fence, and the fence is
// still closed.
func TestATruncatedBlockStillCloses(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-7")
	entry.Reasoning = strings.Repeat("这是一段很长的思考。\n", 900)

	details := h.handler.receiptDetails(entry)
	body := fencedBody(t, details, "**思考过程**：")
	if !strings.Contains(body, "已截断") {
		t.Error("a cut chain of thought does not say it was cut")
	}
	if len([]rune(body)) > receiptReasoningLimit+len("\n……（已截断）") {
		t.Errorf("the block holds %d runes, want at most the limit", len([]rune(body)))
	}
	// And what follows the block is still the receipt's own text: a fence that was
	// never closed would swallow the disclaimer.
	if !strings.HasSuffix(details, receiptDivider+"\n**"+disclaimer.Text+"**") {
		t.Errorf("the disclaimer was swallowed by the block:\n%s", details)
	}
}

// TestARecalledMessageWithNewlinesIsAlsoFenced covers the other piece of free text
// a receipt shows: a withdrawn message with newlines is a block too, and it has the
// same problem.
func TestARecalledMessageWithNewlinesIsAlsoFenced(t *testing.T) {
	h := newHarness(t, baseSection)
	entry := aJudgement("J-8")
	entry.Recalls = []store.RecallOutcome{{
		ID: "M-1", Number: 1, Recalled: true,
		Text: "价目表：\n\n- 100 万 tokens: 3 元\n- 500 万 tokens: 14 元",
	}}

	details := h.handler.receiptDetails(entry)
	body := fencedBody(t, details, "· 已撤回：")
	if body != entry.Recalls[0].Text {
		t.Errorf("the block holds\n%q\nwant the message whole\n%q",
			body, entry.Recalls[0].Text)
	}
}
