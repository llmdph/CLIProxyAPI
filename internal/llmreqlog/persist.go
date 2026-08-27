package llmreqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	persistVersion  = 1
	persistDebounce = 400 * time.Millisecond
)

type persistPayload struct {
	Version int     `json:"version"`
	Items   []Entry `json:"items"`
}

// SetPersistPath stores LLM request logs on disk so they survive process restarts.
func SetPersistPath(path string) {
	defaultStore.setPersistPath(path)
}

func (s *store) setPersistPath(path string) {
	if s == nil {
		return
	}
	path = strings.TrimSpace(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persistPath == path {
		return
	}
	s.persistPath = path
	if path == "" {
		return
	}
	if len(s.items) > 0 {
		s.scheduleSaveLocked()
		return
	}
	loaded, err := readPersistFile(path)
	if err != nil || len(loaded) == 0 {
		return
	}
	if s.capacity <= 0 {
		s.capacity = defaultCapacity
	}
	if len(loaded) > s.capacity {
		loaded = loaded[len(loaded)-s.capacity:]
	}
	s.items = loaded
}

func (s *store) scheduleSaveLocked() {
	if s == nil || strings.TrimSpace(s.persistPath) == "" {
		return
	}
	if s.saveTimer != nil {
		s.saveTimer.Stop()
	}
	s.saveTimer = time.AfterFunc(persistDebounce, s.flushAsync)
}

func (s *store) flushAsync() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.saveTimer = nil
	path := s.persistPath
	items := append([]Entry(nil), s.items...)
	s.mu.Unlock()
	if strings.TrimSpace(path) == "" {
		return
	}
	_ = writePersistFile(path, items)
}

func (s *store) flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.saveTimer != nil {
		s.saveTimer.Stop()
		s.saveTimer = nil
	}
	path := s.persistPath
	items := append([]Entry(nil), s.items...)
	s.mu.Unlock()
	if strings.TrimSpace(path) == "" {
		return
	}
	_ = writePersistFile(path, items)
}

func entryMatchesAccount(entry Entry, account string) bool {
	query := strings.ToLower(strings.TrimSpace(account))
	if query == "" {
		return true
	}
	fields := []string{entry.Account, entry.AuthID, entry.Source}
	if detail, ok := entry.Detail.(map[string]any); ok && detail != nil {
		for _, key := range []string{"auth_index", "email"} {
			if value, ok := detail[key].(string); ok {
				fields = append(fields, value)
			}
		}
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(strings.TrimSpace(field)), query) {
			return true
		}
	}
	return false
}

func readPersistFile(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var payload persistPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		var items []Entry
		if errItems := json.Unmarshal(raw, &items); errItems != nil {
			return nil, err
		}
		return items, nil
	}
	return payload.Items, nil
}

func writePersistFile(path string, items []Entry) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(persistPayload{Version: persistVersion, Items: items})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(path)
		if errRename := os.Rename(tmp, path); errRename != nil {
			_ = os.Remove(tmp)
			return errRename
		}
	}
	return nil
}
