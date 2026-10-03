package broadcast

import (
	"strconv"
	"strings"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/disclaimer"
)

// TestTheCardOpensWithNothingChosen covers the state a card starts in: no option
// has been decided and no group has been picked, so nothing can be sent by accident.
func TestTheCardOpensWithNothingChosen(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)

	card := p.lastText()
	for _, want := range []string{"Markdown 渲染：未选", "匿名发送：未选", "发送到：还没选"} {
		if !strings.Contains(card, want) {
			t.Errorf("the card does not say %q:\n%s", want, card)
		}
	}
	buttons := p.cardButtons()
	for _, label := range []string{"MD：未选", "匿名：未选", "群-HERE", "群-OTHER", "群-THIRD"} {
		if _, ok := buttons[label]; !ok {
			t.Errorf("the card has no %q button", label)
		}
	}
}

// TestNothingContinuesUntilEveryChoiceIsMade covers the rule the card is built
// around: an administrator says what they want, and a default nobody looked at is not
// an answer.
func TestNothingContinuesUntilEveryChoiceIsMade(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)

	p.press(t, "确认", theAdmin)
	if said := p.lastText(); !strings.Contains(said, "还要选：") ||
		!strings.Contains(said, "Markdown 渲染") || !strings.Contains(said, "至少一个群") {
		t.Errorf("pressing 确认 with nothing chosen said:\n%s", said)
	}
	if last := p.answered[len(p.answered)-1]; last != qqbotsdk.InteractionCodeFailed {
		t.Errorf("a card that is not ready answered with %v, want failed", last)
	}

	// Both switches and one group: now it goes on to the summary.
	p.choose(t, true, true)
	p.press(t, "确认", theAdmin)
	if summary := p.lastText(); !strings.Contains(summary, "**广播参数**") {
		t.Errorf("the card did not go on to the summary:\n%s", summary)
	}
}

// TestTheRichTextButtonIsRefusedAndIsNotRequired covers the option the platform has no
// message for: it is shown so that an administrator knows, refused when pressed, and
// left out of what has to be chosen -- a card that waited for it would never finish.
func TestTheRichTextButtonIsRefusedAndIsNotRequired(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)

	p.press(t, "富文本✗", theAdmin)
	if said := p.lastText(); !strings.Contains(said, "富文本") {
		t.Errorf("pressing the rich-text button did not say why:\n%s", said)
	}
	if last := p.answered[len(p.answered)-1]; last != qqbotsdk.InteractionCodeFailed {
		t.Errorf("the rich-text button answered %v, want failed", last)
	}

	p.choose(t, true, true)
	p.press(t, "确认", theAdmin)
	if summary := p.lastText(); !strings.Contains(summary, "**广播参数**") {
		t.Errorf("the flow did not get past the rich-text option:\n%s", summary)
	}
}

// TestAGroupIsChosenAndChosenAgain covers the multi-select: one press picks a group,
// the next one lets it go.
func TestAGroupIsChosenAndChosenAgain(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)

	p.press(t, "群-OTHER", theAdmin)
	if card := p.lastText(); !strings.Contains(card, "发送到：群-OTHER") {
		t.Errorf("the group was not chosen:\n%s", card)
	}
	if _, ok := p.cardButtons()["✓群-OTHER"]; !ok {
		t.Error("the chosen group is not shown as chosen")
	}

	p.press(t, "✓群-OTHER", theAdmin)
	if card := p.lastText(); !strings.Contains(card, "发送到：还没选") {
		t.Errorf("the group was not let go again:\n%s", card)
	}
}

// TestTheBroadcastIsPostedAsWritten covers the whole flow, and what a group reads at
// the end of it: the header, the divider, and the text as the preview showed it.
func TestTheBroadcastIsPostedAsWritten(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)
	p.choose(t, true, true)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, "第一行\n第二行")

	preview := p.lastText()
	if !strings.Contains(preview, "**预览**") {
		t.Fatalf("the text did not become a preview:\n%s", preview)
	}
	if !strings.Contains(preview, "第一行\n第二行") {
		t.Errorf("the preview does not show what was written:\n%s", preview)
	}

	p.press(t, "发送", theAdmin)

	posted := p.notices()
	if len(posted) != 1 {
		t.Fatalf("the broadcast was posted %d time(s), want once: %v", len(posted), posted)
	}
	// Rendered as markdown, so the header is bold, a blank line separates it from the rule,
	// the rule is the one the platform draws, and the disclaimer sits under one of its own
	// at the end: the body is the writer's text, carried under the bot's name.
	want := "**来自管理员的广播**\n\n" + markdownRule + "\n第一行\n第二行\n\n" +
		markdownRule + "\n**" + disclaimer.Text + "**"
	if posted[0] != want {
		t.Errorf("the group reads:\n%q\nwant:\n%q", posted[0], want)
	}
	if len(p.recalls) == 0 {
		t.Error("the card and the preview were left behind in the group")
	}
}

// TestTheDividerFollowsTheRendering covers what separates the two dividers: the rule the
// platform draws only is a rule when markdown is rendered, and the same message taken
// literally would show it as three dashes.
func TestTheDividerFollowsTheRendering(t *testing.T) {
	rendered := newPlatform(t)
	rendered.start(t, theAdmin)
	rendered.choose(t, true, true)
	rendered.press(t, "确认", theAdmin)
	rendered.press(t, "继续", theAdmin)
	rendered.say(t, theAdmin, "正文")

	// The blank line is the point, not the spacing: a rule on the line straight under a
	// line of text is a setext heading underline in markdown, which made the header a big
	// title and drew no divider at all in a group.
	if preview := rendered.lastText(); !strings.Contains(preview, "\n\n"+markdownRule+"\n") {
		t.Errorf("a rendered notice does not separate its header from the rule:\n%s", preview)
	}

	literal := newPlatform(t)
	literal.start(t, theAdmin)
	literal.choose(t, false, true)
	literal.press(t, "确认", theAdmin)
	literal.press(t, "继续", theAdmin)
	literal.say(t, theAdmin, "正文")

	preview := literal.lastText()
	if strings.Contains(preview, "\n"+markdownRule+"\n") {
		t.Errorf("a notice that is not rendered uses the markdown rule:\n%s", preview)
	}
	if !strings.Contains(preview, "\n"+plainDivider+"\n") {
		t.Errorf("a notice that is not rendered has no divider at all:\n%s", preview)
	}
}

// TestTheHeaderSaysWhoAsked covers the difference the anonymous switch makes.
func TestTheHeaderSaysWhoAsked(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)
	p.choose(t, true, false)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, "大家好")

	p.press(t, "发送", theAdmin)
	posted := p.notices()
	if len(posted) != 1 {
		t.Fatalf("the broadcast was posted %d time(s), want once: %v", len(posted), posted)
	}
	if !strings.Contains(posted[0], "来自 <@"+theAdmin+"> 的广播") {
		t.Errorf("with the switch off the group should be told who asked:\n%s", posted[0])
	}
}

// TestMarkdownOffEscapesWhatWasWritten covers what turning markdown rendering off has
// to mean: the text is what the writer wrote, not what markdown makes of it.
func TestMarkdownOffEscapesWhatWasWritten(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)
	p.choose(t, false, true)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, "*不是加粗*")

	p.press(t, "发送", theAdmin)
	posted := p.notices()
	if len(posted) != 1 {
		t.Fatalf("the broadcast was posted %d time(s), want once: %v", len(posted), posted)
	}
	if !strings.Contains(posted[0], `\*不是加粗\*`) {
		t.Errorf("the asterisks were left to mean something: %q", posted[0])
	}
	// And the header is plain there: a message nobody renders would show the asterisks of
	// a bold marker as asterisks.
	if !strings.HasPrefix(posted[0], "来自管理员的广播\n") {
		t.Errorf("a notice that is not rendered does not have a plain header:\n%s", posted[0])
	}
}

// TestTheNoticeSaysTheBotDidNotWriteIt covers the disclaimer.
//
// What the group reads is the writer's text carried under this bot's name, so the message
// has to say which of the two wrote it -- in both kinds of message, and in the preview too,
// because the preview is what the writer approves.
func TestTheNoticeSaysTheBotDidNotWriteIt(t *testing.T) {
	for _, rendered := range []bool{true, false} {
		p := newPlatform(t)
		p.start(t, theAdmin)
		p.choose(t, rendered, true)
		p.press(t, "确认", theAdmin)
		p.press(t, "继续", theAdmin)
		p.say(t, theAdmin, "正文")

		divider := plainDivider
		if rendered {
			divider = markdownRule
		}
		if preview := p.lastText(); !strings.HasSuffix(preview,
			divider+"\n**"+disclaimer.Text+"**") {
			t.Errorf("the preview of a notice rendered=%v does not end with the "+
				"disclaimer:\n%s", rendered, preview)
		}

		p.press(t, "发送", theAdmin)
		posted := p.notices()
		if len(posted) != 1 {
			t.Fatalf("the broadcast was posted %d time(s), want once: %v", len(posted), posted)
		}
		if !strings.HasSuffix(posted[0], divider+"\n**"+disclaimer.Text+"**") {
			t.Errorf("the notice a group reads rendered=%v does not end with the "+
				"disclaimer:\n%s", rendered, posted[0])
		}
	}
}

// TestACommandIsNotSwallowedAsText covers what the table is asked for: a command typed
// while a broadcast is being written has to be answered, not taken as its text.
func TestACommandIsNotSwallowedAsText(t *testing.T) {
	p := newPlatform(t)
	p.handler.SetCommands(commands{looks: true})
	p.start(t, theAdmin)
	p.choose(t, true, true)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)

	p.say(t, theAdmin, "/群广播")
	if preview := p.lastText(); strings.Contains(preview, "**预览**") {
		t.Error("a command was taken as the text of a broadcast")
	}

	// And with the table saying it is not a command, the same message is the text.
	p.handler.SetCommands(commands{looks: false})
	p.say(t, theAdmin, "这次是正文")
	if preview := p.lastText(); !strings.Contains(preview, "**预览**") {
		t.Error("what was written was not taken as the text")
	}
}

// TestAGroupThatCannotBeSpokenToIsSkipped covers the one thing a broadcast cannot do
// without the platform: a group that only accepts answers cannot receive a message the
// bot starts, and the administrator is told rather than left guessing.
func TestAGroupThatCannotBeSpokenToIsSkipped(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)
	p.choose(t, true, true)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, "大家好")

	p.proactive = false
	before := len(p.sent)
	p.press(t, "发送", theAdmin)

	for _, message := range p.sent[before:] {
		markdown, _ := message["markdown"].(map[string]any)
		if text, _ := markdown["content"].(string); strings.Contains(text, "大家好") {
			t.Errorf("the broadcast was posted to a group that does not accept it: %s", text)
		}
	}
	if said := p.lastText(); !strings.Contains(said, "没有发出") {
		t.Errorf("the administrator was not told the broadcast did not go out:\n%s", said)
	}
}

// TestTheCardCapsWhatAButtonCanHold covers the shortening, which is the difference
// between a card that is sent and one the platform refuses.
func TestTheCardCapsWhatAButtonCanHold(t *testing.T) {
	if got := fitLabel("短"); got != "短" {
		t.Errorf("a label that fits was shortened to %q", got)
	}
	if got := fitLabel("一二三四五六"); got != "一二三四五" {
		t.Errorf("fitLabel = %q, want five characters, which is what ten counts as", got)
	}
	if got := fitLabel("abcdefghijkl"); got != "abcdefghij" {
		t.Errorf("fitLabel = %q, want ten characters", got)
	}
}

// TestTheAnswerCarriesWhatWasPressed covers the reading of a button's own data, which
// is the only thing that says which card and which action a press is about.
func TestTheAnswerCarriesWhatWasPressed(t *testing.T) {
	asked, ok := readAction("token:group:GROUP-OTHER")
	if !ok {
		t.Fatal("a well-formed payload was refused")
	}
	if asked.token != "token" || asked.kind != kindGroup || asked.group != "GROUP-OTHER" {
		t.Errorf("read %+v, want the token, the kind and the group", asked)
	}
	if _, ok := readAction("token"); ok {
		t.Error("a payload with no action was accepted")
	}
}

// itoa is the number of a sent message, for the stand-in platform's ids.
func itoa(number int) string { return strconv.Itoa(number) }
