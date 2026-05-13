package useragent

import (
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

const (
	sourceURL    = "https://microlink.io/user-agents.json"
	fetchTimeout = 10 * time.Second

	kvKeyList      = "_ua_pool_list"
	kvKeyFetchedAt = "_ua_pool_fetched_at"

	minTTLDays = 20
	maxTTLDays = 60
)

var fallback = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0",
}

// Store is the persistence interface for UA pool state.
type Store interface {
	Get(name string) (value string, ok bool, err error)
	SetBatch(settings map[string]string, source string) error
}

type Pool struct {
	mu   sync.RWMutex
	list []string
}

// NewPool creates a UA pool. It loads the cached list from the DB; if the
// cached list is older than a random 20–60 day threshold, it fetches a fresh
// list from the remote source and persists it. No background goroutine runs.
func NewPool(store Store) *Pool {
	p := &Pool{list: fallback}
	p.init(store, sourceURL)
	return p
}

func newPoolWithURL(store Store, url string) *Pool {
	p := &Pool{list: fallback}
	p.init(store, url)
	return p
}

func (p *Pool) Get() string {
	p.mu.RLock()
	list := p.list
	p.mu.RUnlock()
	return list[rand.Intn(len(list))]
}

func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.list)
}

func (p *Pool) List() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.list))
	copy(out, p.list)
	return out
}

func (p *Pool) init(store Store, url string) {
	if store == nil {
		p.fetchRemote(url)
		return
	}

	ttl := time.Duration(minTTLDays+rand.Intn(maxTTLDays-minTTLDays+1)) * 24 * time.Hour

	cached, expired := p.loadFromStore(store, ttl)
	if cached {
		if !expired {
			log.Printf("useragent: using cached list (%d entries), next refresh in ≤%dd", len(p.list), maxTTLDays)
			return
		}
		log.Printf("useragent: cached list expired, refreshing from remote")
	}

	if p.fetchRemote(url) {
		p.saveToStore(store)
	}
}

func (p *Pool) loadFromStore(store Store, ttl time.Duration) (cached bool, expired bool) {
	tsRaw, ok, err := store.Get(kvKeyFetchedAt)
	if err != nil || !ok || tsRaw == "" {
		return false, true
	}

	fetchedAt, err := time.Parse(time.RFC3339, tsRaw)
	if err != nil {
		log.Printf("useragent: bad timestamp in DB: %v", err)
		return false, true
	}

	listRaw, ok, err := store.Get(kvKeyList)
	if err != nil || !ok || listRaw == "" {
		return false, true
	}

	var agents []string
	if err := json.Unmarshal([]byte(listRaw), &agents); err != nil || len(agents) == 0 {
		return false, true
	}

	p.mu.Lock()
	p.list = agents
	p.mu.Unlock()

	return true, time.Since(fetchedAt) > ttl
}

func (p *Pool) fetchRemote(url string) bool {
	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("useragent: fetch failed: %v", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("useragent: fetch returned status %d", resp.StatusCode)
		return false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("useragent: read body: %v", err)
		return false
	}

	var agents []string
	if err := json.Unmarshal(body, &agents); err != nil {
		log.Printf("useragent: parse json: %v", err)
		return false
	}

	if len(agents) == 0 {
		log.Printf("useragent: empty list from remote")
		return false
	}

	p.mu.Lock()
	p.list = agents
	p.mu.Unlock()
	log.Printf("useragent: fetched %d user agents from remote", len(agents))
	return true
}

func (p *Pool) saveToStore(store Store) {
	p.mu.RLock()
	list := p.list
	p.mu.RUnlock()

	data, err := json.Marshal(list)
	if err != nil {
		log.Printf("useragent: marshal list: %v", err)
		return
	}

	err = store.SetBatch(map[string]string{
		kvKeyList:      string(data),
		kvKeyFetchedAt: time.Now().UTC().Format(time.RFC3339),
	}, "system")
	if err != nil {
		log.Printf("useragent: save to DB: %v", err)
		return
	}
	log.Printf("useragent: persisted %d entries to DB", len(list))
}
