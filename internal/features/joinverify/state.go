package joinverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// The pending list lives in the data layer. What is left here is the JSON file
// the earlier build wrote, read exactly once so that changing over does not
// forget members who are already held.
//
// A hold is a real thing applied to a real person, and losing the record means it
// stays applied with nobody knowing why: a mute, which lasts for as long as twenty
// nine days, or a pseudo-mute, which lasts until the member verifies and therefore
// lasts for exactly as long as the record does. Either way there is nobody to
// release them, no deadline acted on, and a button that answers 操作失败 until it
// is over.

// A hold that reaches the platform but not the data layer is exactly the record
// that must not be lost, so every mutation is written as it happens rather than
// on shutdown.

// stateFileVersion marks the layout of the JSON file, so an older or newer file
// is refused instead of being misread.
const stateFileVersion = 1

// heldUntilStamp renders the moment a hold runs out for the data layer.
//
// The zero moment is how "this hold has no expiry of its own" is written down --
// a pseudo-mute lasts until the member verifies -- and it is stored as zero
// rather than as the zero time's own year, because a column read by a filter that
// asks "is this still in the future" has to be able to tell the two apart.
func heldUntilStamp(moment time.Time) int64 {
	if moment.IsZero() {
		return 0
	}
	return moment.Unix()
}

// heldUntilMoment reads that moment back, and gives the zero moment back for a
// hold that has no expiry.
func heldUntilMoment(stamp int64) time.Time {
	if stamp == 0 {
		return time.Time{}
	}
	return time.Unix(stamp, 0)
}

// importMarker records that the JSON file has been imported. It is a marker and
// not "the table is empty" because the table empties again once the last member
// verifies: without it, that day would read the stale file and bring dead holds
// back to life.
const importMarker = "pending_json_imported"

// storedEntry is one record as the JSON file holds it.
type storedEntry struct {
	Token        string    `json:"token"`
	GroupOpenID  string    `json:"group_openid"`
	MemberOpenID string    `json:"member_openid"`
	JoinedAt     int64     `json:"joined_at"`
	Deadline     time.Time `json:"deadline"`
	HeldUntil    time.Time `json:"held_until"`
	Reported     bool      `json:"reported"`
	Settings     Settings  `json:"settings"`
}

// storedState is the JSON file as a whole.
type storedState struct {
	Version int           `json:"version"`
	Pending []storedEntry `json:"pending"`
}

// readState loads the records written by the earlier build.
//
// Records whose hold the platform has already released are dropped: their mute
// is over, so there is nothing to answer and nothing to act on. A record with no
// expiry of its own is kept: that is a hold the earlier build could not have
// written, but this one can, and it is still somebody's hold.
func readState(path string, now time.Time) ([]storedEntry, error) {
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var stored storedState
	if err := json.Unmarshal(payload, &stored); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if stored.Version != stateFileVersion {
		return nil, fmt.Errorf("%s has version %d, but this build reads version %d",
			path, stored.Version, stateFileVersion)
	}

	kept := make([]storedEntry, 0, len(stored.Pending))
	for _, entry := range stored.Pending {
		if entry.Token == "" || entry.GroupOpenID == "" || entry.MemberOpenID == "" {
			return nil, fmt.Errorf("%s has an entry without a token, group or member", path)
		}
		if !entry.HeldUntil.IsZero() && !now.Before(entry.HeldUntil) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept, nil
}

// encodeSettings renders a group's settings for the data layer.
//
// Encoded by field name, so a field renamed in Settings would stop being read
// back. That is deliberate: a silent default would apply different rules to
// somebody who is already muted, which is worse than a visible failure.
func encodeSettings(settings Settings) (string, error) {
	payload, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("encoding the settings: %w", err)
	}
	return string(payload), nil
}

// decodeSettings reads settings back out of the data layer.
func decodeSettings(raw string) (Settings, error) {
	var settings Settings
	if raw == "" {
		return settings, nil
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return settings, fmt.Errorf("reading the settings back: %w", err)
	}
	return settings, nil
}

// restore loads the members who were already held, importing the JSON file the
// earlier build wrote the first time this runs.
func (v *verifier) restore(ctx context.Context, now time.Time) {
	v.importLegacyState(ctx, now)

	if v.pending == nil {
		// No data layer: nothing was ever written down, so there is nothing to
		// restore. The feature still works, it just forgets on a restart -- which
		// is worth saying out loud, because what it forgets is not the same in
		// both modes: a real mute stays applied with nobody left to release it,
		// while a pseudo-mute simply stops, and the member talks as if they had
		// verified.
		v.deps.Logger.Warn("no data layer is configured, so a restart would forget " +
			"every member who is still being held: under a real mute they stay " +
			"muted with nobody to release them, and under a pseudo-mute they are " +
			"free to talk again")
		return
	}

	entries, err := v.pending.Load(ctx, now)
	if err != nil {
		// Starting with none would leave every held member unaccounted for while
		// their mute stays in force, which is the one outcome worth shouting
		// about.
		v.deps.Logger.Error("could not load the pending verifications, so nobody "+
			"who is already held is accounted for", "error", err)
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	for _, entry := range entries {
		settings, err := decodeSettings(entry.Settings)
		if err != nil {
			// Kept with no settings rather than dropped: the member is muted, and
			// forgetting them would leave the button dead and the deadline
			// unattended.
			v.deps.Logger.Error("could not read back the settings of a hold",
				"group", entry.GroupOpenID, "member", entry.MemberOpenID, "error", err)
		}
		held := &pending{
			token:        entry.Token,
			groupOpenID:  entry.GroupOpenID,
			memberOpenID: entry.MemberOpenID,
			joinedAt:     entry.JoinedAt,
			deadline:     time.Unix(entry.Deadline, 0),
			heldUntil:    heldUntilMoment(entry.HeldUntil),
			reported:     entry.Reported,
			settings:     settings,
		}
		v.byToken[held.token] = held
		v.byMember[memberKey(held.groupOpenID, held.memberOpenID)] = held
	}
	if len(entries) > 0 {
		v.deps.Logger.Info("restored members who were still waiting to verify",
			"count", len(entries))
	}
}

// importLegacyState copies the JSON file into the data layer, once.
//
// Failures are reported and swallowed: the file is not the source of truth any
// more, and refusing to start over a file that may already have been imported
// would take the whole bot down for nothing.
func (v *verifier) importLegacyState(ctx context.Context, now time.Time) {
	if v.cfg.StateFile == "" || v.pending == nil || v.meta == nil {
		return
	}
	if _, done, err := v.meta.Get(ctx, importMarker); err != nil {
		v.deps.Logger.Error("could not read the import marker, so the older file "+
			"is left untouched", "error", err)
		return
	} else if done {
		return
	}

	entries, err := readState(v.cfg.StateFile, now)
	if err != nil {
		v.deps.Logger.Error("could not read the file the earlier build wrote, "+
			"so its records are not imported", "file", v.cfg.StateFile, "error", err)
		return
	}
	imported := 0
	for _, entry := range entries {
		settings, err := encodeSettings(entry.Settings)
		if err != nil {
			v.deps.Logger.Error("could not import a record from the older file",
				"token", entry.Token, "error", err)
			return
		}
		if err := v.pending.Put(ctx, store.Pending{
			Token:        entry.Token,
			GroupOpenID:  entry.GroupOpenID,
			MemberOpenID: entry.MemberOpenID,
			JoinedAt:     entry.JoinedAt,
			Deadline:     entry.Deadline.Unix(),
			HeldUntil:    heldUntilStamp(entry.HeldUntil),
			Reported:     entry.Reported,
			Settings:     settings,
		}); err != nil {
			v.deps.Logger.Error("could not import a record from the older file",
				"token", entry.Token, "error", err)
			return
		}
		imported++
	}

	// The marker is written even when nothing was imported, so an absent or empty
	// file is not read again on every start.
	if err := v.meta.Set(ctx, importMarker, now.Format(time.RFC3339)); err != nil {
		v.deps.Logger.Error("could not record that the older file was imported, "+
			"so it may be imported again", "error", err)
		return
	}
	v.deps.Logger.Info("imported the records the earlier build left in a file",
		"file", v.cfg.StateFile, "count", imported)
}
