package httpapi

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinverify"
	"github.com/fouc3/qq-group-management-bot/internal/logring"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// The shapes this package answers with.
//
// They are declared here rather than the store's own structs being encoded
// directly, for one reason: what a table column is called is not what a console
// should be told. A store type keeps an int64 because the column is an integer;
// a console wants the time it means. Keeping the two apart also means a column
// can be renamed without the front end breaking, which is the whole point of
// having an interface at all.
//
// Times are Unix seconds, the same as everywhere else in this bot: the browser
// is the only place that knows what time zone the person reading it is in.

// statusResponse is what the console shows on its first screen.
type statusResponse struct {
	// Version names the build. It is what an operator quotes when something is
	// wrong, so it is read from the binary rather than from a constant that
	// somebody has to remember to change.
	Version string `json:"version"`
	// StartedAt is when the process came up, in Unix seconds.
	StartedAt int64 `json:"started_at"`
	// UptimeSeconds is how long it has been up.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// ConfigPath is the file that was read.
	ConfigPath string `json:"config_path"`
	// Features is every feature the file names, and whether it is running.
	//
	// Both, and not just the second: a feature written in the file and not
	// running is the state that looks like nothing at all, and it is the one an
	// operator has to be able to see.
	Features []featureState `json:"features"`
	// GroupsManaged is how many groups bot.groups names, which is zero when the
	// file leaves it out and every group is managed.
	GroupsManaged int `json:"groups_managed"`
	// Database describes where the bot's records live.
	Database databaseInfo `json:"database"`
	// Redis describes the cache. It carries no password.
	Redis redisInfo `json:"redis"`
	// CommandPrefix is what a command is typed with.
	CommandPrefix string `json:"command_prefix"`
	// JudgedModel names the model the moderation feature judges with, empty when
	// nothing is configured.
	JudgedModel string `json:"judged_model"`
	// HumanVerification is the online verification service: whether the file
	// configures one, and whether its connection is up right now.
	//
	// Both, because the difference is the thing an operator acts on: a deployment
	// with the section but no connection cannot hand out a single link, and until
	// somebody looks it reads like one that verifies online.
	HumanVerification humanVerificationStatus `json:"human_verification"`
}

// humanVerificationStatus is the online verification service's health.
type humanVerificationStatus struct {
	// Configured reports whether the file names a service, never its tokens.
	Configured bool `json:"configured"`
	// Connected reports whether a result could reach this process now.
	Connected bool `json:"connected"`
}

// featureState is one feature's switch and whether it is live.
type featureState struct {
	Name string `json:"name"`
	// Enabled is what the file says. A section written without the switch is on.
	Enabled bool `json:"enabled"`
	// Running is whether it was built and registered.
	Running bool `json:"running"`
}

// databaseInfo says where the records are, and how much there is.
type databaseInfo struct {
	Driver string `json:"driver"`
	// SizeBytes is the size of the SQLite file, or zero for a database this
	// process cannot measure -- a PostgreSQL server is not a file.
	SizeBytes int64 `json:"size_bytes"`
}

// redisInfo says where the cache is. Never its password.
type redisInfo struct {
	Addr   string `json:"addr"`
	DB     int    `json:"db"`
	Prefix string `json:"prefix"`
}

// overviewResponse is the summary the first screen is built from.
type overviewResponse struct {
	// Since is the beginning of the window the counts are over, in Unix seconds.
	//
	// It is reported rather than left to the caller to remember: a count without
	// the window it is over is a number nobody can check.
	Since int64 `json:"since"`
	// Groups is how many groups the file names.
	Groups int `json:"groups"`
	// Pending is how many members are being held until they verify.
	Pending int `json:"pending"`
	// HighRisk is how many members are on the list whose messages are judged.
	HighRisk int `json:"high_risk"`
	// Blacklist counts the barred applicants.
	Blacklist blacklistCounts `json:"blacklist"`
	// MemberEvents counts who came and went.
	MemberEvents memberEventCounts `json:"member_events"`
	// Judgements counts what was judged.
	Judgements judgementCounts `json:"judgements"`
	// Notes says what a number cannot: which of them is a lower bound because
	// the query behind it is bounded, and which are missing because the
	// configuration does not let a question be asked.
	//
	// A console that showed a bounded count as if it were exact would be lying
	// in the one place an operator trusts.
	Notes []string `json:"notes,omitempty"`
}

// blacklistCounts is the barred list, per group and in total.
type blacklistCounts struct {
	// Total counts entries by their own id, so one entry that every group is
	// barred by is counted once rather than once per group.
	Total   int                 `json:"total"`
	ByGroup []blacklistPerGroup `json:"by_group"`
}

// blacklistPerGroup is one group's share of the list.
type blacklistPerGroup struct {
	GroupOpenID string `json:"group_openid"`
	Count       int    `json:"count"`
}

// memberEventCounts is who came and went in the window.
type memberEventCounts struct {
	Total  int `json:"total"`
	Joined int `json:"joined"`
	Left   int `json:"left"`
	// Scanned is how many rows were read to answer this. The store lists the
	// newest rows and has no count, so a window that reaches further back than
	// this is under-counted -- which is what the console says instead of
	// pretending the number is the whole truth.
	Scanned int `json:"scanned"`
}

// judgementCounts is what was judged in the window.
type judgementCounts struct {
	Total      int `json:"total"`
	Violations int `json:"violations"`
	OK         int `json:"ok"`
	Errors     int `json:"errors"`
	// Truncated reports that some group answered with as many rows as it was
	// willing to return, so more may exist further back.
	Truncated bool `json:"truncated"`
}

// groupsResponse lists the managed groups and their switches.
type groupsResponse struct {
	Groups []groupResponse `json:"groups"`
	// Unrestricted reports that bot.groups is empty, which means every group the
	// bot is in is managed -- including ones this configuration has never heard
	// of, so the list below is not the whole story.
	Unrestricted bool `json:"unrestricted"`
}

// groupResponse is one group and what applies to it.
type groupResponse struct {
	OpenID string `json:"openid"`
	// QQGroupID is the group's number, or zero when the file does not name it.
	// The openid is what the platform speaks; the number is what a person does.
	QQGroupID int64 `json:"qq_group_id"`
	// Features is every feature the file names, with what applies to this group.
	Features []groupFeature `json:"features"`
}

// groupFeature is one feature's switch for one group.
type groupFeature struct {
	Name string `json:"name"`
	// GlobalEnabled is the feature's own switch.
	GlobalEnabled bool `json:"global_enabled"`
	// GroupOverride is what the group says, or null when it says nothing.
	//
	// Three states rather than two, because "this group turned it off" and "this
	// group did not say, so it takes the default" are different answers, and a
	// console that showed the second as the first would make an operator think
	// they had decided something they had not.
	GroupOverride *bool `json:"group_override"`
	// Effective is what actually happens.
	Effective bool `json:"effective"`
}

// pendingResponse is the queue of members waiting to verify.
type pendingResponse struct {
	Pending []pendingEntry `json:"pending"`
}

// pendingEntry is one member being held.
type pendingEntry struct {
	Token        string `json:"token"`
	GroupOpenID  string `json:"group_openid"`
	MemberOpenID string `json:"member_openid"`
	JoinedAt     int64  `json:"joined_at"`
	// Deadline is when the hold ends if nothing is done, in Unix seconds.
	Deadline int64 `json:"deadline"`
	// HeldUntil is when a real mute ends, or zero for a hold that is not one.
	HeldUntil int64 `json:"held_until"`
	// EscalatedAt is when this hold was turned into a real mute, or zero.
	EscalatedAt int64 `json:"escalated_at"`
	// Reported is whether the deadline has already been acted on.
	Reported bool `json:"reported"`
	// Settings is the feature's own configuration for this hold, as it was
	// written down when the member joined.
	//
	// Kept opaque in the column and decoded here, because the console needs to
	// say which kind of hold this is and the answer is in the feature's terms,
	// not the store's.
	Settings string `json:"settings"`
	// MuteMode is the decoded form of what Settings says about silencing:
	// "mute" or "pseudo", empty when it cannot be read.
	MuteMode string `json:"mute_mode"`
	// DeadlineHours is the configured deadline, zero when it cannot be read.
	DeadlineHours int `json:"deadline_hours"`
	// LeftToTalk is how many more messages a member may send under a pseudo-mute
	// before it becomes a real one, zero when that is not configured.
	LeftToTalk int `json:"left_to_talk"`
	// OnlineVerify is how this member is asked to prove they are human: through
	// the browser service ("true"), or with the button in the group ("false").
	//
	// A pointer because the settings column is the feature's own structure and a
	// hold written by an older version has no such field in it: null says "this
	// record does not say", which is a different thing from "the button".
	//
	// It is the setting frozen when the member joined, not the group's current
	// one: a hold that was taken before the service was asked for keeps the button
	// it was given.
	OnlineVerify *bool `json:"online_verify"`
}

// memberEventsResponse is the log of who came and went.
type memberEventsResponse struct {
	Events []memberEvent `json:"events"`
	// Scanned is how many rows were read before filtering.
	Scanned int `json:"scanned"`
	// Truncated reports that the store had at least as many rows as were asked
	// for, so older ones exist that were not read.
	Truncated bool `json:"truncated"`
}

// memberEvent is one arrival or departure.
type memberEvent struct {
	GroupOpenID  string `json:"group_openid"`
	GroupName    string `json:"group_name"`
	GroupQQID    int64  `json:"group_qq_id"`
	MemberOpenID string `json:"member_openid"`
	// Kind is "join" or "leave".
	//
	// There is deliberately no way to tell a member who left from one who was
	// removed: the platform reports both as a departure with nobody named in it,
	// and a log that guessed would be worse than one that says less.
	Kind    string `json:"kind"`
	EventAt int64  `json:"event_at"`
}

// judgementsResponse is the record of what was judged.
type judgementsResponse struct {
	Judgements []judgement `json:"judgements"`
	// Truncated reports that some group answered with as many rows as it will
	// return, so the list is the newest of what exists rather than all of it.
	Truncated bool `json:"truncated"`
	// GroupRequired reports that no group was named and the configuration names
	// no groups, so the question could not be asked at all.
	//
	// The store answers this question one group at a time -- there is no query
	// over every group -- so a deployment that manages every group it is in has
	// no list of them to walk, and says so instead of returning nothing.
	GroupRequired bool `json:"group_required,omitempty"`
}

// judgement is one verdict and what followed.
type judgement struct {
	ID            string `json:"id"`
	GroupOpenID   string `json:"group_openid"`
	SubjectOpenID string `json:"subject_openid"`
	// ReporterOpenID is who asked for the judgement, empty when nobody did.
	ReporterOpenID string `json:"reporter_openid"`
	// Category is the configured name of what was found, empty for nothing.
	Category string `json:"category"`
	// CategoryLabel is that category in the words a group is told, taken from
	// the moderation feature rather than translated here.
	CategoryLabel string `json:"category_label"`
	// Verdict is "ok", "violation" or "error".
	//
	// The third is not a clean verdict: "nothing was found" and "nobody looked"
	// are different facts, and a console that showed them alike would hide every
	// outage behind a calm row.
	Verdict string `json:"verdict"`
	// Model names what answered, empty when nothing was asked.
	Model string `json:"model"`
	// MessageIDs are the cached messages that were sent for judgement.
	MessageIDs []string `json:"message_ids"`
	// Reason is the model's explanation, kept for administrators and never for
	// the group.
	Reason string `json:"reason"`
	// Reasoning is the model's chain of thought when one was kept.
	Reasoning string `json:"reasoning"`
	// Action is what was done, in the words shown to the group.
	Action string `json:"action"`
	// MuteSeconds is how long the punishment lasted, zero when there was none.
	MuteSeconds int64 `json:"mute_seconds"`
	// RecallReason says why the messages were or were not taken back.
	RecallReason string `json:"recall_reason"`
	// Recalls is what happened to each message that was to be taken back.
	Recalls   []recall `json:"recalls"`
	CreatedAt int64    `json:"created_at"`
}

// recall is one message's fate.
type recall struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Recalled reports whether it was actually taken back.
	Recalled bool `json:"recalled"`
	// Reason says why not, when it was not.
	Reason string `json:"reason"`
}

// blacklistResponse is the list of barred applicants.
type blacklistResponse struct {
	Entries []blacklistEntry `json:"entries"`
}

// blacklistEntry is one barred identity.
type blacklistEntry struct {
	ID string `json:"id"`
	// GroupOpenID is the group that made the decision, or empty for an entry
	// that bars every group.
	GroupOpenID  string `json:"group_openid"`
	MemberOpenID string `json:"member_openid"`
	UnionOpenID  string `json:"union_openid"`
	Reason       string `json:"reason"`
	AddedAt      int64  `json:"added_at"`
	AddedBy      string `json:"added_by"`
	// ExpiresAt is zero for an entry that never expires.
	ExpiresAt int64 `json:"expires_at"`
}

// highRiskResponse is the list of members whose messages are judged as they
// arrive.
type highRiskResponse struct {
	Watches []highRiskWatch `json:"watches"`
}

// highRiskWatch is one marked member.
type highRiskWatch struct {
	ID string `json:"id"`
	// GroupOpenID is the group that marked them, or empty for a mark that
	// applies everywhere.
	GroupOpenID  string `json:"group_openid"`
	MemberOpenID string `json:"member_openid"`
	Reason       string `json:"reason"`
	AddedAt      int64  `json:"added_at"`
	AddedBy      string `json:"added_by"`
	// ExpiresAt is when the mark ends. There is no zero: every mark ends, so
	// that a list of them stays a decision somebody made rather than a fact
	// nobody remembers deciding.
	ExpiresAt int64 `json:"expires_at"`
}

// broadcastsResponse is the record of what an administrator had the bot post.
type broadcastsResponse struct {
	Broadcasts []broadcast `json:"broadcasts"`
	// Unrestricted reports that no group was named and the file names none, so
	// there is no list of groups to read the record for.
	Unrestricted bool `json:"unrestricted,omitempty"`
}

// broadcast is one notice and where it went.
type broadcast struct {
	Token string `json:"token"`
	// SenderOpenID is who asked for it. It is the field the record exists for:
	// the group reads a message that does not say who wrote it, and this is
	// where that is still answerable.
	SenderOpenID string   `json:"sender_openid"`
	Anonymous    bool     `json:"anonymous"`
	Markdown     bool     `json:"markdown"`
	Content      string   `json:"content"`
	SentAt       int64    `json:"sent_at"`
	Targets      []target `json:"targets"`
}

// target is one group a notice reached.
type target struct {
	GroupOpenID string `json:"group_openid"`
	MessageID   string `json:"message_id"`
}

// commandsResponse is the command table.
type commandsResponse struct {
	// Prefix is what the commands are typed with.
	Prefix   string         `json:"prefix"`
	Commands []commandShown `json:"commands"`
}

// commandShown is one command.
//
// It is built from the feature's own table rather than from a list kept here,
// because the table is what the dispatcher runs, what the help shows and what
// the platform's panel publishes: a second copy would go stale exactly where
// nobody looks -- in a menu nobody re-reads.
type commandShown struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Usage   string   `json:"usage"`
	Desc    string   `json:"desc"`
	// Scenes says where it is offered: "group", "private" or both.
	Scenes []string `json:"scenes"`
	// Audience is who may run it: "everyone", "admins" or "whois".
	Audience string `json:"audience"`
}

// configResponse is the configuration in force, with every secret left out.
type configResponse struct {
	Path string `json:"path"`
	Bot  struct {
		QQ     int64         `json:"qq"`
		Groups []groupOnlyID `json:"groups"`
		// CredentialsConfigured reports that *something* is set, never what.
		CredentialsConfigured bool `json:"credentials_configured"`
	} `json:"bot"`
	Log      logInfo      `json:"log"`
	Database databaseInfo `json:"database"`
	Redis    redisInfo    `json:"redis"`
	// Features lists the features the file names and the groups each of them
	// overrides, so the console can say where a setting lives.
	Features []featureConfig `json:"features"`
	Admin    adminInfo       `json:"admin"`
	// HumanVerification is the online verification service as configured, with the
	// two tokens reported as present or absent and never as values.
	HumanVerification humanVerificationConfig `json:"human_verification"`
}

// humanVerificationConfig is the human_verification section, redacted.
type humanVerificationConfig struct {
	// Configured reports whether the section names a service at all. False is the
	// answer that decides everything else: no group can verify online, and one that
	// asks for it is refused at startup rather than holding members with no way out.
	Configured bool   `json:"configured"`
	BaseURL    string `json:"base_url"`
	// Cdata is the business marker every link is signed with, resolved rather than
	// raw: empty in the file means the marker this bot ships with, and an empty
	// string here would hide which one is actually in use.
	Cdata string `json:"cdata"`
	// TTLSeconds is how long one link stays usable, already resolved to the
	// service's default when the file leaves the field out.
	TTLSeconds int `json:"ttl_seconds"`
	// TimeoutSeconds is how long one call to the service may take, resolved.
	TimeoutSeconds int `json:"timeout_seconds"`
	// IssueTokenConfigured and WSTokenConfigured report presence, never values:
	// whoever holds the first can issue a link for anything, and the second is what
	// the results arrive on.
	IssueTokenConfigured bool `json:"issue_token_configured"`
	WSTokenConfigured    bool `json:"ws_token_configured"`
}

// groupOnlyID is a group named by the file.
type groupOnlyID struct {
	OpenID    string `json:"openid"`
	QQGroupID int64  `json:"qq_group_id"`
}

// logInfo is the logging section.
type logInfo struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// featureConfig is one feature as the file writes it.
type featureConfig struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Groups are the groups this feature overrides.
	Groups []string `json:"groups"`
}

// adminInfo is the console's own settings.
type adminInfo struct {
	Listen string `json:"listen"`
	// TokenConfigured reports whether a token is set, never the token.
	TokenConfigured bool   `json:"token_configured"`
	WebDir          string `json:"web_dir,omitempty"`
}

// reloadResponse answers a reload request.
type reloadResponse struct {
	Reloaded bool `json:"reloaded"`
}

// pendingFrom converts one stored hold.
//
// The settings column is opaque to the store and is the verification feature's
// own structure, so it is decoded as that very type rather than as a copy that
// would drift the first time a setting was renamed.
func pendingFrom(entry store.Pending) pendingEntry {
	out := pendingEntry{
		Token:        entry.Token,
		GroupOpenID:  entry.GroupOpenID,
		MemberOpenID: entry.MemberOpenID,
		JoinedAt:     entry.JoinedAt,
		Deadline:     entry.Deadline,
		HeldUntil:    entry.HeldUntil,
		EscalatedAt:  entry.EscalatedAt,
		Reported:     entry.Reported,
		Settings:     entry.Settings,
	}
	if strings.TrimSpace(entry.Settings) == "" {
		return out
	}
	var settings joinverify.Settings
	if err := json.Unmarshal([]byte(entry.Settings), &settings); err != nil {
		// A row written by an older version, or a column somebody edited. The
		// hold is still real, so it is returned with what could be read rather
		// than dropped: a member being held by a row the console refuses to show
		// is a member nobody can release.
		return out
	}
	out.MuteMode = settings.MuteMode
	out.DeadlineHours = settings.DeadlineHours
	// Read out by address, because null and false are different answers: a record
	// that does not say how the member is asked to verify must not be shown as one
	// that says "the button".
	out.OnlineVerify = &settings.OnlineVerify
	if settings.MuteMode == joinverify.MuteModePseudo && settings.PseudoMuteAfter > 0 {
		out.LeftToTalk = settings.PseudoMuteAfter
	}
	return out
}

// judgementFrom converts one stored verdict.
//
// labelFor is the moderation feature's own answer to "what is this category
// called", and may be nil when that feature is not running -- in which case the
// category is shown as written rather than invented.
func judgementFrom(entry store.Judgement, labelFor func(string) string) judgement {
	out := judgement{
		ID:             entry.ID,
		GroupOpenID:    entry.GroupOpenID,
		SubjectOpenID:  entry.SubjectOpenID,
		ReporterOpenID: entry.ReporterOpenID,
		Category:       entry.Category,
		CategoryLabel:  entry.Category,
		Verdict:        entry.Verdict,
		Model:          entry.Model,
		MessageIDs:     entry.MessageIDs,
		Reason:         entry.Reason,
		Reasoning:      entry.Reasoning,
		Action:         entry.Action,
		MuteSeconds:    entry.MuteSeconds,
		RecallReason:   entry.RecallReason,
		CreatedAt:      entry.CreatedAt,
	}
	if out.MessageIDs == nil {
		out.MessageIDs = []string{}
	}
	if labelFor != nil && entry.Category != "" {
		if label := labelFor(entry.Category); label != "" {
			out.CategoryLabel = label
		}
	}
	out.Recalls = make([]recall, 0, len(entry.Recalls))
	for _, one := range entry.Recalls {
		out.Recalls = append(out.Recalls, recall{
			ID:       one.ID,
			Text:     one.Text,
			Recalled: one.Recalled,
			Reason:   one.Reason,
		})
	}
	return out
}

// scenesOf says where a command is offered.
func scenesOf(def command.Def) []string {
	var scenes []string
	seenGroup, seenPrivate := false, false
	for _, placement := range def.Panels {
		if placement.Scene&command.InGroup != 0 && !seenGroup {
			seenGroup = true
			scenes = append(scenes, "group")
		}
		if placement.Scene&command.InPrivate != 0 && !seenPrivate {
			seenPrivate = true
			scenes = append(scenes, "private")
		}
	}
	if scenes == nil {
		scenes = []string{}
	}
	return scenes
}

// audienceOf says who may run a command.
func audienceOf(audience command.Audience) string {
	switch audience {
	case command.Admins:
		return "admins"
	case command.Whois:
		return "whois"
	default:
		return "everyone"
	}
}

// seconds renders a time as Unix seconds, and the zero time as zero.
//
// The zero time is not 1970: a column holding it means "never", and encoding it
// as a real instant would put a 1970 date in front of an operator where the
// answer is "nobody set one".
func seconds(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.Unix()
}

// verificationActionResponse is what happened to one waiting member.
//
// It reports one field per thing that really happened rather than a single
// "success": a rejection can take the member out of the group and fail to bar
// them, and an operator who is told only "done" would leave the list half
// updated without knowing it.
type verificationActionResponse struct {
	Token        string `json:"token"`
	GroupOpenID  string `json:"group_openid"`
	MemberOpenID string `json:"member_openid"`
	// Action is what was asked for: resend, approve or reject.
	Action string `json:"action"`
	// Message is the sentence to show, written for the person who pressed it.
	Message string `json:"message"`
	// Blacklisted is meaningful for reject: whether the list write happened.
	// Nothing reports a removal, because nothing removes: the bot cannot kick,
	// and the sentence in Message says so.
	Blacklisted bool `json:"blacklisted,omitempty"`
	// Pending is how many members are still waiting, counted after the action.
	Pending int `json:"pending"`
}

// logsResponse is the newest lines of the log, as far as the buffer still holds
// them.
//
// Everything that qualifies the answer is in the answer: how many records the
// buffer holds, how many have fallen out of it, and which level filter was
// actually applied. A log view that could not say "these are the newest lines and
// there used to be more" would be a log view that lies by omission.
type logsResponse struct {
	Records []logEntry `json:"records"`
	// Level is the filter that was applied, in capitals. It is echoed because a
	// request may ask for a level this side does not know, and the caller should
	// see what it got rather than what it asked for.
	Level string `json:"level"`
	// Capacity is how many records the buffer holds at once.
	Capacity int `json:"capacity"`
	// Kept is how many it holds now.
	Kept int `json:"kept"`
	// Dropped is how many have fallen out of the buffer since the process
	// started, because it was full.
	Dropped uint64 `json:"dropped"`
	// NextSeq is the sequence number of the newest record, or 0 when the buffer
	// is empty. A caller that reconnects asks for `since=NextSeq` and misses
	// nothing the buffer still has.
	NextSeq uint64 `json:"next_seq"`
}

// logEntry is one line.
type logEntry struct {
	// Seq is the sequence number: it counts from 1 and never repeats, so a
	// stream can be resumed without repeating or skipping a line.
	Seq     uint64 `json:"seq"`
	Time    int64  `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
	// Attrs are the structured fields of the line, already rendered to text and
	// in the order they were logged.
	Attrs []logAttr `json:"attrs"`
}

// logAttr is one field of a line.
type logAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// logEntriesOf converts captured records into the shapes above.
func logEntriesOf(records []logring.Record) []logEntry {
	out := make([]logEntry, 0, len(records))
	for _, record := range records {
		attrs := make([]logAttr, 0, len(record.Attrs))
		for _, attr := range record.Attrs {
			attrs = append(attrs, logAttr{Key: attr.Key, Value: attr.Value})
		}
		out = append(out, logEntry{
			Seq:     record.Seq,
			Time:    record.Time.Unix(),
			Level:   record.Level.String(),
			Message: record.Message,
			Attrs:   attrs,
		})
	}
	return out
}
