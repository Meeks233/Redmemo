package legacy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/config"
)

const defaultRedlibAddr = "http://redlib:8080"

var (
	selectValueRe  = regexp.MustCompile(`<select[^>]*name="([^"]+)"[^>]*>([\s\S]*?)</select>`)
	selectedOptRe  = regexp.MustCompile(`<option[^>]*value="([^"]*)"[^>]*selected[^>]*>`)
	checkboxOnRe   = regexp.MustCompile(`<input[^>]*name="([^"]+)"[^>]*type="checkbox"[^>]*checked`)
	checkboxOnRe2  = regexp.MustCompile(`<input[^>]*type="checkbox"[^>]*name="([^"]+)"[^>]*checked`)
	checkboxOffRe  = regexp.MustCompile(`<input[^>]*name="([^"]+)"[^>]*type="checkbox"[^>]*>`)
	checkboxOffRe2 = regexp.MustCompile(`<input[^>]*type="checkbox"[^>]*name="([^"]+)"[^>]*>`)
)

type SyncResult struct {
	Settings map[string]string
	Source   string
}

func SyncSettings(cfg config.LegacyConfig) (*SyncResult, error) {
	if !cfg.SyncEnabled {
		return nil, nil
	}

	targets := buildTargetList(cfg)

	for _, addr := range targets {
		result, err := tryFetchSettings(addr)
		if err != nil {
			log.Printf("legacy: %s unreachable: %v", addr, err)
			continue
		}

		total := len(result.Settings)
		for name, value := range result.Settings {
			if config.IsSettingExplicitlySet(name) {
				explicit := config.GetExplicitSetting(name)
				log.Printf("legacy: skipping %s=%q (env override: %q)", name, value, explicit)
			}
		}
		// Actually drop the env-overridden keys from the returned map so it matches
		// the log above and honours the documented REDMEMO_* > REDLIB_* precedence
		// (otherwise the caller transiently writes the legacy value into the DB).
		result.Settings = FilterByEnv(result.Settings)

		log.Printf("legacy: synced %d settings from %s (%d applied, %d skipped by env override)",
			total, addr, len(result.Settings), total-len(result.Settings))
		return result, nil
	}

	return nil, fmt.Errorf("legacy: all sync targets unreachable (%s), skipping sync", strings.Join(targets, ", "))
}

func buildTargetList(cfg config.LegacyConfig) []string {
	if cfg.Instance == "" {
		return []string{defaultRedlibAddr}
	}

	explicit := cfg.Instance
	if !strings.HasPrefix(explicit, "http") {
		explicit = "http://" + explicit
	}

	if strings.TrimRight(explicit, "/") == strings.TrimRight(defaultRedlibAddr, "/") {
		return []string{explicit}
	}

	return []string{explicit, defaultRedlibAddr}
}

func tryFetchSettings(addr string) (*SyncResult, error) {
	settingsURL := strings.TrimRight(addr, "/") + "/settings"
	log.Printf("legacy: attempting one-time settings sync from %s", settingsURL)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(settingsURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", settingsURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s returned HTTP %d", settingsURL, resp.StatusCode)
	}

	// Cap the body: the redlib /settings page is a few KB of HTML. A broken or
	// misbehaving upstream returning a huge body would otherwise be fully buffered
	// and then scanned by several regexes.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read body from %s: %w", settingsURL, err)
	}

	settings := parseSettingsHTML(string(body))
	return &SyncResult{
		Settings: settings,
		Source:   addr,
	}, nil
}

func parseSettingsHTML(html string) map[string]string {
	settings := make(map[string]string)

	for _, match := range selectValueRe.FindAllStringSubmatch(html, -1) {
		name := match[1]
		block := match[2]
		if optMatch := selectedOptRe.FindStringSubmatch(block); len(optMatch) > 1 {
			settings[name] = optMatch[1]
		}
	}

	checkedNames := make(map[string]bool)
	for _, re := range []*regexp.Regexp{checkboxOnRe, checkboxOnRe2} {
		for _, match := range re.FindAllStringSubmatch(html, -1) {
			name := match[1]
			settings[name] = "on"
			checkedNames[name] = true
		}
	}

	for _, re := range []*regexp.Regexp{checkboxOffRe, checkboxOffRe2} {
		for _, match := range re.FindAllStringSubmatch(html, -1) {
			name := match[1]
			if !checkedNames[name] {
				if _, exists := settings[name]; !exists {
					settings[name] = ""
				}
			}
		}
	}

	return settings
}

func FilterByEnv(settings map[string]string) map[string]string {
	filtered := make(map[string]string)
	for name, value := range settings {
		if !config.IsSettingExplicitlySet(name) {
			filtered[name] = value
		}
	}
	return filtered
}
