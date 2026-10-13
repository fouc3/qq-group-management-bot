package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/features/admincmd"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinverify"
	"github.com/fouc3/qq-group-management-bot/internal/humanverify"
	"github.com/fouc3/qq-group-management-bot/internal/logring"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// The bounds this package puts on every answer.
//
// A console is read by one person, and every list it can ask for is a table that
// grows for as long as the bot runs. Each query is therefore bounded, and the
// response says when the bound was reached rather than pretending the list ended
// on its own.
const (
	// defaultLimit is how many rows are returned when no number is asked for.
	defaultLimit = 200
	// maxLimit is the most that may be asked for.
	maxLimit = 2000
	// overviewScan is how many rows the summary reads to build its counts.
	//
	// The store lists rows and does not count them, so a summary is a statement
	// about the newest overviewScan events rather than about all of them. That
	// is said in the answer, because a count an operator cannot check is one
	// they should not trust.
	overviewScan = 500
	// judgementScan is how many judgements one group's query answers with.
	//
	// It is not this package's number: the store's own query stops there, so
	// asking for more would be a promise this side cannot keep.
	judgementScan = 50
	// defaultWindow is how far back a window reaches when no other is asked for.
	defaultWindow = 24 * time.Hour
	// maxWindow caps what may be asked for, so that "since the beginning" cannot
	// be asked for by accident and answered with a full table scan.
	maxWindow = 90 * 24 * time.Hour
	// defaultLogLimit is how many log records are answered when no number is
	// asked for: about a screen and a half of reading.
	defaultLogLimit = 200
	// maxLogLimit is the most a caller may ask for. The buffer holds less than
	// this, so the number is a bound on the question rather than on the answer.
	maxLogLimit = 2000
)

// handleStatus answers what the bot is and what it is running.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.opts.Config.Current()
	redis := cfg.RedisConfig()

	response := statusResponse{
		Version:       version(),
		StartedAt:     s.opts.Started.Unix(),
		UptimeSeconds: int64(s.now().Sub(s.opts.Started).Seconds()),
		ConfigPath:    s.opts.ConfigPath,
		GroupsManaged: len(cfg.Bot.Groups),
		Database: databaseInfo{
			Driver:    cfg.Database.Driver,
			SizeBytes: databaseSize(cfg),
		},
		Redis: redisInfo{
			Addr:   redis.Addr,
			DB:     redis.DB,
			Prefix: redis.Prefix,
		},
		CommandPrefix: commandPrefix(cfg),
		JudgedModel:   judgedModel(cfg),
		Features:      s.featureStates(cfg),
	}
	response.HumanVerification = s.humanVerificationStatus(cfg)
	writeJSON(w, http.StatusOK, response)
}

// humanVerificationStatus is the online verification service's health.
//
// Asked of the service in force rather than worked out from the file, because the
// two answers differ in the way that matters: a deployment whose long connection is
// down cannot hand out a single link, so every group that verifies online is falling
// back to the button in the group -- and until somebody looks, it reads like a
// deployment that verifies online.
func (s *Server) humanVerificationStatus(cfg *config.Config) humanVerificationStatus {
	return humanVerificationStatus{
		Configured: cfg.HumanVerification.Configured(),
		Connected:  s.opts.HumanVerify != nil && s.opts.HumanVerify.HumanVerifyConnected(),
	}
}

// featureStates is every feature the file names, and whether it is running.
func (s *Server) featureStates(cfg *config.Config) []featureState {
	names := cfg.FeatureNames()
	sort.Strings(names)
	states := make([]featureState, 0, len(names))
	for _, name := range names {
		section, _ := cfg.Feature(name)
		on, err := feature.Enabled(name, section)
		if err != nil {
			// A section that cannot be parsed would have stopped the bot from
			// starting, so this is a file that changed since. Reported as off,
			// which is what it is: nothing is running from it.
			on = false
		}
		states = append(states, featureState{
			Name:    name,
			Enabled: on,
			Running: s.opts.Features != nil && s.opts.Features.Running(name),
		})
	}
	return states
}

// handleOverview answers the summary the first screen is built from.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.opts.Config.Current()
	now := s.now()
	since := s.sinceOf(r, now)

	response := overviewResponse{
		Since:  since.Unix(),
		Groups: len(cfg.Bot.Groups),
	}

	pending, err := s.opts.Store.Pending().Load(ctx, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response.Pending = len(pending)

	watches, err := s.opts.Store.Watches().List(ctx, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response.HighRisk = len(watches)

	blacklist, notes, err := s.blacklistCounts(ctx, cfg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response.Blacklist = blacklist
	response.Notes = append(response.Notes, notes...)

	events, err := s.opts.Store.MemberEvents().List(ctx, overviewScan)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response.MemberEvents.Scanned = len(events)
	for _, event := range events {
		if event.EventAt < since.Unix() {
			continue
		}
		response.MemberEvents.Total++
		if event.Kind == store.MemberJoined {
			response.MemberEvents.Joined++
		} else {
			response.MemberEvents.Left++
		}
	}
	if len(events) >= overviewScan {
		response.Notes = append(response.Notes,
			"成员事件只统计了最近 "+strconv.Itoa(overviewScan)+" 条，更早的没有读")
	}

	judgements, truncated, err := s.judgementsOf(ctx, groupIDs(cfg), since)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(cfg.Bot.Groups) == 0 {
		response.Notes = append(response.Notes,
			"bot.groups 为空时判定记录无法统计：判定表按群存储，没有全局查询")
	} else if truncated {
		response.Notes = append(response.Notes,
			"判定记录每群只读最近 "+strconv.Itoa(judgementScan)+" 条，可能还有更早的")
	}
	response.Judgements.Truncated = truncated
	for _, judge := range judgements {
		response.Judgements.Total++
		switch judge.Verdict {
		case store.JudgementViolation:
			response.Judgements.Violations++
		case store.JudgementError:
			response.Judgements.Errors++
		default:
			response.Judgements.OK++
		}
	}

	writeJSON(w, http.StatusOK, response)
}

// blacklistCounts counts the barred list without counting one entry twice.
//
// The store answers per group, and every group's answer includes the entries
// that bar all of them. Adding the answers up would report an entry that bars
// every group once per group, so the entries are counted by their own id.
func (s *Server) blacklistCounts(ctx context.Context,
	cfg *config.Config) (blacklistCounts, []string, error) {
	groups := groupIDs(cfg)
	if len(groups) == 0 {
		// No groups are named, so only the entries that bar every group can be
		// read. That is a real answer, and it is narrower than the question, so
		// it is said out loud.
		entries, err := s.opts.Store.Blacklist().List(ctx, "", defaultLimit)
		if err != nil {
			return blacklistCounts{}, nil, err
		}
		notes := []string{"bot.groups 为空，这里只统计了作用于所有群的黑名单条目"}
		if len(entries) >= defaultLimit {
			notes = append(notes, "黑名单只读了最近 "+strconv.Itoa(defaultLimit)+" 条")
		}
		return blacklistCounts{Total: len(entries)}, notes, nil
	}

	counts := blacklistCounts{
		ByGroup: make([]blacklistPerGroup, 0, len(groups)),
	}
	seen := map[string]bool{}
	truncated := false
	for _, group := range groups {
		entries, err := s.opts.Store.Blacklist().List(ctx, group, defaultLimit)
		if err != nil {
			return blacklistCounts{}, nil, err
		}
		if len(entries) >= defaultLimit {
			truncated = true
		}
		counts.ByGroup = append(counts.ByGroup, blacklistPerGroup{
			GroupOpenID: group,
			Count:       len(entries),
		})
		for _, entry := range entries {
			seen[entry.ID] = true
		}
	}
	counts.Total = len(seen)

	var notes []string
	if truncated {
		notes = append(notes,
			"黑名单每群只读了最近 "+strconv.Itoa(defaultLimit)+" 条，可能还有更多")
	}
	return counts, notes, nil
}

// judgementsOf reads every named group's judgements since a moment.
//
// One group at a time, because that is the only question the store answers: there
// is no query over every group. A caller with no groups to walk is told so by the
// handler rather than handed an empty list.
//
// RecentInGroup rather than Recent: the latter is scoped to a member as well, and
// a judgement that found something always names the member it was about, so
// asking it for the group's whole record answers with the judgements that never
// happened.
func (s *Server) judgementsOf(ctx context.Context, groups []string,
	since time.Time) ([]store.Judgement, bool, error) {
	limit := judgementScan
	var all []store.Judgement
	truncated := false
	for _, group := range groups {
		entries, err := s.opts.Store.Judgements().RecentInGroup(ctx, group, since, limit)
		if err != nil {
			return nil, false, err
		}
		// Reaching the bound means there may be more rather than that there were
		// exactly this many.
		if len(entries) >= limit {
			truncated = true
		}
		all = append(all, entries...)
	}
	return all, truncated, nil
}

// handleGroups answers the managed groups and what applies to each of them.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	cfg := s.opts.Config.Current()
	names := cfg.FeatureNames()
	sort.Strings(names)

	response := groupsResponse{Unrestricted: len(cfg.Bot.Groups) == 0}
	response.Groups = make([]groupResponse, 0, len(cfg.Bot.Groups))
	for _, group := range cfg.Bot.Groups {
		entry := groupResponse{OpenID: group.OpenID, QQGroupID: group.QQGroupID}
		entry.Features = make([]groupFeature, 0, len(names))
		for _, name := range names {
			section, _ := cfg.Feature(name)
			on, err := feature.Enabled(name, section)
			if err != nil {
				on = false
			}
			override := groupOverride(section, group.OpenID)
			effective := on
			if override != nil {
				effective = *override
			}
			entry.Features = append(entry.Features, groupFeature{
				Name:          name,
				GlobalEnabled: on,
				GroupOverride: override,
				Effective:     effective,
			})
		}
		response.Groups = append(response.Groups, entry)
	}
	writeJSON(w, http.StatusOK, response)
}

// groupOverride reads a group's own switch out of a feature's section.
//
// The shape is the one every feature that lets a group differ uses -- groups
// keyed by openid, each with an enabled -- and it is read as raw YAML here
// rather than through each feature's own type, because the console asks the same
// question of all of them and a per-feature answer would be seven copies of it.
func groupOverride(section yaml.Node, groupOpenID string) *bool {
	var probe struct {
		Groups map[string]struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"groups"`
	}
	if err := section.Decode(&probe); err != nil {
		return nil
	}
	entry, found := probe.Groups[groupOpenID]
	if !found {
		return nil
	}
	return entry.Enabled
}

// handlePending answers the queue of members waiting to verify.
func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	group := strings.TrimSpace(r.URL.Query().Get("group"))

	entries, err := s.opts.Store.Pending().Load(ctx, s.now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response := pendingResponse{Pending: make([]pendingEntry, 0, len(entries))}
	for _, entry := range entries {
		if group != "" && entry.GroupOpenID != group {
			continue
		}
		response.Pending = append(response.Pending, pendingFrom(entry))
	}
	writeJSON(w, http.StatusOK, response)
}

// handleMemberEvents answers the log of who came and went.
func (s *Server) handleMemberEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()
	limit := limitOf(r, defaultLimit)
	group := strings.TrimSpace(query.Get("group"))
	kind := strings.TrimSpace(query.Get("kind"))
	var since int64
	if raw := strings.TrimSpace(query.Get("since")); raw != "" {
		parsed, ok := parseUnix(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_since", "since 必须是 Unix 秒")
			return
		}
		since = parsed
	}

	// The store lists the newest rows and filters nothing, so what is asked of it
	// is a number of rows and the narrowing happens here. Reading more than the
	// caller asked for is deliberate: a filter that dropped nine rows out of ten
	// would otherwise answer with a page that looked short rather than filtered.
	scan := limit
	if group != "" || kind != "" || since != 0 {
		scan = min(limit*10, maxLimit)
	}
	events, err := s.opts.Store.MemberEvents().List(ctx, scan)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	response := memberEventsResponse{
		Events:    make([]memberEvent, 0, len(events)),
		Scanned:   len(events),
		Truncated: len(events) >= scan,
	}
	for _, event := range events {
		if len(response.Events) >= limit {
			break
		}
		if group != "" && event.GroupOpenID != group {
			continue
		}
		if kind != "" && event.Kind != kind {
			continue
		}
		if since != 0 && event.EventAt < since {
			continue
		}
		response.Events = append(response.Events, memberEvent{
			GroupOpenID:  event.GroupOpenID,
			GroupName:    event.GroupName,
			GroupQQID:    event.GroupQQID,
			MemberOpenID: event.MemberOpenID,
			Kind:         event.Kind,
			EventAt:      event.EventAt,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// handleJudgements answers the record of what was judged.
func (s *Server) handleJudgements(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.opts.Config.Current()
	limit := limitOf(r, 50)
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	since := s.sinceOf(r, s.now())

	groups := []string{group}
	if group == "" {
		groups = groupIDs(cfg)
		if len(groups) == 0 {
			// Not an error: a deployment that manages every group it is in has
			// no list to walk, and saying so is more useful than an empty list
			// that looks like "nothing was judged".
			writeJSON(w, http.StatusOK, judgementsResponse{
				Judgements:    []judgement{},
				GroupRequired: true,
			})
			return
		}
	}

	labelFor := s.categoryLabeller()
	all, truncated, err := s.judgementsOf(ctx, groups, since)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	if len(all) > limit {
		all = all[:limit]
	}

	response := judgementsResponse{
		Judgements: make([]judgement, 0, len(all)),
		Truncated:  truncated,
	}
	for _, entry := range all {
		response.Judgements = append(response.Judgements, judgementFrom(entry, labelFor))
	}
	writeJSON(w, http.StatusOK, response)
}

// categoryLabeller asks the moderation feature what a category is called.
//
// The label is the word a group was told, and the configuration that holds it
// belongs to that feature, so it is asked rather than looked up in a second copy.
// A deployment without moderation running gets nil, and the category is then
// shown as written.
func (s *Server) categoryLabeller() func(string) string {
	if s.opts.Features == nil {
		return nil
	}
	instance := s.opts.Features.Instance("moderation")
	if instance == nil {
		return nil
	}
	labeler, ok := instance.(feature.Moderation)
	if !ok {
		return nil
	}
	return labeler.LabelFor
}

// handleBlacklist answers the list of barred applicants.
func (s *Server) handleBlacklist(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.opts.Config.Current()
	limit := limitOf(r, defaultLimit)
	group := strings.TrimSpace(r.URL.Query().Get("group"))

	groups := []string{group}
	if group == "" {
		groups = groupIDs(cfg)
		if len(groups) == 0 {
			// No groups named: only the entries that bar every group can be
			// read, which is a real answer rather than a missing one.
			groups = []string{""}
		}
	}

	seen := map[string]bool{}
	response := blacklistResponse{Entries: []blacklistEntry{}}
	for _, one := range groups {
		entries, err := s.opts.Store.Blacklist().List(ctx, one, limit)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, entry := range entries {
			// An entry that bars every group is in every group's answer.
			if seen[entry.ID] {
				continue
			}
			seen[entry.ID] = true
			response.Entries = append(response.Entries, blacklistEntry{
				ID:           entry.ID,
				GroupOpenID:  entry.GroupOpenID,
				MemberOpenID: entry.MemberOpenID,
				UnionOpenID:  entry.UnionOpenID,
				Reason:       entry.Reason,
				AddedAt:      entry.AddedAt,
				AddedBy:      entry.AddedBy,
				ExpiresAt:    entry.ExpiresAt,
			})
			if len(response.Entries) >= limit {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// handleHighRisk answers the members whose messages are judged as they arrive.
func (s *Server) handleHighRisk(w http.ResponseWriter, r *http.Request) {
	watches, err := s.opts.Store.Watches().List(r.Context(), s.now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response := highRiskResponse{Watches: make([]highRiskWatch, 0, len(watches))}
	for _, watch := range watches {
		response.Watches = append(response.Watches, highRiskWatch{
			ID:           watch.ID,
			GroupOpenID:  watch.GroupOpenID,
			MemberOpenID: watch.MemberOpenID,
			Reason:       watch.Reason,
			AddedAt:      watch.AddedAt,
			AddedBy:      watch.AddedBy,
			ExpiresAt:    watch.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// handleBroadcasts answers the record of what an administrator had posted.
func (s *Server) handleBroadcasts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.opts.Config.Current()
	limit := limitOf(r, 50)

	groups := groupIDs(cfg)
	if len(groups) == 0 {
		// The record is read by the groups a notice reached, and with no groups
		// named there is nothing to read it by.
		writeJSON(w, http.StatusOK, broadcastsResponse{
			Broadcasts:   []broadcast{},
			Unrestricted: true,
		})
		return
	}

	entries, err := s.opts.Store.Broadcasts().ListByGroups(ctx, groups, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	response := broadcastsResponse{Broadcasts: make([]broadcast, 0, len(entries))}
	for _, entry := range entries {
		one := broadcast{
			Token:        entry.Token,
			SenderOpenID: entry.SenderOpenID,
			Anonymous:    entry.Anonymous,
			Markdown:     entry.Markdown,
			Content:      entry.Content,
			SentAt:       entry.SentAt,
			Targets:      make([]target, 0, len(entry.Targets)),
		}
		for _, where := range entry.Targets {
			one.Targets = append(one.Targets, target{
				GroupOpenID: where.GroupOpenID,
				MessageID:   where.MessageID,
			})
		}
		response.Broadcasts = append(response.Broadcasts, one)
	}
	writeJSON(w, http.StatusOK, response)
}

// handleCommands answers the command table.
func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	cfg := s.opts.Config.Current()
	prefix := commandPrefix(cfg)

	response := commandsResponse{Prefix: prefix, Commands: []commandShown{}}
	if s.opts.Features == nil {
		writeJSON(w, http.StatusOK, response)
		return
	}
	// The commands are not one feature's. The table is kept by admin_commands, but
	// the commands in it are offered by whoever answers them -- /whois by the table's
	// owner, the queue's own by join verification -- so this asks everything that is
	// running. Asking the keeper alone is why this answer used to be an empty list
	// on a bot whose command table was full.
	for _, def := range s.opts.Features.CommandDefs() {
		response.Commands = append(response.Commands, commandShown{
			Name:     def.Name,
			Aliases:  aliasesOf(def),
			Usage:    def.Show(prefix),
			Desc:     def.Desc,
			Scenes:   scenesOf(def),
			Audience: audienceOf(def.Audience),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// handleConfig answers the configuration in force, with every secret left out.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.opts.Config.Current()
	redis := cfg.RedisConfig()

	response := configResponse{Path: s.opts.ConfigPath}
	response.Bot.QQ = cfg.Bot.QQ
	response.Bot.CredentialsConfigured = strings.TrimSpace(cfg.Bot.AccessToken) != "" ||
		(strings.TrimSpace(cfg.Bot.AppID) != "" && strings.TrimSpace(cfg.Bot.ClientSecret) != "")
	response.Bot.Groups = make([]groupOnlyID, 0, len(cfg.Bot.Groups))
	for _, group := range cfg.Bot.Groups {
		response.Bot.Groups = append(response.Bot.Groups, groupOnlyID{
			OpenID:    group.OpenID,
			QQGroupID: group.QQGroupID,
		})
	}
	response.Log = logInfo{Level: cfg.Log.Level, Format: cfg.Log.Format}
	response.Database = databaseInfo{
		Driver:    cfg.Database.Driver,
		SizeBytes: databaseSize(cfg),
	}
	response.Redis = redisInfo{Addr: redis.Addr, DB: redis.DB, Prefix: redis.Prefix}
	// The business marker is resolved rather than passed through: an empty string in
	// the file means the marker this bot ships with, and a reader has to be able to
	// see which marker their links are actually signed with -- changing it throws
	// away the results of every link issued under the old one.
	human := cfg.HumanVerification
	marker := strings.TrimSpace(human.Cdata)
	if marker == "" {
		marker = humanverify.BusinessMarker
	}
	response.HumanVerification = humanVerificationConfig{
		Configured:           human.Configured(),
		BaseURL:              human.BaseURL,
		Cdata:                marker,
		TTLSeconds:           human.TTLSeconds,
		TimeoutSeconds:       human.TimeoutSeconds,
		IssueTokenConfigured: strings.TrimSpace(human.IssueToken) != "",
		WSTokenConfigured:    strings.TrimSpace(human.WSToken) != "",
	}
	admin := s.admin()
	response.Admin = adminInfo{
		Listen:          s.listenAddr(admin),
		TokenConfigured: strings.TrimSpace(admin.Token) != "",
		WebDir:          admin.WebDir,
	}

	names := cfg.FeatureNames()
	sort.Strings(names)
	response.Features = make([]featureConfig, 0, len(names))
	for _, name := range names {
		section, _ := cfg.Feature(name)
		on, err := feature.Enabled(name, section)
		if err != nil {
			on = false
		}
		response.Features = append(response.Features, featureConfig{
			Name:    name,
			Enabled: on,
			Groups:  sectionGroups(section),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// sectionGroups lists the groups a feature's section overrides.
func sectionGroups(section yaml.Node) []string {
	var probe struct {
		Groups map[string]yaml.Node `yaml:"groups"`
	}
	if err := section.Decode(&probe); err != nil {
		return []string{}
	}
	groups := make([]string, 0, len(probe.Groups))
	for name := range probe.Groups {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	return groups
}

// handleReload reads the configuration file again.
//
// It is the one thing here that is not a read, and it is still not a change: it
// adopts what the file already says. The answer is the point -- a console that
// said "done" whenever the button was pressed would be wrong exactly when it
// mattered, which is when the file has just been edited and does not parse.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.opts.Reload == nil {
		writeError(w, http.StatusServiceUnavailable, "reload_unavailable",
			"这个部署没有可用的重载入口")
		return
	}
	if err := s.opts.Reload.Reload("管理后台"); err != nil {
		// 500 rather than 400: the request was fine and the file was not. What
		// the operator needs is what the file failed on, so it is passed
		// through rather than replaced with "reload failed".
		writeError(w, http.StatusInternalServerError, "reload_failed",
			"配置没有生效，仍在使用上一份："+err.Error())
		return
	}
	s.opts.Logger.Info("the configuration was reloaded from the management console")
	writeJSON(w, http.StatusOK, reloadResponse{Reloaded: true})
}

// handleLogs answers with the newest lines the buffer still holds.
//
// A snapshot, not a stream: this is what a page loads before it subscribes, and
// what it asks for again when the stream breaks. `since` makes that second use
// cheap -- the caller says which sequence number it already has.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	level, levelName := levelOf(r)
	limit := limitOf(r, defaultLogLimit)
	records := s.opts.Logs.Tail(limit, level)
	// Asked for only if the parameter is there: `after=0` means "everything the
	// buffer has", and an absent one means "the snapshot is all I want".
	if seq, ok := seqOf(r, "after"); ok {
		records = newerThan(records, seq)
	}
	writeJSON(w, http.StatusOK, logsResponse{
		Records:  logEntriesOf(records),
		Level:    levelName,
		Capacity: s.opts.Logs.Capacity(),
		Kept:     s.opts.Logs.Kept(),
		Dropped:  s.opts.Logs.Dropped(),
		NextSeq:  s.opts.Logs.Newest(),
	})
}

// handleLogStream answers with the log as it is written.
//
// Server-sent events rather than a web socket: the traffic goes one way, and a
// browser reconnects an event stream by itself. The connection is held open until
// the caller leaves or the bot stops, which is why nothing here may block on a
// reader that stopped reading -- the buffer drops records for a slow reader and
// keeps the log itself whole.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if s.opts.Logs == nil {
		writeError(w, http.StatusServiceUnavailable, "logs_unavailable",
			"这个部署没有保留日志缓冲区，无法提供日志流")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_streaming",
			"当前服务实现不支持流式响应")
		return
	}

	level, levelName := levelOf(r)
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	// A proxy that buffers would hold the stream until it ended, which for a log
	// stream is never. Said in the header rather than assumed.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// How long a browser should wait before reconnecting, and the level filter
	// this connection is applying.
	fmt.Fprintf(w, "retry: 3000\n")
	fmt.Fprintf(w, "event: open\ndata: {\"level\":%q}\n\n", levelName)
	flusher.Flush()

	sub := s.opts.Logs.Subscribe(level)
	defer sub.Close()

	// A caller that reconnects says what it already has, and gets the gap from
	// the buffer before the live records: reconnecting must not leave a hole.
	// `after=0` asks for everything the buffer holds, which is what a page that
	// has just opened does when it wants the stream to be its only source.
	if seq, ok := seqOf(r, "after"); ok {
		for _, record := range newerThan(s.opts.Logs.Tail(maxLogLimit, level), seq) {
			if !writeLogEvent(w, record) {
				return
			}
		}
		flusher.Flush()
	}

	// A comment every so often: an idle connection through a proxy is a
	// connection that gets closed, and a log stream is idle whenever the bot is
	// quiet.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case record, open := <-sub.C:
			if !open {
				return
			}
			if !writeLogEvent(w, record) {
				return
			}
			flusher.Flush()
		}
	}
}

// writeLogEvent writes one record as an event, reporting whether it went out.
func writeLogEvent(w io.Writer, record logring.Record) bool {
	entries := logEntriesOf([]logring.Record{record})
	payload, err := json.Marshal(entries[0])
	if err != nil {
		// A record that cannot be encoded is a field this package cannot
		// render; skipping it keeps the stream alive for the rest.
		return true
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", record.Seq, payload)
	return err == nil
}

// newerThan keeps the records newer than a sequence number.
//
// Named apart from the `since` parameter the other endpoints take, which is a
// Unix second: the same query key meaning two different things in one API is how
// a caller ends up asking the wrong question and getting a plausible answer.
func newerThan(records []logring.Record, seq uint64) []logring.Record {
	kept := make([]logring.Record, 0, len(records))
	for _, record := range records {
		if record.Seq > seq {
			kept = append(kept, record)
		}
	}
	return kept
}

// levelOf reads the level filter a request asks for.
//
// An unknown level is INFO rather than an error: the answer says which filter was
// applied, so a caller that misspelled one can see that it did not take effect.
func levelOf(r *http.Request) (slog.Level, string) {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level"))) {
	case "debug":
		return slog.LevelDebug, "DEBUG"
	case "warn", "warning":
		return slog.LevelWarn, "WARN"
	case "error":
		return slog.LevelError, "ERROR"
	default:
		return slog.LevelInfo, "INFO"
	}
}

// handleVerificationAction is the three things an operator does to somebody who
// is waiting to verify: say the prompt again, let them in, or turn them away.
//
// One handler for all three because they share everything that can go wrong: the
// feature may not be running, the token may name nobody, and the action may be
// refused. Only the last step differs, and it is the feature's to perform -- this
// side resolves who the token is about and reports what the feature answered.
func (s *Server) handleVerificationAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		token := strings.TrimSpace(r.PathValue("token"))
		if token == "" {
			writeError(w, http.StatusBadRequest, "missing_token", "请求里没有验证令牌")
			return
		}

		actions, ok := s.verificationActions()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "verification_unavailable",
				"入群验证功能没有运行，或者没有提供这三个动作，无法处理这个请求")
			return
		}

		entry, found, err := s.pendingByToken(ctx, token)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !found {
			// Said as an answer rather than as a failure: a token that is not in
			// the queue is the ordinary end of a verification, and the page that
			// asked is usually about to be refreshed anyway.
			writeError(w, http.StatusNotFound, "verification_not_found",
				"这个令牌不在待验证队列里：可能已经被处理过，或者已经到期。刷新一下看最新队列。")
			return
		}

		outcome := verificationActionResponse{
			Token:        token,
			Action:       action,
			GroupOpenID:  entry.GroupOpenID,
			MemberOpenID: entry.MemberOpenID,
		}

		switch action {
		case "resend":
			err = actions.Resend(ctx, entry.GroupOpenID, entry.MemberOpenID)
			outcome.Message = "已重新发送验证卡片（按钮还是原来那个，仍然有效）"
		case "approve":
			err = actions.Approve(ctx, entry.GroupOpenID, entry.MemberOpenID)
			// 群里那条通知是这一页看不见的后果，所以在这里说一声：放行是一次有人
			// 能做主的决定，群成员会看到「管理员已跳过 …」那句话。
			outcome.Message = "已放行：扣留解除，记录从队列里移除；群里已经发了那条「管理员已跳过 …」（skip_message）"
		case "reject":
			outcome, err = s.rejectVerification(ctx, r, actions, entry, outcome)
		default:
			writeError(w, http.StatusBadRequest, "unknown_action", "不认识的验证动作："+action)
			return
		}

		if err != nil {
			var notHeld *joinverify.NotHeld
			if errors.As(err, &notHeld) {
				// The feature has already let go of this member: an answer, not
				// a malfunction.
				writeError(w, http.StatusConflict, "verification_not_held", notHeld.Error())
				return
			}
			var badBody *rejectBodyError
			if errors.As(err, &badBody) {
				writeError(w, http.StatusBadRequest, "bad_body", badBody.Message)
				return
			}
			s.fail(w, r, err)
			return
		}

		if outcome.Message != "" {
			s.opts.Logger.Info("a verification was acted on from the management console",
				"action", action, "group", entry.GroupOpenID, "member", entry.MemberOpenID,
				"token", token, "blacklisted", outcome.Blacklisted)
		}
		s.finishVerification(ctx, &outcome)
		writeJSON(w, http.StatusOK, outcome)
	}
}

// rejectVerification turns a member away, and bars them from coming back when the
// request asked for it.
//
// The two halves stay separate on purpose: the removal is the feature's decision
// and the list is the store's, and a removal that worked must not be reported as
// a failure because the list write did not. What did happen is what the answer
// says, field by field.
func (s *Server) rejectVerification(ctx context.Context, r *http.Request,
	actions feature.VerifierActions, entry store.Pending,
	outcome verificationActionResponse) (verificationActionResponse, error) {
	var request struct {
		Blacklist bool   `json:"blacklist"`
		Reason    string `json:"reason"`
	}
	// A body is optional: rejecting without barring is the ordinary case, and an
	// empty body has to mean that rather than a bad request.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			return outcome, &rejectBodyError{Message: "请求体不是合法的 JSON：" + err.Error()}
		}
	}

	if err := actions.Reject(ctx, entry.GroupOpenID, entry.MemberOpenID); err != nil {
		return outcome, err
	}
	// The record is closed and the hold was not lifted. What is still silencing
	// them depends on which kind it was, and the two are said apart:
	// a real mute is the platform's and outlives the record, while a pseudo-hold
	// was the record taking messages back -- so the feature turned it into a real
	// mute on the way out, and the sentence says so rather than leaving the
	// operator to wonder whether the member can speak now.
	hold := "平台禁言没有解除，到 " + time.Unix(entry.HeldUntil, 0).Format("2006-01-02 15:04") + " 自行结束"
	if entry.HeldUntil == 0 {
		hold = "这条原本是伪禁言（靠记录撤回消息），已按本群配置的时长改为真禁言，所以关掉记录不会把人放出来"
	}

	if request.Blacklist {
		if err := s.blacklistAdd(ctx, entry, request.Reason); err != nil {
			// The record is already closed; only the list is missing. Said in
			// the message rather than hidden behind a 500, because the operator
			// has to know which half to repeat.
			s.opts.Logger.Error("a barred member could not be added to the blacklist",
				"group", entry.GroupOpenID, "member", entry.MemberOpenID, "error", err)
			outcome.Message = "验证记录已关闭，但加入黑名单失败（详情见机器人日志），这一半要重试；" + hold + "。"
			return outcome, nil
		}
		outcome.Blacklisted = true
		outcome.Message = "已拉黑并关闭验证记录；" + hold +
			"。机器人不能踢出群成员，要移除请在群里手动踢出。"
		return outcome, nil
	}
	outcome.Message = "已关闭验证记录（没有加入黑名单）；" + hold + "。"
	return outcome, nil
}

// finishVerification reports how long the queue still is, after the action.
//
// Counted from the store rather than decremented from the answer: the queue is
// what the store holds, and a number this side computed would be a second answer
// to the same question.
func (s *Server) finishVerification(ctx context.Context, outcome *verificationActionResponse) {
	entries, err := s.opts.Store.Pending().Load(ctx, s.now())
	if err != nil {
		s.opts.Logger.Warn("could not count the queue after an action", "error", err)
		return
	}
	outcome.Pending = len(entries)
}

// blacklistAdd bars a member from a group's future join requests.
//
// The id is random rather than the member's openid, because the same column also
// has to hold union openids for people who have never applied here and therefore
// have no member openid to be keyed on -- which is the store's reason for the
// shape, restated here so the entry keeps it.
func (s *Server) blacklistAdd(ctx context.Context, entry store.Pending, reason string) error {
	key, err := newEntryID()
	if err != nil {
		return err
	}
	return s.opts.Store.Blacklist().Add(ctx, store.Barred{
		ID:           key,
		GroupOpenID:  entry.GroupOpenID,
		MemberOpenID: entry.MemberOpenID,
		Reason:       reason,
		AddedAt:      s.now().Unix(),
		AddedBy:      consoleActor,
	})
}

// consoleActor is who the console's writes are recorded as coming from.
//
// The console has no openid: it is one person at a token, and inventing a member
// openid for them would put a name in the record that no group can resolve.
const consoleActor = "management console"

// newEntryID returns the id a new list entry is stored under.
func newEntryID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating an entry id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// verificationActions is the join verification feature as the console drives it.
//
// Asserted here rather than declared in Options: the bot has no dependency on the
// console, so a deployment without this feature is a bot that works and a console
// that says so, rather than a program that will not start.
func (s *Server) verificationActions() (feature.VerifierActions, bool) {
	if s.opts.Features == nil {
		return nil, false
	}
	instance := s.opts.Features.Instance(joinverify.Name)
	if instance == nil {
		return nil, false
	}
	actions, ok := instance.(feature.VerifierActions)
	return actions, ok
}

// pendingByToken finds who a verification token belongs to.
//
// Read from the data layer rather than from the feature, because that is where
// the queue the console shows came from: resolving it anywhere else would be a
// second opinion about who is waiting.
func (s *Server) pendingByToken(ctx context.Context, token string) (store.Pending, bool, error) {
	entries, err := s.opts.Store.Pending().Load(ctx, s.now())
	if err != nil {
		return store.Pending{}, false, err
	}
	for _, entry := range entries {
		if entry.Token == token {
			return entry, true, nil
		}
	}
	return store.Pending{}, false, nil
}

// rejectBodyError is a request body that could not be read. It carries the
// sentence to show, so the answer is written in one place -- the handler -- and
// not from inside the step that failed.
type rejectBodyError struct {
	Message string
}

// Error implements error.
func (e *rejectBodyError) Error() string { return e.Message }

// handleBlacklistAdd bars a member from a group's future join requests.
//
// Written straight to the store, because nothing keeps a copy: the admission
// decision reads this table when a request arrives, so a row is the whole of what
// "barred" means. (The high-risk list is the opposite -- see handleWatchAdd.)
func (s *Server) handleBlacklistAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var request struct {
		GroupOpenID  string `json:"group_openid"`
		MemberOpenID string `json:"member_openid"`
		UnionOpenID  string `json:"union_openid"`
		Reason       string `json:"reason"`
		// ExpiresAt is Unix seconds, and zero means permanent -- which for this
		// list is a real answer rather than a missing one.
		ExpiresAt int64 `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "请求体不是合法的 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(request.MemberOpenID) == "" && strings.TrimSpace(request.UnionOpenID) == "" {
		writeError(w, http.StatusBadRequest, "no_identity",
			"至少要给一个 member openid 或 union openid：两者都没有的条目拦不住任何人")
		return
	}

	key, err := newEntryID()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	entry := store.Barred{
		ID:           key,
		GroupOpenID:  strings.TrimSpace(request.GroupOpenID),
		MemberOpenID: strings.TrimSpace(request.MemberOpenID),
		UnionOpenID:  strings.TrimSpace(request.UnionOpenID),
		Reason:       strings.TrimSpace(request.Reason),
		AddedAt:      s.now().Unix(),
		AddedBy:      consoleActor,
		ExpiresAt:    request.ExpiresAt,
	}
	if err := s.opts.Store.Blacklist().Add(ctx, entry); err != nil {
		// Validation rather than a malfunction: the store refuses an entry that
		// names nobody, and says so in its own words.
		writeError(w, http.StatusBadRequest, "blacklist_refused", "没有写进黑名单："+err.Error())
		return
	}
	s.opts.Logger.Info("a blacklist entry was added from the management console",
		"group", entry.GroupOpenID, "member", entry.MemberOpenID, "entry", entry.ID)
	writeJSON(w, http.StatusCreated, blacklistEntry{
		ID:           entry.ID,
		GroupOpenID:  entry.GroupOpenID,
		MemberOpenID: entry.MemberOpenID,
		UnionOpenID:  entry.UnionOpenID,
		Reason:       entry.Reason,
		AddedAt:      entry.AddedAt,
		AddedBy:      entry.AddedBy,
		ExpiresAt:    entry.ExpiresAt,
	})
}

// handleBlacklistRemove takes one entry back.
//
// The group is a query parameter rather than part of the path: an entry with no
// group of its own is everybody's, and the store's own removal is scoped by the
// group that is asking.
func (s *Server) handleBlacklistRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := strings.TrimSpace(r.PathValue("id"))
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", "请求里没有条目 id")
		return
	}
	if err := s.opts.Store.Blacklist().Remove(ctx, group, id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.opts.Logger.Info("a blacklist entry was removed from the management console",
		"entry", id, "group", group)
	writeJSON(w, http.StatusOK, removalResponse{Removed: true})
}

// handleWatchAdd marks a member whose messages are to be judged as they arrive.
//
// Through the feature and never through the store: who is watched is kept in the
// feature's memory and read from there on the message path, so a row written
// behind its back would be a mark the console showed and nothing enforced.
func (s *Server) handleWatchAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	moderation, ok := s.moderationFeature()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "moderation_unavailable",
			"违规审核功能没有运行，无法标记高风险成员")
		return
	}

	var request struct {
		GroupOpenID  string `json:"group_openid"`
		MemberOpenID string `json:"member_openid"`
		Reason       string `json:"reason"`
		ExpiresAt    int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "请求体不是合法的 JSON："+err.Error())
		return
	}
	if strings.TrimSpace(request.MemberOpenID) == "" {
		writeError(w, http.StatusBadRequest, "missing_member", "标记高风险需要一个 member openid")
		return
	}
	if request.ExpiresAt <= 0 {
		// Not a convenience: a mark with no expiry is one nobody ever revisits,
		// and the store refuses it. Said here so the answer is a sentence rather
		// than a store error.
		writeError(w, http.StatusBadRequest, "missing_expiry",
			"到期时间是必填的（Unix 秒）：这条标记会一直让该成员的每条消息都过一次判定，所以必须有人决定它什么时候结束")
		return
	}

	key, err := newEntryID()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	entry := store.Watch{
		ID:           key,
		GroupOpenID:  strings.TrimSpace(request.GroupOpenID),
		MemberOpenID: strings.TrimSpace(request.MemberOpenID),
		Reason:       strings.TrimSpace(request.Reason),
		AddedAt:      s.now().Unix(),
		AddedBy:      consoleActor,
		ExpiresAt:    request.ExpiresAt,
	}
	if err := moderation.Watch(ctx, entry); err != nil {
		writeError(w, http.StatusBadRequest, "watch_refused", "没有标记成功："+err.Error())
		return
	}
	s.opts.Logger.Info("a high risk mark was added from the management console",
		"group", entry.GroupOpenID, "member", entry.MemberOpenID, "entry", entry.ID)
	writeJSON(w, http.StatusCreated, highRiskWatch{
		ID:           entry.ID,
		GroupOpenID:  entry.GroupOpenID,
		MemberOpenID: entry.MemberOpenID,
		Reason:       entry.Reason,
		AddedAt:      entry.AddedAt,
		AddedBy:      entry.AddedBy,
		ExpiresAt:    entry.ExpiresAt,
	})
}

// handleWatchRemove takes one mark off the list.
//
// The pair is taken as query parameters rather than as two path segments, and
// that is not a style choice: a mark written before the list was kept per group
// has an empty group openid, and an empty path segment is a segment the router
// cannot tell from a missing one -- `/high-risk//MEMBER` is normalised to
// `/high-risk/MEMBER`, which matches no route at all. The console then reports
// "no such endpoint" for a request that was reasonable, which is exactly the kind
// of wrong answer this package exists to avoid. A query parameter carries an
// empty value without any of that.
func (s *Server) handleWatchRemove(w http.ResponseWriter, r *http.Request) {
	moderation, ok := s.moderationFeature()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "moderation_unavailable",
			"违规审核功能没有运行，无法撤销高风险标记")
		return
	}
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	member := strings.TrimSpace(r.URL.Query().Get("member"))
	if member == "" {
		writeError(w, http.StatusBadRequest, "missing_member",
			"撤销高风险标记需要 member 参数（群可以留空，表示那条不标群的历史记录）")
		return
	}
	removed, err := moderation.Unwatch(r.Context(), group, member)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.opts.Logger.Info("a high risk mark was removed from the management console",
		"group", group, "member", member, "removed", removed)
	// The answer is the feature's own, not "done": it reports whether anybody was
	// on the list, so an operator is never told a list changed when it did not.
	writeJSON(w, http.StatusOK, removalResponse{Removed: removed})
}

// removalResponse is what a delete answers with.
type removalResponse struct {
	// Removed reports whether there was anything to remove.
	Removed bool `json:"removed"`
}

// moderationFeature is the moderation feature as the console drives it.
func (s *Server) moderationFeature() (feature.Moderation, bool) {
	if s.opts.Features == nil {
		return nil, false
	}
	instance := s.opts.Features.Instance("moderation")
	if instance == nil {
		return nil, false
	}
	moderation, ok := instance.(feature.Moderation)
	return moderation, ok
}

// listenAddr is where the console is actually answering.
//
// The bound socket first: an edit to admin.listen cannot move one that is
// already bound, and reporting the edited address would send an operator to a
// port nothing is listening on.
func (s *Server) listenAddr(admin config.Admin) string {
	if s.bound != "" {
		return s.bound
	}
	return admin.AdminListen()
}

// version names the build, from what the toolchain recorded rather than from a
// constant somebody has to remember to change.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision, dirty := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		return revision + "+dirty"
	}
	return revision
}

// databaseSize measures the SQLite file, or reports zero for a database that is
// not a file.
func databaseSize(cfg *config.Config) int64 {
	if cfg.Database.Driver != config.DefaultDatabaseDriver {
		return 0
	}
	info, err := os.Stat(cfg.Database.DSN)
	if err != nil {
		return 0
	}
	return info.Size()
}

// commandPrefix reads what a command is typed with out of the command feature's
// section, falling back to the default that feature applies itself.
func commandPrefix(cfg *config.Config) string {
	section, ok := cfg.Feature(admincmd.Name)
	if !ok {
		return admincmd.DefaultPrefix
	}
	var probe struct {
		Prefix string `yaml:"prefix"`
	}
	if err := section.Decode(&probe); err != nil || strings.TrimSpace(probe.Prefix) == "" {
		return admincmd.DefaultPrefix
	}
	return strings.TrimSpace(probe.Prefix)
}

// judgedModel reads the model name the moderation feature judges with.
//
// A probe of the raw section rather than the feature's own type: the console
// shows one string out of a large section, and importing the whole type to read
// it would be a dependency that has to move every time that section does.
func judgedModel(cfg *config.Config) string {
	section, ok := cfg.Feature("moderation")
	if !ok {
		return ""
	}
	var probe struct {
		Model struct {
			Name string `yaml:"name"`
		} `yaml:"model"`
	}
	if err := section.Decode(&probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model.Name)
}

// groupIDs lists the managed groups' openids.
func groupIDs(cfg *config.Config) []string {
	ids := make([]string, 0, len(cfg.Bot.Groups))
	for _, group := range cfg.Bot.Groups {
		ids = append(ids, group.OpenID)
	}
	return ids
}

// aliasesOf returns a command's other names, never nil so that the front end can
// map over it without a check.
func aliasesOf(def command.Def) []string {
	if len(def.Aliases) == 0 {
		return []string{}
	}
	return def.Aliases
}

// limitOf reads the limit a caller asked for, bounded.
func limitOf(r *http.Request, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return min(value, maxLimit)
}

// sinceOf reads the window a caller asked for, bounded, defaulting to the last
// day.
func (s *Server) sinceOf(r *http.Request, now time.Time) time.Time {
	fallback := now.Add(-defaultWindow)
	raw := strings.TrimSpace(r.URL.Query().Get("since"))
	if raw == "" {
		return fallback
	}
	value, ok := parseUnix(raw)
	if !ok {
		return fallback
	}
	at := time.Unix(value, 0)
	if at.After(now) {
		return now
	}
	if oldest := now.Add(-maxWindow); at.Before(oldest) {
		return oldest
	}
	return at
}

// parseUnix reads a Unix second count.
func parseUnix(raw string) (int64, bool) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// seqOf reads an unsigned number from the query, reporting whether there was one.
//
// The distinction between absent and zero is the caller's, not this function's:
// for the log, "I have seen nothing" and "I did not ask" are different questions
// and the answers differ by the whole buffer.
func seqOf(r *http.Request, key string) (uint64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
