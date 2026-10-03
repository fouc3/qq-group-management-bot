package feature

import "gopkg.in/yaml.v3"

// Reloadable is a feature that can adopt a new configuration while it is running.
//
// The section is the feature's own part of the file, decoded exactly as New decodes
// it, so a feature implements one path for the first configuration and every later
// one. An error means nothing was adopted: the feature keeps the configuration it
// already had, and the reload says so rather than leaving a half-applied file behind.
//
// A feature that does not implement this cannot be reconfigured at all. That is not
// a gap to paper over: a reload which would change such a section is refused whole,
// because a bot running half of one file and half of another is worse than a bot
// running an old one.
type Reloadable interface {
	Reload(section yaml.Node) error
}
