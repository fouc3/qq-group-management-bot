package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// Scene says where a press of a button can come from.
type Scene uint8

const (
	// InGroup: the button is under a message in a group, and the press names the
	// group and the member who pressed it.
	InGroup Scene = 1 << iota
	// InPrivate: the button is under a message in a single chat.
	InPrivate
)

// Press is one button press, already known to carry a namespace some feature
// claimed and to be well-formed for where it happened.
type Press struct {
	// Data is the press as the platform sent it.
	Data *qqbotsdk.InteractionCreateData
	// EventID is the id of the event that carried the press.
	//
	// A feature that answers a press with a message needs it: the platform only
	// accepts an answer to an event as a passive reply within that event's own
	// window, so the id is what makes the reply arrive at all rather than being
	// refused or counted against the active message quota.
	EventID string
	// Payload is the button's own data with its namespace removed, which is what
	// the feature put in the button.
	Payload string
}

// ButtonHandler answers a press of one of the buttons a feature claimed.
type ButtonHandler func(ctx context.Context, press Press) error

// ButtonClaim is one feature's claim on a namespace of button data.
type ButtonClaim struct {
	// Namespace is what every button of this claim has in front of its payload.
	//
	// It is the only thing that says whose button a press is: the platform
	// reports a press with the button's own data and nothing about who made it.
	Namespace string

	// Scenes are the places a press of these buttons is answered -- the group, a
	// single chat, or both.
	//
	// It is separate from where the buttons are put, because the two are not
	// always the same: a keyboard made for a group can arrive with a press the
	// platform says came from a single chat, and a feature that wants to answer
	// there says so here.
	Scenes Scene

	// Handle answers a press of one of them.
	Handle ButtonHandler
}

// Buttons routes button presses to the feature that made the button.
//
// It exists because a press carries nothing but the button's own data: no field
// names the feature that wrote it. Reading that data here means one dispatcher
// instead of every feature looking at every press and guessing whether it is
// theirs -- and it means a namespace is claimed rather than assumed, so two
// features that write the same prefix are refused when the second one claims it
// instead of both acting on one press.
//
// Claiming a namespace is not enough on its own: a feature also has to ask for
// the interaction intent, or the presses it claimed never arrive.
type Buttons struct {
	mu     sync.Mutex
	claims []ButtonClaim
	// wired is the clients a dispatcher has already been declared on. A bot has
	// one connection, so it has one dispatcher however many features claim a
	// part of it.
	wired map[*qqbotsdk.Client]bool
}

// NewButtons returns an empty registry.
func NewButtons() *Buttons {
	return &Buttons{wired: map[*qqbotsdk.Client]bool{}}
}

// ButtonsOrOwn returns the registry to claim in.
//
// shared is what the bot passed down, and it is nil for a feature built without
// one -- which is what a test builds. Such a feature gets a registry of its own,
// which is right for the same reason it is wrong in production: nothing else is
// listening on that connection, so its buttons are its own business.
func ButtonsOrOwn(shared *Buttons) *Buttons {
	if shared == nil {
		return NewButtons()
	}
	return shared
}

// Claim registers one feature's buttons.
//
// A namespace that overlaps one already claimed is refused. Routing a press would
// otherwise depend on which feature claimed first, and the buttons of one would
// answer as those of the other -- the failure being that a member's press does
// something nobody intended.
func (b *Buttons) Claim(claim ButtonClaim) error {
	if strings.TrimSpace(claim.Namespace) == "" {
		return errors.New("command: a button claim carries no namespace")
	}
	if claim.Handle == nil {
		return fmt.Errorf("command: the %q buttons have nothing to answer them",
			claim.Namespace)
	}
	if claim.Scenes == 0 {
		return fmt.Errorf("command: the %q buttons say nothing about where a "+
			"press is answered", claim.Namespace)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, taken := range b.claims {
		if strings.HasPrefix(claim.Namespace, taken.Namespace) ||
			strings.HasPrefix(taken.Namespace, claim.Namespace) {
			return fmt.Errorf("command: the %q buttons overlap the %q ones",
				claim.Namespace, taken.Namespace)
		}
	}
	b.claims = append(b.claims, claim)
	return nil
}

// Register declares the dispatcher on a client, once for each client.
//
// Once, because every feature that claims buttons shares the one dispatcher a bot
// has: a second one would mean every press being routed twice, and the feature it
// belongs to being asked to act on it twice.
func (b *Buttons) Register(client *qqbotsdk.Client) {
	b.mu.Lock()
	first := !b.wired[client]
	b.wired[client] = true
	b.mu.Unlock()
	if !first {
		return
	}
	client.RegisterFunc(qqbotsdk.EventInteractionCreate, b.Dispatch)
}

// Dispatch hands one press to the feature that owns the button, and answers
// nothing about presses nobody claimed.
func (b *Buttons) Dispatch(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.InteractionCreateData)
	if !ok {
		return nil
	}
	claim, payload, found := b.owner(data)
	if !found {
		// A button of a feature that is not running, a press from somewhere no
		// claim names, or an interaction that is not a button press at all. All
		// three are silence rather than an answer: there is nothing to say to a
		// press about something this bot did not offer.
		return nil
	}
	return claim.Handle(ctx, Press{Data: data, EventID: event.ID, Payload: payload})
}

// owner finds the claim a press belongs to, and what its button carries.
func (b *Buttons) owner(data *qqbotsdk.InteractionCreateData) (ButtonClaim, string, bool) {
	scene, ok := sceneOf(data)
	if !ok {
		return ButtonClaim{}, "", false
	}
	if data.Data == nil || data.Data.Resolved == nil {
		return ButtonClaim{}, "", false
	}
	buttonData := data.Data.Resolved.ButtonData

	b.mu.Lock()
	defer b.mu.Unlock()
	// Overlapping namespaces are refused when they are claimed, so at most one
	// claim can match; the longest is taken anyway, so that the rule does not
	// depend on the order the claims were made in.
	var found ButtonClaim
	for _, claim := range b.claims {
		if claim.Scenes&scene == 0 || !strings.HasPrefix(buttonData, claim.Namespace) {
			continue
		}
		if len(claim.Namespace) > len(found.Namespace) {
			found = claim
		}
	}
	if found.Handle == nil {
		return ButtonClaim{}, "", false
	}
	return found, strings.TrimPrefix(buttonData, found.Namespace), true
}

// sceneOf reports where a press happened, when it happened somewhere a button can
// be.
//
// The identifiers each scene needs are required here rather than by every feature:
// without them there is nothing to check a permission against and nowhere to
// answer, and a press nobody can answer is not one to act on.
func sceneOf(data *qqbotsdk.InteractionCreateData) (Scene, bool) {
	switch data.Scene {
	case qqbotsdk.InteractionSceneGroup:
		if data.GroupOpenID == "" || data.GroupMemberOpenID == "" {
			return 0, false
		}
		return InGroup, true
	case qqbotsdk.InteractionSceneC2C:
		if data.UserOpenID == "" {
			return 0, false
		}
		return InPrivate, true
	default:
		return 0, false
	}
}
