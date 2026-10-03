package moderation

import (
	"errors"
	"fmt"
	"sync"

	"gopkg.in/yaml.v3"
)

// guarded carries the lock around the configuration.
//
// A reload replaces the whole configuration at once, under this lock, and every
// reader takes it: a judgement that started under the old file finishes under the old
// file, and the next one starts under the new one. Nobody sees a mixture.
type guarded struct{ mu sync.RWMutex }

// config is the configuration in force.
func (h *handler) config() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Reload implements feature.Reloadable.
//
// It rebuilds the configuration exactly as New does -- decode, then defaults and
// checks -- so there is one path for the first file and every later one, and a file
// that does not pass the checks is refused with its own reason while the running
// configuration stays exactly as it was.
//
// The few settings it cannot adopt are refused rather than ignored. A reload that
// accepted a new cache_hours and went on using the old one would be a file that lies
// about itself, and the point of the whole path is that the file in force is the file
// that was written.
func (h *handler) Reload(section yaml.Node) error {
	var fresh Config
	if err := section.Decode(&fresh); err != nil {
		return fmt.Errorf("reading the %s section: %w", Name, err)
	}
	if err := fresh.applyDefaults(); err != nil {
		return err
	}

	inForce := h.config()
	if fresh.CacheHours != inForce.CacheHours {
		return errors.New("cache_hours 改了：缓存的寿命在启动时决定，需要重启")
	}
	if fresh.ContextScope != inForce.ContextScope {
		return errors.New("context_scope 改了：取窗方式在启动时决定，需要重启")
	}

	h.mu.Lock()
	h.cfg = fresh
	h.mu.Unlock()
	return nil
}

// groupName is what the platform calls this group.
func (h *handler) groupName(groupOpenID string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if name := h.names[groupOpenID]; name != "" {
		return name
	}
	return "（未知）"
}

// topicFor is what this group says it is about, or empty when it has not said.
func (h *handler) topicFor(groupOpenID string) string {
	topic := h.config().groupFor(groupOpenID).Topic
	if topic == "" {
		return "（未声明）"
	}
	return topic
}
