package broadcast

import (
	"strconv"
	"strings"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
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

	before := len(p.sent)
	p.press(t, "发送", theAdmin)

	// What was posted, told from what was said about it: the answer the administrator
	// reads afterwards is a message as well.
	var posted []string
	for _, message := range p.sent[before:] {
		markdown, _ := message["markdown"].(map[string]any)
		if text, _ := markdown["content"].(string); strings.Contains(text, divider) {
			posted = append(posted, text)
		}
	}
	if len(posted) != 1 {
		t.Fatalf("the broadcast was posted %d time(s), want once: %v", len(posted), posted)
	}
	want := "来自管理员的广播\n" + divider + "\n第一行\n第二行"
	if posted[0] != want {
		t.Errorf("the group reads:\n%q\nwant:\n%q", posted[0], want)
	}
	if len(p.recalls) == 0 {
		t.Error("the card and the preview were left behind in the group")
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

	before := len(p.sent)
	p.press(t, "发送", theAdmin)
	markdown, _ := p.sent[before]["markdown"].(map[string]any)
	sent, _ := markdown["content"].(string)
	if !strings.HasPrefix(sent, "来自 <@"+theAdmin+"> 的广播") {
		t.Errorf("with the switch off the group should be told who asked:\n%s", sent)
	}
}

// TestMarkdownOffEscapesWhatWasWritten covers what turning markdown rendering off has
// to mean: the text is what the administrator wrote, not what markdown makes of it.
func TestMarkdownOffEscapesWhatWasWritten(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)
	p.choose(t, false, true)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, "*不是加粗*")

	before := len(p.sent)
	p.press(t, "发送", theAdmin)
	markdown, _ := p.sent[before]["markdown"].(map[string]any)
	sent, _ := markdown["content"].(string)
	if !strings.Contains(sent, `\*不是加粗\*`) {
		t.Errorf("the asterisks were left to mean something: %q", sent)
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
