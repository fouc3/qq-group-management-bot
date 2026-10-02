package joinverify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The pending list is written to disk because a restart must not lose it.
//
// A hold is a real mute on a real person, and the platform applies it for as
// long as twenty nine days. Losing the record means the member stays muted with
// nobody knowing why, no deadline ever acted on, and a button that answers
// 操作失败 for the rest of the mute. Restarting is routine here, so the record
// has to outlive the process.

// stateFileVersion marks the layout of the state file, so a later change can
// refuse to misread an older file instead of silently dropping holds.
const stateFileVersion = 1

// storedEntry is one pending entry as the state file holds it.
//
// It is deliberately separate from pending: the file outlives a release, so a
// field renamed in memory must not quietly lose its data on disk, and the file
// has to stay readable by an older binary.
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

// storedState is the state file as a whole.
type storedState struct {
	Version int           `json:"version"`
	Pending []storedEntry `json:"pending"`
}

// snapshotLocked renders the pending list for the state file.
//
// The caller holds the lock. The result is ordered so the file stays readable
// and a save does not reshuffle it for no reason.
func (v *verifier) snapshotLocked() []storedEntry {
	entries := make([]storedEntry, 0, len(v.byToken))
	for _, entry := range v.byToken {
		entries = append(entries, storedEntry{
			Token:        entry.token,
			GroupOpenID:  entry.groupOpenID,
			MemberOpenID: entry.memberOpenID,
			JoinedAt:     entry.joinedAt,
			Deadline:     entry.deadline,
			HeldUntil:    entry.heldUntil,
			Reported:     entry.reported,
			Settings:     entry.settings,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Token < entries[j].Token })
	return entries
}

// saveStateLocked writes the pending list out.
//
// The caller holds the lock, which is what keeps the snapshot from changing
// underneath it; the file is small and replaced atomically, so the write is
// short. A failure is reported and swallowed: the hold itself is the platform's
// and is still in force, and taking the bot down over a failed write would be
// worse than the write being stale.
func (v *verifier) saveStateLocked() {
	if v.stateFile == "" {
		return
	}
	if err := writeState(v.stateFile, v.snapshotLocked()); err != nil {
		v.deps.Logger.Error("could not save the pending verifications",
			"file", v.stateFile, "error", err)
	}
}

// writeState replaces the state file atomically.
//
// The replacement is a rename, so a crash midway leaves either the old file or
// the new one, never a half written list: a truncated file would read as an
// empty list and would strand every member it named.
func writeState(path string, entries []storedEntry) error {
	payload, err := json.MarshalIndent(storedState{
		Version: stateFileVersion,
		Pending: entries,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the pending list: %w", err)
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("creating a temporary state file in %s: %w", dir, err)
	}
	name := temporary.Name()
	// A failure past this point must not leave the temporary file behind.
	defer func() {
		if _, statErr := os.Stat(name); statErr == nil {
			_ = os.Remove(name)
		}
	}()

	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return fmt.Errorf("writing %s: %w", name, err)
	}
	// The data has to reach the disk before the rename, or a power loss can
	// leave the new name pointing at an empty file.
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("flushing %s: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("restricting %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}

// readState loads the pending list written by an earlier run.
//
// Entries whose hold the platform has already released are dropped rather than
// returned: their mute is over, so there is nothing to answer and nothing to
// act on, and keeping them would mean reporting a member as "still waiting"
// long after they were free to talk. The deadline is left to the ordinary sweep,
// which is what makes a deadline that passed while the bot was down still get
// acted on.
func readState(path string, now time.Time) ([]*pending, error) {
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// A first run has nothing to restore, and that is not a failure.
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
		return nil, fmt.Errorf("%s has version %d, but this build writes version %d",
			path, stored.Version, stateFileVersion)
	}

	restored := make([]*pending, 0, len(stored.Pending))
	for _, entry := range stored.Pending {
		if entry.Token == "" || entry.GroupOpenID == "" || entry.MemberOpenID == "" {
			return nil, fmt.Errorf("%s has an entry without a token, group or member", path)
		}
		if !now.Before(entry.HeldUntil) {
			continue
		}
		restored = append(restored, &pending{
			token:        entry.Token,
			groupOpenID:  entry.GroupOpenID,
			memberOpenID: entry.MemberOpenID,
			joinedAt:     entry.JoinedAt,
			deadline:     entry.Deadline,
			heldUntil:    entry.HeldUntil,
			reported:     entry.Reported,
			settings:     entry.Settings,
		})
	}
	return restored, nil
}

// restore loads the state file into the verifier.
//
// A file that cannot be read is reported and treated as empty rather than
// stopping the bot: refusing to start would leave the groups unmanaged
// altogether, which is worse than losing records that are already suspect.
func (v *verifier) restore(now time.Time) {
	if v.stateFile == "" {
		v.deps.Logger.Warn("no state_file is configured: a restart would forget " +
			"every member who is still waiting to verify, leaving them muted " +
			"with nothing to answer")
		return
	}
	restored, err := readState(v.stateFile, now)
	if err != nil {
		v.deps.Logger.Error("could not restore the pending verifications, "+
			"starting with none", "file", v.stateFile, "error", err)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, entry := range restored {
		v.byToken[entry.token] = entry
		v.byMember[memberKey(entry.groupOpenID, entry.memberOpenID)] = entry
	}
	if len(restored) > 0 {
		v.deps.Logger.Info("restored members who were still waiting to verify",
			"count", len(restored))
	}
}
