package disclaimer

import (
	"strings"
	"testing"
)

// TestTheSentenceIsTheOneThatWasAgreed covers the wording itself.
//
// It is one fixed sentence used everywhere, so a change to it is a change to what the bot
// claims about every message it relays, and it should not be possible to make one by
// accident.
func TestTheSentenceIsTheOneThatWasAgreed(t *testing.T) {
	want := "免责声明：以上部分字段中的文字并非本bot产生，本bot仅从数据库读取、照搬或复述他人已有内容，" +
		"不代表本bot观点，亦不构成任何建议或承诺，相关责任由内容来源方承担。"
	if Text != want {
		t.Errorf("the sentence reads:\n%s\nwant:\n%s", Text, want)
	}
	// And it is one sentence, not two or three run together: what it does is read as one
	// claim rather than as terms and conditions.
	if count := strings.Count(Text, "免责声明"); count != 1 {
		t.Errorf("the sentence says 免责声明 %d time(s), want once", count)
	}
}

// TestTheSentenceGoesAtTheEndUnderADivider covers the shape every caller gets.
func TestTheSentenceGoesAtTheEndUnderADivider(t *testing.T) {
	divider := "────────"
	message := After("**违规回执 1 详细信息**\n"+divider+"\n· 已撤回：试试", divider)

	if !strings.HasSuffix(message, divider+"\n**"+Text+"**") {
		t.Errorf("the disclaimer is not the last thing in the message:\n%s", message)
	}
	if !strings.Contains(message, "· 已撤回：试试") {
		t.Errorf("the message it was added to is not there any more:\n%s", message)
	}
	// A blank line in front of the divider, which is what makes a markdown rule a rule
	// rather than a heading underline -- the mistake the broadcast made in a group.
	if !strings.Contains(message, "\n\n"+divider+"\n") {
		t.Errorf("the divider is not separated from the text above it:\n%s", message)
	}
	// And exactly one disclaimer, however the message arrived.
	if count := strings.Count(message, Text); count != 1 {
		t.Errorf("the disclaimer appears %d time(s), want once:\n%s", count, message)
	}
}

// TestNoTrailingNewlinesAreAdded covers a message that already ends in one: the disclaimer
// must not end up floating lines below the text it belongs to.
func TestNoTrailingNewlinesAreAdded(t *testing.T) {
	message := After("正文\n\n", "———")

	if strings.Contains(message, "正文\n\n\n") {
		t.Errorf("the message has a gap between it and its disclaimer:\n%q", message)
	}
	if !strings.HasSuffix(message, "\n\n———\n**"+Text+"**") {
		t.Errorf("the shape is not the one every caller gets:\n%q", message)
	}
}
