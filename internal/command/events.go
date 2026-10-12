package command

import (
	"sync"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// Events is where one feature declares the handlers it answers, and the record of what it
// declared.
//
// One entry point rather than a call to the connection per event, because a feature is
// built, stopped and built again while the bot serves: what a stopped instance declared
// has to be given back, and a feature that keeps the registration it was handed has to
// remember to. Three of them did not, and the instance that replaced them went on
// answering every event a second time -- with the configuration it had been replaced for.
// Nothing here can make a caller remember; a type that owns what it was handed can.
//
// One of these belongs to one instance of one feature, which is why it is made per build
// rather than shared the way the button registry is: a shared record would give one
// feature's Stop the power to take another feature's handlers with it.
type Events struct {
	client *qqbotsdk.Client

	mu            sync.Mutex
	registrations []*qqbotsdk.Registration
}

// NewEvents returns the record for one instance of one feature.
func NewEvents(client *qqbotsdk.Client) *Events {
	return &Events{client: client}
}

// EventsOrOwn returns the record to declare in.
//
// shared is what the app made for this build, and it is nil for a feature built without
// one -- which is what a test builds. Such a feature gets a record of its own, which is
// right for the same reason it is wrong in production: nothing else is listening on that
// connection, so its handlers are its own business.
func EventsOrOwn(shared *Events, client *qqbotsdk.Client) *Events {
	if shared == nil {
		return NewEvents(client)
	}
	return shared
}

// On declares one handler for one event type.
func (e *Events) On(eventType string, handler EventHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.registrations = append(e.registrations,
		e.client.RegisterFunc(eventType, handler))
}

// OnGroup declares one handler for both event types a group message arrives as.
//
// Both, always, and whether a command had to mention the bot is enforced on the message
// instead. Which of the two carries a mention depends on the group's receive setting
// rather than on the message -- a group set to receive everything delivers a message that
// mentions the bot as the ordinary event, as production showed, while a mention-only
// group delivers the same message as the mention event. Declaring one of them made the
// bot unable to see anything at all in the other mode.
func (e *Events) OnGroup(handler EventHandler) {
	e.On(qqbotsdk.EventGroupAtMessageCreate, handler)
	e.On(qqbotsdk.EventGroupMessageCreate, handler)
}

// OnPrivate declares one handler for a message in a single chat.
func (e *Events) OnPrivate(handler EventHandler) {
	e.On(qqbotsdk.EventC2CMessageCreate, handler)
}

// Stop gives back everything this instance declared.
//
// It is safe to call more than once, and after a Register that failed or never happened,
// because Close may run after either.
func (e *Events) Stop() {
	e.mu.Lock()
	registrations := e.registrations
	e.registrations = nil
	e.mu.Unlock()

	for _, registration := range registrations {
		registration.Cancel()
	}
}
