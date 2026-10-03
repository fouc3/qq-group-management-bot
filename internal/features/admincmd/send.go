package admincmd

import (
	"context"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// Sending is where every message this feature puts into a group goes through.
//
// One place, for two reasons. The transport is not a command's business: the
// official bot is the normal way out, and a caller should not have to know that
// -- nor be able to pick something else by accident. And a send fails for reasons
// that pass, so the retry and its back-off belong here, in one place that can be
// reasoned about, rather than in every caller or, worse, nowhere.
//
// OneBot is deliberately not the path here. It is the fallback for the operations
// the official bot is not allowed to perform -- kicking, reading the member list
// -- and sending a message is not one of them. A message sent through the
// protocol side is also a different identity, which is not what a group should
// see answering its own member.
const (
	// sendAttempts is how many times one message is tried before it is given up
	// on. Three is enough to ride out a blip, and short enough that a genuinely
	// broken send does not hold a goroutine for long.
	sendAttempts = 3
	// sendBackoff is the pause after the first failure, doubled for each attempt
	// after it. The waiting is the point: these failures are a moment of
	// unreadiness rather than a refusal, and knocking again immediately makes them
	// last longer.
	sendBackoff = 500 * time.Millisecond
)

// sendMessage puts one message into one group, retrying a failure with a pause
// that grows.
//
// replyTo, when it is not empty, makes the message an answer to that one: the
// platform only lets a bot reply to an event within its own message, and a
// command's answer has to be that.
func (h *handler) sendMessage(ctx context.Context, groupOpenID, text, replyTo string) error {
	_, err := h.sendMessageWithKeyboard(ctx, groupOpenID, text, replyTo, nil)
	return err
}

// sendMessageWithKeyboard is sendMessage with buttons under the message.
//
// The response is returned because a keyboard can offer to take its own message
// back, and that needs the id the platform gave the message. A keyboard hangs off
// a markdown message -- the platform drops buttons on a plain text one without
// saying so, and the SDK refuses that combination before the request is made --
// which every message this feature sends already is.
func (h *handler) sendMessageWithKeyboard(ctx context.Context, groupOpenID, text,
	replyTo string, keyboard *qqbotsdk.Keyboard) (*qqbotsdk.MessageResponse, error) {
	return h.sendWithRetry(ctx, text, replyTo, keyboard,
		func(message *qqbotsdk.Message) (*qqbotsdk.MessageResponse, error) {
			return h.deps.Client.SendGroupMessage(ctx, groupOpenID, message)
		})
}

// sendPrivateMessage is the same thing into a single chat.
//
// A single chat is not a group without a group id: it is a second destination
// with its own endpoint, and it is the only way an administrator can ask about a
// receipt without the group reading the answer. It goes through the same retry
// as everything else, because the reasons a send fails are the transport's and
// have nothing to do with where the message is going.
func (h *handler) sendPrivateMessage(ctx context.Context, userOpenID, text, replyTo string) error {
	_, err := h.sendPrivateMessageWithKeyboard(ctx, userOpenID, text, replyTo, nil)
	return err
}

// sendPrivateMessageWithKeyboard is the same with buttons under the message.
func (h *handler) sendPrivateMessageWithKeyboard(ctx context.Context, userOpenID, text,
	replyTo string, keyboard *qqbotsdk.Keyboard) (*qqbotsdk.MessageResponse, error) {
	return h.sendWithRetry(ctx, text, replyTo, keyboard,
		func(message *qqbotsdk.Message) (*qqbotsdk.MessageResponse, error) {
			return h.deps.Client.SendC2CMessage(ctx, userOpenID, message)
		})
}

// sendWithRetry is the retry and the back-off, in one place.
//
// Both destinations share it rather than each carrying their own copy: a second
// loop would be a second set of numbers to keep in step, and the one that was
// not being looked at would be the one that drifted.
func (h *handler) sendWithRetry(ctx context.Context, text, replyTo string,
	keyboard *qqbotsdk.Keyboard,
	deliver func(*qqbotsdk.Message) (*qqbotsdk.MessageResponse, error)) (
	*qqbotsdk.MessageResponse, error) {
	message := &qqbotsdk.Message{
		MsgType:  qqbotsdk.MsgTypeMarkdown,
		Markdown: &qqbotsdk.MessageMarkdown{Content: text},
		Keyboard: keyboard,
	}
	if replyTo != "" {
		message.MsgID = replyTo
		message.MsgSeq = 1
	}

	wait := sendBackoff
	var failure error
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		var response *qqbotsdk.MessageResponse
		if response, failure = deliver(message); failure == nil {
			return response, nil
		}

		// The last attempt has nothing left to wait for, and a cancelled context
		// means the bot is shutting down: neither is worth another pause.
		if attempt == sendAttempts || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
	return nil, failure
}
