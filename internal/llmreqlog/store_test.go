package llmreqlog

import (
	"os"
	"testing"
)

func TestStoreListFiltersRequestClass(t *testing.T) {
	t.Parallel()
	s := &store{capacity: 10}
	s.add(Entry{ID: "1", RequestClass: "normal"})
	s.add(Entry{ID: "2", RequestClass: "compaction"})
	s.add(Entry{ID: "3", RequestClass: "session_name"})
	s.add(Entry{ID: "4", RequestClass: "internal"})

	items, total := s.list(10, 0, "compaction", "")
	if total != 1 || len(items) != 1 || items[0].ID != "2" {
		t.Fatalf("compaction filter = %+v total=%d", items, total)
	}
	items, total = s.list(10, 0, "", "")
	if total != 4 || len(items) != 4 {
		t.Fatalf("all filter = %d items total=%d", len(items), total)
	}
	if items[0].ID != "4" || items[3].ID != "1" {
		t.Fatalf("newest-first order = %+v", items)
	}
}

func TestStoreListFiltersAccount(t *testing.T) {
	t.Parallel()
	s := &store{capacity: 10}
	s.add(Entry{ID: "1", Account: "alice@outlook.com", AuthID: "xai-alice@outlook.com"})
	s.add(Entry{ID: "2", Account: "bob@gmail.com", Source: "xai-bob@gmail.com"})
	s.add(Entry{ID: "3", Account: "carol@outlook.com", Detail: map[string]any{"auth_index": "xai-carol@outlook.com"}})

	items, total := s.list(10, 0, "", "outlook")
	if total != 2 || len(items) != 2 {
		t.Fatalf("outlook filter = %+v total=%d", items, total)
	}
	items, total = s.list(10, 0, "", "BOB@")
	if total != 1 || len(items) != 1 || items[0].ID != "2" {
		t.Fatalf("bob filter = %+v total=%d", items, total)
	}
}

func TestStorePersistRoundTripAndClear(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + string(os.PathSeparator) + "llm-request-logs.json"
	s := &store{capacity: 10}
	s.setPersistPath(path)
	s.add(Entry{ID: "a", Account: "sean@outlook.com", RequestClass: "normal"})
	s.add(Entry{ID: "b", Account: "other@outlook.com", RequestClass: "session_name"})
	s.flush()

	loaded := &store{capacity: 10}
	loaded.setPersistPath(path)
	items, total := loaded.list(10, 0, "", "sean")
	if total != 1 || len(items) != 1 || items[0].ID != "a" {
		t.Fatalf("reloaded filter = %+v total=%d", items, total)
	}
	if n := loaded.clear(); n != 2 {
		t.Fatalf("clear = %d", n)
	}
	loaded.flush()

	again := &store{capacity: 10}
	again.setPersistPath(path)
	_, total = again.list(10, 0, "", "")
	if total != 0 {
		t.Fatalf("cleared logs came back: total=%d", total)
	}
}
