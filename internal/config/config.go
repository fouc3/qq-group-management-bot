// Package config loads and validates the bot's runtime configuration.
//
// The file is YAML for readability. Only the parts the core owns are typed
// here; every feature owns its own section, which is handed over as a raw node
// so that adding a feature never means editing this package.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults and addresses.
const (
	// DefaultLogLevel is used when the file leaves the level out.
	DefaultLogLevel = "info"
	// DefaultConfigPath is read when no path is given on the command line.
	DefaultConfigPath = "config.yaml"

	// SandboxBaseURL is the sandbox request address.
	SandboxBaseURL = "https://sandbox.api.sgroup.qq.com"
	// ProductionBaseURL is the live request address.
	ProductionBaseURL = "https://api.bot.qq.com"

	// DefaultJoinTimeTolerance is how far apart the official join timestamp
	// and OneBot's recorded join time may be and still describe the same join.
	DefaultJoinTimeTolerance = 15
	// DefaultOneBotTimeout bounds one OneBot call.
	DefaultOneBotTimeout = 10 * time.Second
)

// ErrNoCredentials reports that the bot section carries no usable credential.
var ErrNoCredentials = errors.New(
	"config: set access_token, or set both appid and client_secret",
)

// envReference matches ${NAME} in the file, so a secret can stay out of it.
var envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Config is the whole runtime configuration.
type Config struct {
	// Bot holds credentials and the managed groups.
	Bot Bot `yaml:"bot"`
	// OneBot holds the fallback used for what the official bot may not do.
	OneBot OneBot `yaml:"onebot"`
	// Log holds logging settings.
	Log Log `yaml:"log"`
	// Features holds one raw node per feature, keyed by the feature name.
	//
	// It is deliberately untyped: each feature decodes its own section, so a
	// new feature does not touch this package.
	Features map[string]yaml.Node `yaml:"features"`
}

// Bot holds credentials and how to reach the platform.
type Bot struct {
	// AppID and ClientSecret enable the access token flow and automatic
	// refresh. They are ignored when AccessToken is set.
	AppID string `yaml:"appid"`
	// ClientSecret is the bot secret that pairs with AppID.
	ClientSecret string `yaml:"client_secret"`
	// AccessToken is an already issued token. Setting it skips the token flow.
	AccessToken string `yaml:"access_token"`
	// Sandbox points the client at the sandbox address instead of the live one.
	Sandbox bool `yaml:"sandbox"`
	// BaseURL overrides the address entirely, for tests or a private gateway.
	BaseURL string `yaml:"base_url"`
	// QQ is the bot's own QQ number, the account everyone in the group sees
	// it as.
	//
	// It is needed only by the OneBot fallback, which reads the bot's own
	// messages back and must be able to tell them apart from a message a member
	// forged to look the same.
	QQ int64 `yaml:"qq"`
	// Groups lists the managed groups. Empty means every group the bot is in,
	// which is usually not what an operator wants.
	Groups Groups `yaml:"groups"`
}

// Log holds logging settings.
type Log struct {
	// Level is one of debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is text or json.
	Format string `yaml:"format"`
}

// OneBot configures the fallback used for actions the official bot is not
// allowed to perform.
//
// It exists because the group member removal endpoint answers 40012010
// "应用无接口访问权限" even for a bot that administers the group, so removing
// someone has to be done by an OneBot account.
type OneBot struct {
	// URL is the OneBot HTTP address, for example http://127.0.0.1:3000.
	// Empty disables the fallback, leaving those actions to a person.
	URL string `yaml:"url"`
	// AccessToken is sent as a bearer token when set.
	AccessToken string `yaml:"access_token"`
	// JoinTimeToleranceSeconds is how far apart the official join timestamp
	// and OneBot's recorded join time may be and still be the same join.
	//
	// It is the only bridge between an openid and a QQ number, so it should be
	// wide enough to survive a second or two of processing delay and narrow
	// enough that two people joining at once stay distinguishable.
	JoinTimeToleranceSeconds int64 `yaml:"join_time_tolerance_seconds"`
	// TimeoutSeconds bounds one OneBot call.
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// Enabled reports whether an OneBot fallback is configured.
func (o OneBot) Enabled() bool { return strings.TrimSpace(o.URL) != "" }

// Timeout is the per call timeout.
func (o OneBot) Timeout() time.Duration {
	if o.TimeoutSeconds <= 0 {
		return DefaultOneBotTimeout
	}
	return time.Duration(o.TimeoutSeconds) * time.Second
}

// Tolerance is how many seconds of difference a join match tolerates.
func (o OneBot) Tolerance() int64 {
	if o.JoinTimeToleranceSeconds <= 0 {
		return DefaultJoinTimeTolerance
	}
	return o.JoinTimeToleranceSeconds
}

// Group is one managed group.
//
// Both identifiers for the same group live together, because they are easy to
// confuse: the official platform only ever speaks of the openid, while OneBot
// only ever speaks of the QQ number.
type Group struct {
	// OpenID is the official bot's group openid. It is what every official
	// API call and every event uses.
	OpenID string `yaml:"openid"`
	// QQGroupID is the QQ group number. It is needed only by the OneBot
	// fallback, and leaving it out only disables that fallback for this group.
	QQGroupID int64 `yaml:"qq_group_id"`
}

// DisplayName names a group in messages and logs.
func (g Group) DisplayName() string {
	if g.QQGroupID != 0 {
		return fmt.Sprintf("%s (QQ %d)", g.OpenID, g.QQGroupID)
	}
	return g.OpenID
}

// Groups is the list of managed groups.
type Groups []Group

// Allowed reports whether a group is managed. An empty list allows every
// group, which is what a file that leaves the list out asks for.
func (g Groups) Allowed(openID string) bool {
	if len(g) == 0 {
		return true
	}
	_, found := g.QQGroupID(openID)
	return found
}

// QQGroupID returns the QQ group number for an openid.
func (g Groups) QQGroupID(openID string) (int64, bool) {
	for _, group := range g {
		if group.OpenID == openID {
			return group.QQGroupID, true
		}
	}
	return 0, false
}

// Validate reports whether the list is usable.
func (g Groups) Validate() error {
	seen := make(map[string]struct{}, len(g))
	for i, group := range g {
		openID := strings.TrimSpace(group.OpenID)
		if openID == "" {
			return fmt.Errorf("bot.groups[%d] has no openid", i)
		}
		if _, duplicate := seen[openID]; duplicate {
			return fmt.Errorf("bot.groups lists %s twice", openID)
		}
		seen[openID] = struct{}{}
	}
	return nil
}

// ResolvedBaseURL is the request address to use.
func (b Bot) ResolvedBaseURL() string {
	switch {
	case strings.TrimSpace(b.BaseURL) != "":
		return strings.TrimRight(strings.TrimSpace(b.BaseURL), "/")
	case b.Sandbox:
		return SandboxBaseURL
	default:
		return ProductionBaseURL
	}
}

// Validate reports whether the bot section can produce an authenticated client.
func (b Bot) Validate() error {
	if strings.TrimSpace(b.AccessToken) != "" {
		return nil
	}
	if strings.TrimSpace(b.AppID) != "" && strings.TrimSpace(b.ClientSecret) != "" {
		return nil
	}
	return ErrNoCredentials
}

// Load reads, expands and validates the configuration at path.
//
// References of the form ${NAME} are replaced from the environment before
// parsing, so a secret can live in the environment instead of the file. An
// unset reference is an error rather than an empty string, because a silently
// empty secret is much harder to diagnose.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	expanded, err := expandEnv(string(raw))
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(expanded))
	decoder.KnownFields(true) // a typo in a core key is an error, not a silent default
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// applyDefaults fills in what the file may leave out, then validates.
func (c *Config) applyDefaults() error {
	if strings.TrimSpace(c.Log.Level) == "" {
		c.Log.Level = DefaultLogLevel
	}
	if strings.TrimSpace(c.Log.Format) == "" {
		c.Log.Format = "text"
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q is not debug, info, warn or error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q is not text or json", c.Log.Format)
	}
	if err := c.Bot.Validate(); err != nil {
		return err
	}
	if err := c.Bot.Groups.Validate(); err != nil {
		return err
	}
	if c.OneBot.JoinTimeToleranceSeconds < 0 {
		return fmt.Errorf("onebot.join_time_tolerance_seconds must not be negative, got %d",
			c.OneBot.JoinTimeToleranceSeconds)
	}
	if c.Features == nil {
		c.Features = map[string]yaml.Node{}
	}
	return nil
}

// Feature returns the raw section for a feature.
func (c *Config) Feature(name string) (yaml.Node, bool) {
	node, ok := c.Features[name]
	return node, ok
}

// FeatureNames lists the configured features, for error messages.
func (c *Config) FeatureNames() []string {
	names := make([]string, 0, len(c.Features))
	for name := range c.Features {
		names = append(names, name)
	}
	return names
}

// expandEnv replaces ${NAME} with the environment value of NAME.
func expandEnv(text string) (string, error) {
	var missing []string
	expanded := envReference.ReplaceAllStringFunc(text, func(match string) string {
		name := envReference.FindStringSubmatch(match)[1]
		value, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return match
		}
		return value
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("these environment variables are referenced but unset: %s",
			strings.Join(missing, ", "))
	}
	return expanded, nil
}
