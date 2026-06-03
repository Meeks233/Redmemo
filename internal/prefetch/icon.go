package prefetch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

const (
	iconCheckInterval = 1 * time.Hour

	// iconMinPosts is the archive-history floor for L4 auto-refresh. A sub with
	// fewer than this many archived posts is too thin to justify spending an
	// upstream /about.json call every cycle — its icon will be picked up the
	// first time the archive crosses the threshold.
	iconMinPosts = 30

	// iconMinPerCycle / iconMaxPerCycle bound the per-cycle batch size. A
	// pseudo-random count in [min,max] is drawn each cycle so the L4 schedule
	// is not a perfectly even drumbeat — the same throttle a real client
	// applies when manually opening tabs.
	iconMinPerCycle = 1
	iconMaxPerCycle = 4
)

// logListCap is the maximum number of sub names rendered into an event-log
// line before the rest are collapsed into "...(+N more)". Keeps the L4 panel
// readable when the alive set grows past a few dozen subs.
const logListCap = 10

// previewList returns a compact "[a b c ...(+N more)]" rendering capped at
// logListCap entries.
func previewList(subs []string) string {
	if len(subs) <= logListCap {
		return fmt.Sprintf("%v", subs)
	}
	return fmt.Sprintf("%v ...(+%d more)", subs[:logListCap], len(subs)-logListCap)
}

// allAliveSubs returns the union of locally-known live subs. Falls back to the
// prefetch-configured set if the SubStatus store is unavailable.
func (s *Scheduler) allAliveSubs() []string {
	if s.subStatus == nil {
		return s.activeSubs()
	}
	subs, err := s.subStatus.ListAllAlive()
	if err != nil {
		log.Printf("L4: list alive subs failed: %v, falling back to prefetch subs", err)
		return s.activeSubs()
	}
	return subs
}

// buildIconRound assembles a fresh round queue. A sub enters the round only if:
//  1. it is in the alive set,
//  2. its stored icon is missing or expired (a row with has_icon=false is a
//     terminal "no icon" verdict and is skipped forever),
//  3. its archive history meets iconMinPosts.
//
// The resulting queue is sorted by archived-post count descending so the most
// trafficked subs get refreshed first; ties break by name for determinism.
func (s *Scheduler) buildIconRound() []string {
	subs := s.allAliveSubs()
	if len(subs) == 0 {
		return nil
	}

	now := time.Now()
	var candidates []string
	var noIconSkipped, userRecentSkipped int
	for _, sub := range subs {
		icon, err := s.iconStore.Get(sub)
		if err != nil {
			candidates = append(candidates, sub)
			continue
		}
		if icon != nil && !icon.HasIcon {
			noIconSkipped++
			continue
		}
		// If the user just actively fetched /r/<sub>/about (handler path with
		// active=true) within the previous cooldown window, the upstream was
		// already paid. Skip this round so background L4 does not double-tap.
		// The L4 path also writes about_fetched_at, but it bumps expires_at to
		// now+TTL at the same time, so a stale icon row with a fresh
		// about_fetched_at can only have come from the user-active path.
		if icon != nil && icon.AboutFetchedAt != nil && now.Sub(*icon.AboutFetchedAt) < iconCheckInterval {
			userRecentSkipped++
			continue
		}
		if icon == nil || now.After(icon.ExpiresAt) {
			candidates = append(candidates, sub)
		}
	}
	if noIconSkipped > 0 {
		s.Events.Addf(LevelSkip, "L4", "%d sub(s) skipped: known to have no icon upstream", noIconSkipped)
	}
	if userRecentSkipped > 0 {
		s.Events.Addf(LevelSkip, "L4", "%d sub(s) skipped: user actively refreshed within the last %s", userRecentSkipped, iconCheckInterval)
	}
	if len(candidates) == 0 {
		return nil
	}

	if s.postStore != nil {
		counts, err := s.postStore.SubredditCounts(candidates)
		if err != nil {
			log.Printf("L4: subreddit counts failed: %v, skipping density filter", err)
			return candidates
		}
		byLower := make(map[string]int, len(counts))
		for k, v := range counts {
			byLower[strings.ToLower(k)] = v
		}
		var kept, thin []string
		for _, sub := range candidates {
			if byLower[strings.ToLower(sub)] >= iconMinPosts {
				kept = append(kept, sub)
			} else {
				thin = append(thin, sub)
			}
		}
		if len(thin) > 0 {
			s.Events.Addf(LevelSkip, "L4", "%d sub(s) below %d-post threshold, skipped: %s", len(thin), iconMinPosts, previewList(thin))
		}
		sortSubsByPostCount(kept, byLower)
		return kept
	}
	return candidates
}

// sortSubsByPostCount orders subs by post count descending, breaking ties by
// name ascending. countsByLower must be keyed by lowercased sub name. Mutates
// the input slice in place; returns nothing.
func sortSubsByPostCount(subs []string, countsByLower map[string]int) {
	sort.SliceStable(subs, func(i, j int) bool {
		ci, cj := countsByLower[strings.ToLower(subs[i])], countsByLower[strings.ToLower(subs[j])]
		if ci != cj {
			return ci > cj
		}
		return subs[i] < subs[j]
	})
}

// nextIconBatch pops the next pseudo-random 1..iconMaxPerCycle subs from the
// current round, building a fresh round if the queue is empty. Returns nil if
// nothing is eligible. Both the hourly tick and passive /archive triggers go
// through this single drain point, so rapid passive triggers consume from one
// round instead of restarting it — non-overlapping coverage by construction.
func (s *Scheduler) nextIconBatch() []string {
	s.iconMu.Lock()
	defer s.iconMu.Unlock()

	if len(s.iconRound) == 0 {
		round := s.buildIconRound()
		if len(round) == 0 {
			return nil
		}
		s.iconRound = round
		s.Events.Addf(LevelInfo, "L4", "new round queued: %d sub(s) ordered by archive size: %s", len(round), previewList(round))
	}

	n := iconMinPerCycle + rand.Intn(iconMaxPerCycle-iconMinPerCycle+1)
	if n > len(s.iconRound) {
		n = len(s.iconRound)
	}
	batch := make([]string, n)
	copy(batch, s.iconRound[:n])
	s.iconRound = s.iconRound[n:]
	return batch
}

// iconLoop runs on startup and every 1h, draining one batch from the current
// L4 round queue per tick.
func (s *Scheduler) iconLoop(ctx context.Context) {
	if s.iconStore == nil {
		s.Events.Add(LevelSkip, "L4", "icon store not configured, L4 disabled")
		return
	}

	s.Events.Add(LevelInfo, "L4", "icon layer started, running initial check")
	s.runIconBatch(ctx, "tick")

	ticker := time.NewTicker(iconCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.runIconBatch(ctx, "tick")
		case <-ctx.Done():
			return
		}
	}
}

// CheckIconsPassive is called when a user visits /archive (passive L4 trigger).
// It drains a single batch from the shared round queue — never builds extra
// rounds on its own, so spammy /archive views don't multiply upstream load.
func (s *Scheduler) CheckIconsPassive() {
	if s.iconStore == nil {
		return
	}
	s.runIconBatch(context.Background(), "passive")
}

func (s *Scheduler) runIconBatch(ctx context.Context, source string) {
	batch := s.nextIconBatch()
	if len(batch) == 0 {
		if source == "tick" {
			s.Events.Add(LevelOK, "L4", "tick: round queue empty, nothing to refresh")
		}
		return
	}
	log.Printf("L4 %s: fetching %v", source, batch)
	s.Events.Addf(LevelInfo, "L4", "%s: %d sub(s) batched: %v", source, len(batch), batch)
	for _, sub := range batch {
		if err := ctx.Err(); err != nil {
			return
		}
		s.fetchAndSaveIcon(ctx, sub)
	}
}

func (s *Scheduler) fetchAndSaveIcon(ctx context.Context, sub string) {
	var iconURL string
	var fetchErr error

	label := "L4 r/" + sub + " icon fetch"
	err := s.submit(ctx, label, false, func(ctx context.Context) {
		// Prefer the public endpoint to conserve session-token budget, but
		// Reddit now returns 403 for logged-out /about.json requests, so on
		// any public failure fall back to the authenticated OAuth client — the
		// same host and session identity the rest of NP already rides on.
		var about reddit.Subreddit
		var aboutErr error
		if s.publicCli != nil {
			about, aboutErr = s.publicCli.FetchSubredditAbout(ctx, sub)
			s.recordUpstream(ctx)
		} else {
			aboutErr = fmt.Errorf("no public client available")
		}
		if aboutErr != nil && s.cli != nil {
			s.Events.Addf(LevelWarn, "L4", "r/%s: public icon fetch failed (%v), falling back to authenticated API", sub, aboutErr)
			about, aboutErr = s.cli.FetchSubredditAbout(ctx, sub)
			s.recordUpstream(ctx)
		}
		if aboutErr != nil {
			fetchErr = aboutErr
			s.Events.Addf(LevelError, "L4", "r/%s: icon fetch failed: %v", sub, aboutErr)
			return
		}
		iconURL = about.RawIcon
		s.Events.Addf(LevelOK, "L4", "r/%s: got icon URL: %q", sub, iconURL)

		// Piggy-back: persist the about JSON with its own 60-day expiry.
		// The icon scheduler runs more often than about expires, so this
		// keeps about data fresh "for free" without any extra upstream cost.
		if data, jerr := json.Marshal(about); jerr == nil {
			if serr := s.iconStore.SaveAbout(sub, data); serr != nil {
				log.Printf("L4: r/%s: save about failed: %v", sub, serr)
			}
		}
	})
	if err != nil {
		return
	}
	// A transient fetch failure must NOT write a has_icon verdict — leave the
	// row untouched so the next round retries normally.
	if fetchErr != nil {
		return
	}

	ttl := s.iconStore.IconTTL()
	now := time.Now()

	// Upstream answered. An empty RawIcon is the terminal "this sub has no
	// icon" verdict (e.g. r/golang). Persist has_icon=false so future rounds
	// skip it without spending another /about.json call.
	icon := &store.SubIcon{
		Name:      sub,
		IconURL:   iconURL,
		FetchedAt: now,
		ExpiresAt: now.Add(ttl),
		HasIcon:   iconURL != "",
	}

	if iconURL != "" && s.media != nil {
		dlLabel := "L4 r/" + sub + " icon download"
		s.submit(ctx, dlLabel, false, func(ctx context.Context) {
			if dlErr := s.media.DownloadMedia(ctx, iconURL); dlErr != nil {
				s.Events.Addf(LevelWarn, "L4", "r/%s: icon download failed: %v", sub, dlErr)
			}
		})
	}

	if err := s.iconStore.Save(icon); err != nil {
		s.Events.Addf(LevelError, "L4", "r/%s: save icon failed: %v", sub, err)
		log.Printf("L4: r/%s: save icon failed: %v", sub, err)
	} else if !icon.HasIcon {
		s.Events.Addf(LevelOK, "L4", "r/%s: no icon upstream, marked has_icon=false", sub)
		log.Printf("L4: r/%s: no icon upstream, marked has_icon=false", sub)
	} else {
		s.Events.Addf(LevelOK, "L4", "r/%s: icon saved (expires %s)", sub, icon.ExpiresAt.Format("2006-01-02"))
		log.Printf("L4: r/%s: icon saved, expires %s", sub, icon.ExpiresAt.Format("2006-01-02"))
	}
}
