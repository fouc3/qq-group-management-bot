package admincmd

import (
	"context"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/messaging"
)

// Sending goes through internal/messaging, which is where the retry, the pause
// between attempts and the rule that buttons ride on a markdown message live.
//
// What stays here is this feature's vocabulary: which destination an answer goes
// to, and what making it a reply means. That is worth four one-line wrappers --
// every command answers a message, and saying so at each call site would spread
// one idea over a dozen commands.

// sendMessage puts one message into one group, as an answer to a command.
func (h *handler) sendMessage(ctx context.Context, groupOpenID, text, replyTo string) error {
	_, err := h.sendMessageWithKeyboard(ctx, groupOpenID, text, replyTo, nil)
	return err
}

// sendMessageWithKeyboard is sendMessage with buttons under the message.
func (h *handler) sendMessageWithKeyboard(ctx context.Context, groupOpenID, text,
	replyTo string, keyboard *qqbotsdk.Keyboard) (*qqbotsdk.MessageResponse, error) {
	return messaging.Send(ctx, h.deps.Client, messaging.Message{
		GroupOpenID: groupOpenID,
		Text:        text,
		Keyboard:    keyboard,
		ReplyTo:     replyTo,
	})
}

// sendPrivateMessage is the same thing into a single chat.
//
// A single chat is not a group without a group id: it is a second destination
// with its own endpoint, and it is the only way an administrator can ask about a
// receipt without the group reading the answer.
func (h *handler) sendPrivateMessage(ctx context.Context, userOpenID, text, replyTo string) error {
	_, err := h.sendPrivateMessageWithKeyboard(ctx, userOpenID, text, replyTo, nil)
	return err
}

// sendPrivateMessageWithKeyboard is the same with buttons under the message.
func (h *handler) sendPrivateMessageWithKeyboard(ctx context.Context, userOpenID, text,
	replyTo string, keyboard *qqbotsdk.Keyboard) (*qqbotsdk.MessageResponse, error) {
	return messaging.Send(ctx, h.deps.Client, messaging.Message{
		UserOpenID: userOpenID,
		Text:       text,
		Keyboard:   keyboard,
		ReplyTo:    replyTo,
	})
}
