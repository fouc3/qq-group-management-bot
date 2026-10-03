// Package messaging is the one way a feature puts a message into a group or a
// single chat.
//
// One place, for two reasons.
//
// Every send needs the same retry and back-off: a send fails for reasons that
// pass, and a feature that retried on its own would be a second set of numbers to
// keep in step with the first.
//
// And the platform has a rule that is easy to miss: buttons ride on a markdown
// message, and on a plain one they are dropped with no error at all. Every
// message sent from here is markdown, whether or not its author was thinking
// about buttons.
package messaging

import (
	"context"
	"errors"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

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

// Message is one message and where it goes.
type Message struct {
	// GroupOpenID is the group to send into, and UserOpenID is the single chat.
	// Exactly one of them is given.
	GroupOpenID string
	UserOpenID  string

	// Text is the body.
	Text string

	// Keyboard is the buttons under the message, or nil.
	Keyboard *qqbotsdk.Keyboard

	// ReplyTo makes the message a passive reply to that message, which is the
	// only kind of answer the platform allows to an event.
	//
	// Empty sends it as a message of its own instead. That needs no event to
	// answer, and is how a bot speaks without being asked -- which the platform
	// limits, per group, by its own rules rather than this one's.
	ReplyTo string

	// Sequence numbers the replies to one message, and the first one is 1.
	//
	// The platform treats replies that share both a message and a sequence as
	// duplicates of each other, so a bot that answers one message twice has to
	// count. Leaving it zero means the first reply.
	Sequence int
}

// Send puts one message into one destination, retrying a failure with a pause
// that grows.
//
// The response is returned because a keyboard can offer to take its own message
// back, and that needs the id the platform gave the message.
func Send(ctx context.Context, client *qqbotsdk.Client, message Message) (*qqbotsdk.MessageResponse, error) {
	if client == nil {
		return nil, errors.New("messaging: no client to send with")
	}
	body, deliver, err := route(ctx, client, message)
	if err != nil {
		return nil, err
	}

	wait := sendBackoff
	var failure error
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		var response *qqbotsdk.MessageResponse
		if response, failure = deliver(body); failure == nil {
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

// route builds the body and the call that delivers it, refusing what the platform
// would refuse anyway.
func route(ctx context.Context, client *qqbotsdk.Client, message Message) (*qqbotsdk.Message,
	func(*qqbotsdk.Message) (*qqbotsdk.MessageResponse, error), error) {
	intoGroup := message.GroupOpenID != ""
	intoChat := message.UserOpenID != ""
	switch {
	case intoGroup && intoChat:
		return nil, nil, errors.New("messaging: one message goes to one destination, " +
			"and both a group and a single chat were given")
	case !intoGroup && !intoChat:
		return nil, nil, errors.New("messaging: a message needs a destination, " +
			"and neither a group nor a single chat was given")
	}

	body := &qqbotsdk.Message{
		MsgType:  qqbotsdk.MsgTypeMarkdown,
		Markdown: &qqbotsdk.MessageMarkdown{Content: message.Text},
		Keyboard: message.Keyboard,
	}
	if message.ReplyTo != "" {
		body.MsgID = message.ReplyTo
		body.MsgSeq = message.Sequence
		if body.MsgSeq == 0 {
			body.MsgSeq = 1
		}
	}

	if intoGroup {
		return body, func(body *qqbotsdk.Message) (*qqbotsdk.MessageResponse, error) {
			return client.SendGroupMessage(ctx, message.GroupOpenID, body)
		}, nil
	}
	return body, func(body *qqbotsdk.Message) (*qqbotsdk.MessageResponse, error) {
		return client.SendC2CMessage(ctx, message.UserOpenID, body)
	}, nil
}
