package command

import (
	"regexp"
	"strings"
)

// Parsed is one recognised command.
type Parsed struct {
	// Name is the command word, as written.
	Name string
	// Args are the words after it.
	Args []string
	// Tail is the message with the leading mention of the bot removed.
	//
	// A target is searched for in here rather than in the raw content, because a
	// message that starts by mentioning the bot would otherwise offer the bot
	// itself as the first candidate.
	Tail string
	// BotOpenID is the member the message opened by mentioning, which is the bot
	// itself. It has to be named explicitly because a full receive event lists
	// every mention, the bot included, whatever the documentation says.
	BotOpenID string
}

// mentionMarkup matches the mention the platform leaves in the text when the bot
// runs in full receive mode.
var mentionMarkup = regexp.MustCompile(`^\s*<@!?([0-9A-Za-z_-]+)>\s*`)

// Parse reads a command out of a message.
//
// It reports false for a message that is not a command at all, which is the
// ordinary case: a bot that reads every message in a group sees far more that is
// not addressed to it than that is.
func Parse(content, prefix string) (Parsed, bool) {
	botOpenID := ""
	if match := mentionMarkup.FindStringSubmatch(content); match != nil {
		botOpenID = match[1]
	}
	text := mentionMarkup.ReplaceAllString(content, "")
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, prefix) {
		return Parsed{}, false
	}
	words := strings.Fields(strings.TrimPrefix(text, prefix))
	if len(words) == 0 {
		return Parsed{}, false
	}
	return Parsed{
		Name:      words[0],
		Args:      words[1:],
		Tail:      text,
		BotOpenID: botOpenID,
	}, true
}

// mentionInText matches a mention left in the message text as markup.
var mentionInText = regexp.MustCompile(`<@!?([0-9A-Za-z_-]{8,})>`)

// FirstMention returns the first member the text mentions, if it mentions one.
//
// It reads a mention of the bot the same way it reads any other: which mention
// is the bot's is not something a message says, so a caller looking for a target
// has to leave that one out itself.
func FirstMention(text string) (string, bool) {
	match := mentionInText.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	return match[1], true
}
