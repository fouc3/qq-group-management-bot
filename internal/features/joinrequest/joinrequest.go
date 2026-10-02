// Package joinrequest answers requests to join a group.
//
// It exists as a second feature to show the shape one takes: a section of its
// own in the configuration file, the intents it needs, and its own handlers.
// Unlike the verification feature it never messages anyone, it only decides.
package joinrequest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// Name is the feature's name: its configuration key and its log field.
const Name = "join_request"

// The actions a request can be given.
const (
	// ActionIgnore leaves the request for a person to answer.
	ActionIgnore = "ignore"
	// ActionApprove admits the applicant.
	ActionApprove = "approve"
	// ActionDecline refuses the applicant.
	ActionDecline = "decline"
)

// barredReason is what a barred applicant is told.
//
// Deliberately neutral, and deliberately not the configured reject_reason:
// telling somebody they are on a list invites them to argue about it, and the
// list is not theirs to audit. A refusal that looks like every other refusal
// says everything the applicant needs to know.
const barredReason = "暂不接受你的加群申请"

// Applicant is one join request, reduced to what a decision about it needs.
//
// A struct rather than the raw event, so a blacklist is handed a stable shape:
// the list may well outlive the current event fields, and it should not have to
// be rewritten when one is added.
type Applicant struct {
	GroupOpenID string
	// MemberOpenID identifies the applicant to this application, and is what a
	// blacklist is keyed on.
	MemberOpenID string
	// UnionOpenID is the cross-application identity, for a list that has to
	// outlive this bot. Both are passed: the choice is not made yet.
	UnionOpenID string
	Username    string
	// InvitedBy names the inviter when the applicant was invited.
	InvitedBy string
}

// applicantOf reduces a request to the fields a decision uses.
func applicantOf(data *qqbotsdk.GroupJoinRequestData) Applicant {
	return Applicant{
		GroupOpenID:  data.GroupOpenID,
		MemberOpenID: data.MemberOpenID,
		UnionOpenID:  data.UnionOpenID,
		Username:     data.Username,
		InvitedBy:    data.InvitedBy,
	}
}

// Blacklist answers whether an applicant is barred from joining.
//
// An interface rather than a call into a store because the list does not exist
// yet: the handler needs a decision, not a source, and keeping the two apart is
// what lets the list be chosen later without touching the handler.
type Blacklist interface {
	// Barred reports whether the applicant may not join.
	//
	// An error must mean "unknown", never "allowed": the caller leaves the
	// request for a person rather than guessing.
	Barred(ctx context.Context, applicant Applicant) (bool, error)
}

// emptyBlacklist bars nobody. It is what the feature runs on today.
//
// TODO: replace with the real list. Nothing else has to change: the check is
// already the first thing a request passes through, so filling this in is a
// change to this file alone.
type emptyBlacklist struct{}

// Barred implements Blacklist.
func (emptyBlacklist) Barred(context.Context, Applicant) (bool, error) { return false, nil }

// Config is this feature's section in the configuration file.
type Config struct {
	// Enabled turns the switch on. A section written without it is on.
	Enabled bool `yaml:"enabled"`
	// Action is what to do with a request: ignore, approve or decline.
	//
	// The default is ignore on purpose. Admitting or refusing people
	// automatically is a decision an operator should make explicitly.
	Action string `yaml:"action"`
	// RejectReason is sent with a decline.
	RejectReason string `yaml:"reject_reason"`
	// DeclineBots declines applicants the platform flags as bots, whatever
	// Action says, while leaving people to the configured action.
	DeclineBots bool `yaml:"decline_bots"`
	// Barred says what an applicant on the blacklist gets: decline or ignore.
	//
	// It is a policy rather than a fact, which is why it is not fixed in code:
	// refusing somebody outright and quietly leaving them for a person are both
	// defensible, and a group should not have to fork to choose.
	//
	// Two boundaries belong with it. A barred applicant is only ever refused or
	// left alone -- never admitted, so approve is not a value here. And this says
	// nothing about members who are already in the group: the list decides a join
	// request, it does not reach back and remove anybody.
	Barred string `yaml:"barred"`
	// Groups changes what single groups do, keyed by group openid.
	//
	// Only the action is overridable, because that is the decision a group
	// actually differs on: a large public group may want to admit everybody and
	// let verification sort it out, while a small one wants to look at each
	// request. Whether bots are declined is not a per-group question.
	Groups map[string]GroupOverride `yaml:"groups"`
}

// GroupOverride is one group's difference from the section.
//
// A pointer field so that "not written" is distinguishable from "written as the
// zero value": a group that only sets an action must inherit the rest.
type GroupOverride struct {
	Action *string `yaml:"action"`
}

// actionFor returns the action that applies to a group.
func (c *Config) actionFor(groupOpenID string) string {
	if override, known := c.Groups[groupOpenID]; known && override.Action != nil {
		return *override.Action
	}
	return c.Action
}

// barredAction is what an applicant on the blacklist gets.
//
// The default is to refuse them: a list that only leaves the decision to a person
// still lets the request sit there looking like any other, which is not what
// anybody writes a list for.
func (c *Config) barredAction() string {
	if strings.TrimSpace(c.Barred) == "" {
		return ActionDecline
	}
	return c.Barred
}

// validateGroups checks every group's action at startup, so a typo is found when
// the file is read rather than when somebody is waiting at the door.
func (c *Config) validateGroups() error {
	for groupOpenID, override := range c.Groups {
		if strings.TrimSpace(groupOpenID) == "" {
			return errors.New("groups has an entry with an empty group openid")
		}
		if override.Action == nil {
			continue
		}
		switch *override.Action {
		case ActionIgnore, ActionApprove, ActionDecline:
		default:
			return fmt.Errorf("groups[%s]: action %q is not %s, %s or %s",
				groupOpenID, *override.Action, ActionIgnore, ActionApprove, ActionDecline)
		}
	}
	return nil
}

// applyDefaults fills in what the section leaves out, then validates it.
func (c *Config) applyDefaults() error {
	if strings.TrimSpace(c.Action) == "" {
		c.Action = ActionIgnore
	}
	switch c.Action {
	case ActionIgnore, ActionApprove, ActionDecline:
	default:
		return fmt.Errorf("action %q is not %s, %s or %s",
			c.Action, ActionIgnore, ActionApprove, ActionDecline)
	}
	if c.Action != ActionDecline && strings.TrimSpace(c.RejectReason) != "" {
		return errors.New("reject_reason only applies to action: decline")
	}
	switch c.barredAction() {
	case ActionDecline, ActionIgnore:
	default:
		return fmt.Errorf("barred %q is not %s or %s: an applicant on the list is "+
			"either refused or left to a person, never admitted",
			c.Barred, ActionDecline, ActionIgnore)
	}
	return c.validateGroups()
}

// New builds the feature from its configuration section.
func New(section yaml.Node, deps feature.Deps) (feature.Feature, error) {
	var cfg Config
	if err := section.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("reading the %s section: %w", Name, err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	return &handler{cfg: cfg, deps: deps, blacklist: emptyBlacklist{}}, nil
}

// SetBlacklist hands over the list the feature checks.
//
// The app calls it after the feature is built and before it is registered, so no
// request can arrive in between. A deployment that never calls it runs on
// emptyBlacklist and bars nobody, which is the right way round: a list that
// cannot be reached must refuse no one rather than block everybody.
func (h *handler) SetBlacklist(blacklist Blacklist) {
	h.blacklist = blacklist
}

// handler implements feature.Feature.
type handler struct {
	cfg  Config
	deps feature.Deps
	// blacklist answers whether an applicant is barred. It is the first thing a
	// request is checked against, and it is a field so that a test can decide
	// what it says.
	blacklist Blacklist
}

// Name implements feature.Feature.
func (h *handler) Name() string { return Name }

// Intents implements feature.Feature.
func (h *handler) Intents() qqbotsdk.Intent {
	return qqbotsdk.IntentGroupMemberEvent
}

// Register implements feature.Feature.
func (h *handler) Register(_ context.Context) error {
	h.deps.Client.RegisterFunc(qqbotsdk.EventGroupJoinRequest, h.onJoinRequest)
	return nil
}

// Close implements feature.Feature.
func (h *handler) Close(context.Context) error { return nil }

// onJoinRequest handles GROUP_JOIN_REQUEST.
func (h *handler) onJoinRequest(ctx context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		return err
	}
	data, ok := value.(*qqbotsdk.GroupJoinRequestData)
	if !ok {
		return fmt.Errorf("join request handling got %T", value)
	}

	// Every identity the platform sent, logged before any decision is made.
	//
	// The point is the question this event otherwise leaves open: an applicant is
	// identified by an OpenID, never by a QQ number, and the blacklist compares
	// that OpenID with one recorded from a message in the group. Whether those
	// two are the same value is not something the documentation settles, so the
	// first real request here answers it -- compare this member_openid with what
	// /whois reports for the same person once they are in.
	h.deps.Logger.Info("a join request arrived",
		"group", data.GroupOpenID,
		"member_openid", data.MemberOpenID,
		"union_openid", data.UnionOpenID,
		"username", data.Username,
		"bot", data.Bot,
		"apply_source", data.ApplySource,
		"invited_by", data.InvitedBy,
		"request", data.JoinRequestID)

	if !h.deps.InGroup(data.GroupOpenID) {
		h.deps.Logger.Debug("ignoring a request outside the configured groups",
			"group", data.GroupOpenID)
		return nil
	}

	// The blacklist is checked before anything else decides, so that a barred
	// applicant is refused whatever the configured action says -- including
	// action: approve, which is the point of having the check first.
	barred, err := h.blacklist.Barred(ctx, applicantOf(data))
	if err != nil {
		// Unknown is not a decision. Admitting would let a barred applicant in,
		// and refusing would turn a lookup failure into rejections of ordinary
		// people, so the request is left for a person instead: that is what
		// action: ignore means, and it is the only answer that cannot be wrong.
		h.deps.Logger.Error("could not check the join blacklist, "+
			"leaving the request for a person",
			"group", data.GroupOpenID, "user", data.MemberOpenID, "error", err)
		return nil
	}

	// The action is the group's own, so one group can admit everybody while
	// another looks at each request.
	action := h.cfg.actionFor(data.GroupOpenID)
	reason := h.cfg.RejectReason
	if barred {
		// Refused or left to a person, whichever the group asked for. Never
		// admitted: being on the list is not the same as being approved.
		action = h.cfg.barredAction()
		if action == ActionDecline {
			reason = barredReason
		}
		h.deps.Logger.Warn("an applicant who is barred from joining was answered",
			"group", data.GroupOpenID, "user", data.MemberOpenID, "outcome", action)
	}
	if h.cfg.DeclineBots && data.Bot {
		action = ActionDecline
	}
	if action == ActionIgnore {
		h.deps.Logger.Info("a join request is waiting for a person",
			"group", data.GroupOpenID, "user", data.MemberOpenID, "barred", barred)
		return nil
	}

	approval := &qqbotsdk.JoinRequestApproval{
		Op:            action,
		JoinRequestID: data.JoinRequestID,
	}
	if action == ActionDecline {
		approval.RejectReason = reason
	}
	if err := h.deps.Client.ApproveGroupJoinRequest(ctx, data.GroupOpenID,
		data.MemberOpenID, approval); err != nil {
		return fmt.Errorf("answering the request with %s: %w", action, err)
	}
	h.deps.Logger.Info("answered a join request",
		"group", data.GroupOpenID, "user", data.MemberOpenID, "action", action)
	return nil
}
