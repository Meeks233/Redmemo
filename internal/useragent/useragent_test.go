package useragent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type mockStore struct {
	data map[string]string
}

func newMockStore() *mockStore {
	return &mockStore{data: make(map[string]string)}
}

func (m *mockStore) Get(name string) (string, bool, error) {
	v, ok := m.data[name]
	return v, ok, nil
}

func (m *mockStore) SetBatch(settings map[string]string, source string) error {
	for k, v := range settings {
		m.data[k] = v
	}
	return nil
}

func TestFallback(t *testing.T) {
	p := newPoolWithURL(newMockStore(), "http://127.0.0.1:1/nonexistent")
	ua := p.Get()

	found := false
	for _, fb := range fallback {
		if ua == fb {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Get() returned %q, expected a fallback value", ua)
	}
}

func TestGet(t *testing.T) {
	p := newPoolWithURL(newMockStore(), "http://127.0.0.1:1/nonexistent")
	ua := p.Get()

	if ua == "" {
		t.Fatal("Get() returned empty string")
	}
	if !strings.Contains(ua, "Mozilla") {
		t.Errorf("Get() = %q, expected to contain 'Mozilla'", ua)
	}
}

func TestLoadFromStore(t *testing.T) {
	store := newMockStore()
	agents := []string{"Mozilla/5.0 CachedAgent/1.0", "Mozilla/5.0 CachedAgent/2.0"}
	data, _ := json.Marshal(agents)
	store.data[kvKeyList] = string(data)
	store.data[kvKeyFetchedAt] = time.Now().UTC().Format(time.RFC3339)

	p := newPoolWithURL(store, "http://127.0.0.1:1/nonexistent")
	ua := p.Get()

	if ua != agents[0] && ua != agents[1] {
		t.Errorf("Get() = %q, expected one of the cached agents", ua)
	}
}

func TestExpiredCacheFetchesFresh(t *testing.T) {
	store := newMockStore()
	agents := []string{"Mozilla/5.0 OldAgent/1.0"}
	data, _ := json.Marshal(agents)
	store.data[kvKeyList] = string(data)
	store.data[kvKeyFetchedAt] = time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339)

	// Remote is unreachable, so it should keep the old cached list
	p := newPoolWithURL(store, "http://127.0.0.1:1/nonexistent")
	ua := p.Get()

	if ua != agents[0] {
		t.Errorf("Get() = %q, expected old cached agent after failed refresh", ua)
	}
}
