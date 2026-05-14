package prefetch

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

const iconCheckInterval = 1 * time.Hour

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

// iconLoop runs on startup and every 1h, checking all non-dead subs' icon freshness.
func (s *Scheduler) iconLoop(ctx context.Context) {
	if s.iconStore == nil {
		s.Events.Add(LevelSkip, "L4", "icon store not configured, L4 disabled")
		return
	}

	s.Events.Add(LevelInfo, "L4", "icon layer started, running initial check")
	s.checkAndRefreshIcons(ctx)

	ticker := time.NewTicker(iconCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.checkAndRefreshIcons(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// CheckIconsPassive is called when a user visits /archive (passive L4 trigger).
// It checks freshness and fetches any missing/expired icons in the background.
func (s *Scheduler) CheckIconsPassive() {
	if s.iconStore == nil {
		return
	}

	subs := s.allAliveSubs()
	if len(subs) == 0 {
		return
	}

	now := time.Now()
	var needUpdate []string

	for _, sub := range subs {
		icon, err := s.iconStore.Get(sub)
		if err != nil {
			continue
		}
		if icon == nil || now.After(icon.ExpiresAt) {
			needUpdate = append(needUpdate, sub)
		}
	}

	if len(needUpdate) == 0 {
		return
	}

	log.Printf("L4 passive: %v need icon update, submitting to NP", needUpdate)
	s.Events.Addf(LevelInfo, "L4", "passive: %d subs need icon update, submitting to NP", len(needUpdate))

	ctx := context.Background()
	for _, sub := range needUpdate {
		s.fetchAndSaveIcon(ctx, sub)
	}
}

func (s *Scheduler) checkAndRefreshIcons(ctx context.Context) {
	subs := s.allAliveSubs()
	if len(subs) == 0 {
		s.Events.Add(LevelSkip, "L4", "no alive subs, skipping icon check")
		log.Println("L4: no alive subs, skipping icon check")
		return
	}

	now := time.Now()
	var needUpdate []string
	var upToDate []string

	for _, sub := range subs {
		icon, err := s.iconStore.Get(sub)
		if err != nil {
			needUpdate = append(needUpdate, sub)
			continue
		}
		if icon == nil || now.After(icon.ExpiresAt) {
			needUpdate = append(needUpdate, sub)
		} else {
			upToDate = append(upToDate, sub)
		}
	}

	if len(upToDate) > 0 {
		log.Printf("L4: %v do not need update", upToDate)
	}
	if len(needUpdate) > 0 {
		log.Printf("L4: %v need update, submitting to NP", needUpdate)
		s.Events.Addf(LevelInfo, "L4", "%d subs need icon update: %v, submitting to NP", len(needUpdate), needUpdate)
	} else {
		log.Println("L4: all icons up to date")
		s.Events.Add(LevelOK, "L4", "all icons up to date")
		return
	}

	for _, sub := range needUpdate {
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
		if s.publicCli == nil {
			fetchErr = fmt.Errorf("no public client available")
			s.Events.Addf(LevelError, "L4", "r/%s: no public client for icon fetch", sub)
			return
		}
		about, aboutErr := s.publicCli.FetchSubredditAbout(ctx, sub)
		if aboutErr != nil {
			fetchErr = aboutErr
			s.Events.Addf(LevelError, "L4", "r/%s: icon fetch failed: %v", sub, aboutErr)
			return
		}
		iconURL = about.RawIcon
		s.Events.Addf(LevelOK, "L4", "r/%s: got icon URL: %q", sub, iconURL)
	})
	if err != nil {
		return
	}
	if fetchErr != nil {
		return
	}

	ttl := s.iconStore.IconTTL()
	now := time.Now()

	icon := &store.SubIcon{
		Name:      sub,
		IconURL:   iconURL,
		FetchedAt: now,
		ExpiresAt: now.Add(ttl),
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
	} else {
		s.Events.Addf(LevelOK, "L4", "r/%s: icon saved (expires %s)", sub, icon.ExpiresAt.Format("2006-01-02"))
		log.Printf("L4: r/%s: icon saved, expires %s", sub, icon.ExpiresAt.Format("2006-01-02"))
	}
}
