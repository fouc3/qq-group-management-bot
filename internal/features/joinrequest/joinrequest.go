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
	return nil
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

	action := h.cfg.Action
	reason := h.cfg.RejectReason
	if barred {
		action = ActionDecline
		reason = barredReason
		h.deps.Logger.Warn("refused an applicant who is barred from joining",
			"group", data.GroupOpenID, "user", data.MemberOpenID)
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
