package command

import (
	"context"
	"sync"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// EventHandler processes one event, which is the shape the SDK's dispatcher
// takes.
type EventHandler func(ctx context.Context, event *qqbotsdk.Event) error

// HeldBack reports whether a member's messages are being taken back by the join
// verification, so that nothing is answered at all.
//
// It is a question about somebody else's state, which is why it is asked rather
// than worked out here: only the feature that holds the member knows, and it knows
// for exactly as long as the hold lasts.
type HeldBack func(groupOpenID, memberOpenID string) bool

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

// Router wires a feature's handlers to the events a command can arrive in, and
// takes them back again when the feature stops.
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

	// owner is the feature this router belongs to. It is the name its buttons are
	// claimed under and what Stop gives back.
	owner string

	// buttons is the registry the keyboards below are claimed in. It is shared
	// with the other features of the same bot, so that one press reaches one of
	// them.
	buttons *Buttons

	// mu guards registrations, which is what makes Stop possible.
	mu sync.Mutex
	// registrations are the handlers this router declared, kept so that a feature
	// that stops stops answering.
	registrations []*qqbotsdk.Registration
	// heldBack is asked before a group message reaches the handlers above, and a
	// member it names for is one whose message is not answered at all. Nil when
	// nothing holds anybody back, which is every bot without the verification.
	//
	// It is guarded by mu rather than being a plain field, because the wiring that
	// hands it over runs while the bot is already answering: a feature built again
	// is wired again, and a message can arrive in the middle of that.
	heldBack HeldBack
}

// NewRouter returns a router for one feature, claiming its buttons in the
// registry given.
//
// A nil registry -- which is what a feature built without a bot is handed -- gets
// one of its own, for the reason ButtonsOrOwn gives.
func NewRouter(owner string, buttons *Buttons) *Router {
	return &Router{Seen: NewSeen(), owner: owner, buttons: ButtonsOrOwn(buttons)}
}

// SetHeldBack hands over the question asked before a group message is answered.
//
// A member whose messages are being taken back is one the group cannot see: an
// answer to them would be a message about something nobody else can read, and the
// answer itself is not taken back. So nothing is answered -- no command, no help,
// no failure, and the message is not even counted as one this feature looked at.
//
// It is about the group and the member, not about the message: a hold is over a
// member for as long as it lasts, so every message of theirs is the same answer.
func (r *Router) SetHeldBack(held HeldBack) {
	r.mu.Lock()
	r.heldBack = held
	r.mu.Unlock()
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
	if len(handlers.Buttons) > 0 {
		if err := r.buttons.Set(r.owner, handlers.Buttons); err != nil {
			return err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
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
		group := r.unlessHeldBack(handlers.Group)
		r.registrations = append(r.registrations,
			client.RegisterFunc(qqbotsdk.EventGroupAtMessageCreate, group),
			client.RegisterFunc(qqbotsdk.EventGroupMessageCreate, group))
	}
	if handlers.Private != nil {
		r.registrations = append(r.registrations,
			client.RegisterFunc(qqbotsdk.EventC2CMessageCreate, handlers.Private))
	}
	if len(handlers.Buttons) > 0 {
		// One dispatcher for the bot, however many features claim a part of it.
		// It is deliberately not among the registrations above: it belongs to the
		// connection rather than to this feature, and cancelling it would take
		// the other features' presses with it.
		r.buttons.Register(client)
	}
	return nil
}

// unlessHeldBack wraps a group handler so that a message from a member who is
// being held back never reaches it.
//
// Dropped before the handler rather than inside it, for the reason Seen gives: a
// message this feature never looked at must not be recorded as one it handled, and
// the only way to be sure it was not looked at is not to call.
//
// The sender is read out of the event here instead of by the handler, because the
// question is asked of whoever holds the member, and a feature that had to ask it
// would have to know that holds exist at all. The message is not logged here: the
// hold that drops it says so where it takes the message back, and that is the one
// line worth having.
func (r *Router) unlessHeldBack(handler EventHandler) EventHandler {
	return func(ctx context.Context, event *qqbotsdk.Event) error {
		groupOpenID, memberOpenID, readable := senderOf(event)
		if !readable {
			// Nothing to ask about: the handler is called and says what it thinks
			// of a group event it cannot read, which is the same answer it gave
			// before there was a hold to ask about.
			return handler(ctx, event)
		}
		if r.holds(groupOpenID, memberOpenID) {
			return nil
		}
		return handler(ctx, event)
	}
}

// holds asks the verification whether this member's messages are being taken
// back.
func (r *Router) holds(groupOpenID, memberOpenID string) bool {
	r.mu.Lock()
	held := r.heldBack
	r.mu.Unlock()
	return held != nil && held(groupOpenID, memberOpenID)
}

// senderOf reads who sent a group message, and whether the event said.
//
// An event that does not decode, or that is not a group message at all, is not
// this function's to complain about: it reports that nothing could be read, and
// the handler it was on its way to does the complaining as it always did.
func senderOf(event *qqbotsdk.Event) (groupOpenID, memberOpenID string, readable bool) {
	value, err := event.Decode()
	if err != nil {
		return "", "", false
	}
	data, ok := value.(*qqbotsdk.GroupMessageCreateData)
	if !ok || data.Author == nil || data.Author.MemberOpenID == "" {
		return "", "", false
	}
	return data.GroupOpenID, data.Author.MemberOpenID, true
}

// Stop stops this feature answering, and gives back what it claimed.
//
// It is what makes a feature rebuildable rather than merely startable: a second
// build registers on the same connection, and without this it would sit beside
// the first -- every message answered twice, once per instance, and the button
// namespaces it claimed refused as a conflict with itself.
//
// It is safe to call more than once, because Close may run after a Register that
// failed or never happened.
func (r *Router) Stop() {
	r.mu.Lock()
	registrations := r.registrations
	r.registrations = nil
	r.mu.Unlock()

	for _, registration := range registrations {
		registration.Cancel()
	}
	r.buttons.Release(r.owner)
}
