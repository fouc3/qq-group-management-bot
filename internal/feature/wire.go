package feature

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/fouc3/qq-group-management-bot/internal/command"
)

// FindProvider returns the one feature that provides a capability.
//
// Two features providing the same thing is refused rather than resolved: which
// one a consumer was handed would otherwise be decided by the order the features
// happen to be registered in, and that is not a decision to leave to chance.
//
// Nothing providing it is not an error. A bot without a judge, or without anyone
// watching joins, is a deployment rather than a mistake -- and the zero value is
// what tells a feature to say "there is nothing behind this command" instead of
// failing later on a nil it never expected. The name is then empty, which is how
// Inject knows to say so.
func FindProvider[P any](features []Feature, what string) (P, string, error) {
	var zero, found P
	var names []string
	for _, instance := range features {
		if candidate, ok := any(instance).(P); ok {
			found = candidate
			names = append(names, instance.Name())
		}
	}
	switch len(names) {
	case 0:
		return zero, "", nil
	case 1:
		return found, names[0], nil
	default:
		return zero, "", fmt.Errorf("feature: %s both provide %s",
			strings.Join(names, " and "), what)
	}
}

// CommandSources gathers the commands every running feature offers, in
// registration order.
//
// It is the plural of FindProvider: several features add to one table rather than
// one feature promising something to another. Nothing offering commands is not a
// state to report or refuse -- it is a bot whose only commands are the table
// owner's own.
func CommandSources(features []Feature, logger *slog.Logger) []command.Def {
	var defs []command.Def
	for _, instance := range features {
		source, ok := any(instance).(CommandSource)
		if !ok {
			continue
		}
		offered := source.CommandDefs()
		if logger != nil && len(offered) > 0 {
			logger.Info("a feature offered its commands",
				"feature", instance.Name(), "commands", len(offered))
		}
		defs = append(defs, offered...)
	}
	return defs
}

// InjectCommandSources hands the gathered commands to whoever keeps the table.
//
// It does not go through Inject, for the two reasons that make it a different
// shape: the provider is a list gathered from everyone rather than one feature
// found among them, and this is the one wiring whose consumer can refuse what it
// is handed. A table refuses two commands answering to one word, and the table is
// the only place that can see it -- the sources do not know about each other.
func InjectCommandSources(features []Feature, logger *slog.Logger) error {
	defs := CommandSources(features, logger)

	var consumers []string
	for _, instance := range features {
		aware, ok := any(instance).(CommandSourceAware)
		if !ok {
			continue
		}
		if err := aware.SetCommandSources(defs); err != nil {
			return fmt.Errorf("feature %s: %w", instance.Name(), err)
		}
		consumers = append(consumers, instance.Name())
	}

	if logger == nil {
		return nil
	}
	attrs := []any{
		"capability", "the command table",
		"commands", len(defs),
		"consumers", strings.Join(consumers, " "),
	}
	switch {
	case len(consumers) == 0 && len(defs) > 0:
		logger.Warn("commands are offered but no running feature keeps a table", attrs...)
	case len(defs) > 0:
		logger.Info("features wired", attrs...)
	}
	return nil
}

// Inject hands a capability to every feature that wants it.
//
// It is the whole of the wiring: one mechanism for what three near-identical
// functions used to do -- find who provides it, refuse two providers, hand it to
// each feature that takes it. Adding a capability is a contract, the interface a
// consumer implements beside it, and one call.
//
// set is the setter the consumer declares, as a method expression. It goes in
// rather than a type assertion here because Go cannot ask "does this feature take
// a verifier" by itself, and because the method expression is the compiler's
// proof that the setter takes exactly what the provider is.
//
// A wiring nobody can see is a wiring nobody can debug: a feature that forgot to
// take a capability looks exactly like one that does not want it. What was wired,
// to whom, and whether anybody provided it is therefore logged.
func Inject[P any, A any](features []Feature, provider P, providerBy, what string,
	logger *slog.Logger, set func(A, P)) {
	var consumers []string
	for _, instance := range features {
		aware, ok := any(instance).(A)
		if !ok {
			continue
		}
		set(aware, provider)
		consumers = append(consumers, instance.Name())
	}

	if logger == nil {
		return
	}
	attrs := []any{
		"capability", what,
		"provider", providerBy,
		"consumers", strings.Join(consumers, " "),
	}
	switch {
	case providerBy == "":
		logger.Info("a capability nobody provides was handed over as nothing", attrs...)
	case len(consumers) == 0:
		logger.Warn("a capability is provided but no running feature wants it", attrs...)
	default:
		logger.Info("features wired", attrs...)
	}
}
