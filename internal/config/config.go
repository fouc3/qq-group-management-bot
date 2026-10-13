// Package config loads and validates the bot's runtime configuration.
//
// The file is YAML for readability. Only the parts the core owns are typed
// here; every feature owns its own section, which is handed over as a raw node
// so that adding a feature never means editing this package.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	// Database says where what has to outlive a restart is kept.
	Database Database `yaml:"database"`
	// Redis says where the volatile cache lives.
	Redis Redis `yaml:"redis"`
	// HumanVerification says where the online human verification service lives.
	//
	// Optional, and typed here rather than owned by a feature: the client it
	// describes is opened once for the whole process, because the results arrive
	// on one long connection and that connection must not go down every time a
	// feature is built again.
	HumanVerification HumanVerification `yaml:"human_verification"`
	// Log holds logging settings.
	Log Log `yaml:"log"`
	// Features holds one raw node per feature, keyed by the feature name.
	//
	// It is deliberately untyped: each feature decodes its own section, so a
	// new feature does not touch this package.
	Features map[string]yaml.Node `yaml:"features"`
}

// DefaultRedisAddr is where the cache is looked for when the file names no
// place: a Redis of this bot's own, on the loopback interface.
//
// Its own, because the cache shares a keyspace with whatever else uses the same
// server, and a keyspace is not something to share by accident.
const DefaultRedisAddr = "127.0.0.1:6380"

// Redis is where the volatile cache lives -- group messages kept for a short
// while so that a reported one can be judged in context.
//
// It is deliberately separate from Database: what is here may be lost without
// consequence, and what is there may not. Nothing in this section is a source of
// truth.
type Redis struct {
	// Addr is host:port.
	Addr string `yaml:"addr"`
	// Password is empty when the server wants none.
	Password string `yaml:"password"`
	// DB is the numeric database, for a server shared with something else that
	// keeps to its own.
	DB int `yaml:"db"`
	// Prefix is put in front of every key, so one server can hold several
	// applications' keys without a collision being possible.
	Prefix string `yaml:"prefix"`
	// DialTimeoutSeconds bounds connecting and each command.
	//
	// Short on purpose: the cache sits in the path of answering a command, and
	// waiting on a dead server is worse than judging without context.
	DialTimeoutSeconds int `yaml:"dial_timeout_seconds"`
}

// RedisConfig is the section as the app uses it, with the defaults filled in.
//
// The defaults are applied here rather than written back into the section so
// that one place decides what an unset field means.
func (c *Config) RedisConfig() Redis {
	redis := c.Redis
	if strings.TrimSpace(redis.Addr) == "" {
		redis.Addr = DefaultRedisAddr
	}
	if strings.TrimSpace(redis.Prefix) == "" {
		redis.Prefix = DefaultRedisPrefix
	}
	if redis.DialTimeoutSeconds <= 0 {
		redis.DialTimeoutSeconds = DefaultRedisDialTimeoutSeconds
	}
	return redis
}

// DefaultRedisPrefix keeps this bot's keys apart from anything else's.
const DefaultRedisPrefix = "qgb"

// DefaultRedisDialTimeoutSeconds is how long a cache command may take.
const DefaultRedisDialTimeoutSeconds = 3

// HumanVerification is where the online human verification service lives: the
// service that checks a person in a browser, so the bot can let somebody who has
// proved themselves in without ever knowing what they look like.
//
// One section for the whole bot, not one per group, and one section rather than
// a feature's own: the address and the two tokens identify the service account,
// and the connection the results arrive on is opened once for the process. A
// group only decides whether it uses it at all.
//
// Optional. A deployment that leaves the section out simply cannot offer online
// verification, and a group that asks for it is refused at startup rather than
// quietly holding members with no way to verify themselves.
type HumanVerification struct {
	// BaseURL is the service's address, http(s)://host[:port]. The result
	// connection is derived from it, so this is the only address to write down.
	BaseURL string `yaml:"base_url"`
	// IssueToken authorises asking the service for a link. Server-side secret:
	// whoever holds it can issue a verification link for anything.
	IssueToken string `yaml:"issue_token"`
	// WSToken authorises the connection the results arrive on. Server-side
	// secret for the same reason.
	WSToken string `yaml:"ws_token"`
	// Cdata is the business marker every link is signed with, and it is what
	// the service's statistics group by. Empty means the marker this bot ships
	// with.
	//
	// Changing it while links are outstanding has a consequence worth knowing:
	// a result is only acted on when it carries the marker this bot signs, so
	// links issued under the old one are held in silence until the member is
	// asked to verify again or the deadline acts.
	Cdata string `yaml:"cdata"`
	// TTLSeconds is how long one link stays usable. The service's bounds are
	// 60..86400 and it refuses a value outside them; 0 means the default.
	//
	// It is also how long a member has to find the link and open it: a link
	// that has expired is answered with 链接已过期, and the way to get another
	// one is for an administrator to send the verification again.
	TTLSeconds int `yaml:"ttl_seconds"`
	// TimeoutSeconds bounds one call to the service. 0 means the default.
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// DefaultHumanVerificationTTLSeconds and DefaultHumanVerificationTimeoutSeconds
// are what an unset field means.
const (
	// DefaultHumanVerificationTTLSeconds is the service's own default lifetime.
	DefaultHumanVerificationTTLSeconds = 900
	// MinHumanVerificationTTLSeconds and MaxHumanVerificationTTLSeconds are the
	// service's bounds, which it enforces by refusing the request.
	MinHumanVerificationTTLSeconds = 60
	MaxHumanVerificationTTLSeconds = 86400
	// DefaultHumanVerificationTimeoutSeconds is how long one call may take.
	DefaultHumanVerificationTimeoutSeconds = 10
)

// humanVerificationCdata matches what the service accepts as a business marker:
// letters, digits, underscores and hyphens, at most 255 of them.
//
// It is checked here rather than left to the service because the failure is
// silent and expensive: anything else makes Turnstile drop the value without
// saying so, and with the marker gone the result of a genuine verification is
// discarded as somebody else's.
var humanVerificationCdata = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// Configured reports whether the file asks for an online verification service at
// all.
//
// The address is what decides it: a section with tokens and no address is a
// mistake the validation refuses, and a section nobody wrote has neither.
func (h HumanVerification) Configured() bool {
	return strings.TrimSpace(h.BaseURL) != ""
}

// applyDefaults fills in what the section leaves out, then validates it.
func (h *HumanVerification) applyDefaults() error {
	if !h.Configured() {
		// Nothing is asked for, so there is nothing to check -- and the tokens
		// are deliberately not reported as missing, because a deployment that
		// does not verify online has no use for them.
		return nil
	}
	address, err := url.Parse(strings.TrimRight(strings.TrimSpace(h.BaseURL), "/"))
	if err != nil {
		return fmt.Errorf("human_verification.base_url %q: %w", h.BaseURL, err)
	}
	switch address.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("human_verification.base_url %q is not http or https", h.BaseURL)
	}
	if address.Host == "" {
		return fmt.Errorf("human_verification.base_url %q names no host", h.BaseURL)
	}
	if strings.TrimSpace(h.IssueToken) == "" {
		return errors.New("human_verification.issue_token is required: without it " +
			"no verification link can be asked for")
	}
	if strings.TrimSpace(h.WSToken) == "" {
		return errors.New("human_verification.ws_token is required: without it the " +
			"results have nowhere to arrive, and a member who verifies would never " +
			"be let out of the hold")
	}
	if marker := strings.TrimSpace(h.Cdata); marker != "" &&
		!humanVerificationCdata.MatchString(marker) {
		return fmt.Errorf("human_verification.cdata %q may only contain letters, "+
			"digits, underscores and hyphens (up to 255): anything else is dropped "+
			"by Cloudflare without a word, and a result that comes back without its "+
			"marker is discarded as somebody else's", h.Cdata)
	}
	if h.TTLSeconds == 0 {
		h.TTLSeconds = DefaultHumanVerificationTTLSeconds
	}
	if h.TTLSeconds < MinHumanVerificationTTLSeconds || h.TTLSeconds > MaxHumanVerificationTTLSeconds {
		return fmt.Errorf("human_verification.ttl_seconds must be between %d and %d, got %d",
			MinHumanVerificationTTLSeconds, MaxHumanVerificationTTLSeconds, h.TTLSeconds)
	}
	if h.TimeoutSeconds < 0 {
		return fmt.Errorf("human_verification.timeout_seconds must not be negative, got %d",
			h.TimeoutSeconds)
	}
	if h.TimeoutSeconds == 0 {
		h.TimeoutSeconds = DefaultHumanVerificationTimeoutSeconds
	}
	return nil
}

// Database is where the bot keeps what must survive a restart: the members held
// until they verify, and the applicants barred from joining.
//
// It is not where the settings live. The configuration stays a file a person
// edits, and its credentials have no business in a database.
type Database struct {
	// Driver is sqlite or postgres. Empty means sqlite, the default.
	Driver string `yaml:"driver"`
	// DSN is a file path for sqlite and a connection string for postgres.
	//
	// Empty means the default file, so a deployment that does not care where it
	// lives does not have to name a place.
	DSN string `yaml:"dsn"`
	// MaxOpenConns caps the pool. SQLite has a single writer and wants one;
	// leaving it out means one.
	MaxOpenConns int `yaml:"max_open_conns"`
}

// DefaultDatabaseDriver is the driver used when the file names none.
//
// SQLite is the prototype: no server to run and no compiler to build against,
// and the same code runs against PostgreSQL by changing this value.
const DefaultDatabaseDriver = "sqlite"

// DefaultDatabasePath is where the database lives when the file names no place.
//
// Under the state directory rather than beside the configuration: it is state,
// not settings, and somebody editing the configuration should not have to think
// about where it went.
func DefaultDatabasePath() (string, error) {
	directory := os.Getenv("XDG_STATE_HOME")
	if strings.TrimSpace(directory) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding the home directory: %w", err)
		}
		directory = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(directory, "qq-group-management-bot", "bot.db"), nil
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
	if strings.TrimSpace(c.Database.Driver) == "" {
		c.Database.Driver = DefaultDatabaseDriver
	}
	switch c.Database.Driver {
	case DefaultDatabaseDriver, "postgres":
	default:
		return fmt.Errorf("database.driver %q is not %s or postgres",
			c.Database.Driver, DefaultDatabaseDriver)
	}
	if c.Database.MaxOpenConns < 0 {
		return fmt.Errorf("database.max_open_conns must not be negative, got %d",
			c.Database.MaxOpenConns)
	}
	if strings.TrimSpace(c.Database.DSN) == "" {
		path, err := DefaultDatabasePath()
		if err != nil {
			return err
		}
		c.Database.DSN = path
	}
	if c.Redis.DB < 0 {
		return fmt.Errorf("redis.db must not be negative, got %d", c.Redis.DB)
	}
	if c.Redis.DialTimeoutSeconds < 0 {
		return fmt.Errorf("redis.dial_timeout_seconds must not be negative, got %d",
			c.Redis.DialTimeoutSeconds)
	}
	// The address is not required: an unset one means the default cache server,
	// and a deployment that does not use the cache at all never needs to name
	// one.
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
	if err := c.HumanVerification.applyDefaults(); err != nil {
		return err
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
