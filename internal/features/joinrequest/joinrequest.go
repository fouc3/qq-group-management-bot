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
	return &handler{cfg: cfg, deps: deps}, nil
}

// handler implements feature.Feature.
type handler struct {
	cfg  Config
	deps feature.Deps
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

	action := h.cfg.Action
	if h.cfg.DeclineBots && data.Bot {
		action = ActionDecline
	}
	if action == ActionIgnore {
		h.deps.Logger.Info("a join request is waiting for a person",
			"group", data.GroupOpenID, "user", data.MemberOpenID)
		return nil
	}

	approval := &qqbotsdk.JoinRequestApproval{
		Op:            action,
		JoinRequestID: data.JoinRequestID,
	}
	if action == ActionDecline {
		approval.RejectReason = h.cfg.RejectReason
	}
	if err := h.deps.Client.ApproveGroupJoinRequest(ctx, data.GroupOpenID,
		data.MemberOpenID, approval); err != nil {
		return fmt.Errorf("answering the request with %s: %w", action, err)
	}
	h.deps.Logger.Info("answered a join request",
		"group", data.GroupOpenID, "user", data.MemberOpenID, "action", action)
	return nil
}
