// Package disclaimer marks a message whose content the bot did not write.
//
// Two kinds of message here are mostly other people's words. A broadcast is written by an
// administrator, and a receipt's details quote messages that were taken back -- which
// nobody can read anywhere else afterwards, not the group, not an administrator, not this
// bot, so the copy in the receipt is the only one left. Neither is a statement this bot
// makes, and a reader who takes one for the bot's own words is reading a stranger's text
// as the bot's. This is what says so, in one sentence, wherever it appears.
package disclaimer

import "strings"

// Text is the sentence itself, kept whole.
//
// One wording, used as it is everywhere: a disclaimer that is reworded per message is one
// nobody recognises the second time they read it, and recognising it is the whole of what
// it does.
const Text = "免责声明：以上部分字段中的文字并非本bot产生，本bot仅从数据库读取、照搬或复述他人已有内容，" +
	"不代表本bot观点，亦不构成任何建议或承诺，相关责任由内容来源方承担。"

// After is this message with the disclaimer at the end of it, under a divider.
//
// The divider is passed in rather than chosen here: what a divider is depends on how a
// message is rendered, and the feature that builds the message is the one that knows. It is
// the rule the platform draws for a message it renders as markdown, and a run of characters
// for one that is taken literally.
//
// The blank line above the divider is not spacing. A rule on the line straight under a line
// of text is a setext heading underline in markdown: it turns that line into a title and
// draws no rule at all, which is what happened when the broadcast first used one. An empty
// line costs nothing in a message that is not rendered.
//
// The sentence goes in bold, which works in both kinds of message: the platform renders
// every message this bot sends, and only the writer's own text is ever escaped by the
// feature that builds it -- so a marker added here is always a marker, never two asterisks.
func After(text, divider string) string {
	return strings.TrimRight(text, "\n") + "\n\n" + divider + "\n**" + Text + "**"
}
