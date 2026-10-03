package feature

import (
	"context"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
)

// stub is a feature that only writes down what it was built with, and how many
// times it was started and stopped.
type stub struct {
	name string
	// buttons is the registry this feature was handed, kept so that a test can
	// compare it with its neighbour's.
	buttons *command.Buttons
}

func (s *stub) Name() string                   { return s.name }
func (s *stub) Intents() qqbotsdk.Intent       { return 0 }
func (s *stub) Register(context.Context) error { return nil }
func (s *stub) Close(context.Context) error    { return nil }
