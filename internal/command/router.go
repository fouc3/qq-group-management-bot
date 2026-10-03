package command

import (
	"context"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// EventHandler processes one event, which is the shape the SDK's dispatcher
// takes.
type EventHandler func(ctx context.Context, event *qqbotsdk.Event) error

// Handlers is what a feature answers, by the kind of message it arrives in.
type Handlers struct {
	// Group handles a group message, whichever of the two event types carried
	// it.
	Group EventHandler
	// Private handles a message in a single chat.
	Private EventHandler
	// Buttons are the keyboards this feature owns, each claimed by the namespace
	// its button data carries.
	Buttons []ButtonClaim
}

// Router wires a feature's handlers to the events a command can arrive in.
//
// What it holds is the platform's knowledge rather than any one feature's: a
// group message reaches the bot as one of two event types depending on the
// group's receive setting, a single chat is a type of its own, and a button press
// is another. Declared here once, so that a second feature answering commands of
// its own cannot declare them differently -- and so that the reason for reading
// both group types is written down in one place instead of in every feature.
type Router struct {
	// Seen is where a feature takes the decide-once check for a message.
	//
	// Handed over rather than used here, because when to ask is the feature's
	// decision and it is load-bearing: a message dropped before it is looked at
	// must not be recorded as handled.
	Seen *Seen

	// buttons is the registry the keyboards below are claimed in. It is shared
	// with the other features of the same bot, so that one press reaches one of
	// them.
	buttons *Buttons
}

// NewRouter returns a router that claims its buttons in the registry given.
//
// A nil registry -- which is what a feature built without a bot is handed -- gets
// one of its own, for the reason ButtonsOrOwn gives.
func NewRouter(buttons *Buttons) *Router {
	return &Router{Seen: NewSeen(), buttons: ButtonsOrOwn(buttons)}
}

// Register declares the events on the client.
//
// A handler left out is an event the feature does not answer, and nothing is
// registered for it.
//
// An error means nothing was declared at all: the buttons are claimed first, so
// that a namespace another feature owns stops the registration before any event
// is, rather than leaving a bot with half a feature wired up.
func (r *Router) Register(client *qqbotsdk.Client, handlers Handlers) error {
	for _, claim := range handlers.Buttons {
		if err := r.buttons.Claim(claim); err != nil {
			return err
		}
	}
	if handlers.Group != nil {
		// Both group event types are always read, and whether a command had to
		// mention the bot is enforced on the message itself instead.
		//
		// Which event carries a mention depends on the group's receive setting,
		// not on the command: a group set to receive everything delivers a
		// message that mentions the bot as GROUP_MESSAGE_CREATE, as production
		// showed, while a mention-only group delivers the same message as
		// GROUP_AT_MESSAGE_CREATE. Registering only one of them made the bot
		// unable to see commands at all in one of the two modes.
		client.RegisterFunc(qqbotsdk.EventGroupAtMessageCreate, handlers.Group)
		client.RegisterFunc(qqbotsdk.EventGroupMessageCreate, handlers.Group)
	}
	if handlers.Private != nil {
		client.RegisterFunc(qqbotsdk.EventC2CMessageCreate, handlers.Private)
	}
	if len(handlers.Buttons) > 0 {
		// One dispatcher for the bot, however many features claim a part of it.
		r.buttons.Register(client)
	}
	return nil
}
