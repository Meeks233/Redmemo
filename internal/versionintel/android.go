package versionintel

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FallbackAndroidVersion is the Android major version used when no popular-
// version data is available — notably the first-boot device, before any
// StatCounter poll has run.
const FallbackAndroidVersion = 15

const statCounterChartURL = "https://gs.statcounter.com/chart.php"

// browserUserAgent is sent with the StatCounter request: the CSV export
// rejects clients with a non-browser User-Agent (Go's default 403s).
const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// statCounterURL builds the no-auth CSV export URL for the worldwide monthly
// Android-version breakdown covering the window ending at `now`.
func statCounterURL(now time.Time) string {
	to := now
	from := now.AddDate(0, -3, 0)
	return fmt.Sprintf("%s?device=Mobile&device_hidden=mobile&statType_hidden=android_version"+
		"&region_hidden=ww&granularity=monthly&statType=Android+Version&region=Worldwide"+
		"&fromInt=%04d%02d&toInt=%04d%02d&csv=1",
		statCounterChartURL, from.Year(), int(from.Month()), to.Year(), int(to.Month()))
}

var leadingIntRe = regexp.MustCompile(`^\s*(\d+)`)

// FetchPopularAndroidVersion downloads the StatCounter worldwide Android-
// version breakdown and returns the major version with the largest install
// base in the latest month.
func FetchPopularAndroidVersion(ctx context.Context, client *http.Client, now time.Time) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statCounterURL(now), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("statcounter fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("statcounter: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("statcounter read: %w", err)
	}
	shares, err := parseAndroidShares(body)
	if err != nil {
		return 0, err
	}
	return mostPopular(shares), nil
}

// mostPopular returns the version with the highest share; ties break toward
// the newer version.
func mostPopular(shares map[int]float64) int {
	best, bestShare := 0, -1.0
	for v, s := range shares {
		if s > bestShare || (s == bestShare && v > best) {
			best, bestShare = v, s
		}
	}
	return best
}

// parseAndroidShares parses a StatCounter chart CSV: a header row of version
// labels (column 0 is "Date") followed by one row per month. The rows are NOT
// strictly date-ordered — StatCounter appends trailing all-zero rows for
// pre-launch months — so the most recent month is found by the largest Date
// value, not the last row. Shares for the same major version (e.g. "8.0" and
// "8.1") are summed. Values are returned keyed by major version.
func parseAndroidShares(body []byte) (map[int]float64, error) {
	r := csv.NewReader(strings.NewReader(string(body)))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("statcounter csv: %w", err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("statcounter csv: no data rows")
	}

	header := records[0]
	// Pick the row with the latest Date (YYYY-MM sorts lexically).
	latest := records[1]
	for _, rec := range records[2:] {
		if len(rec) > 0 && len(latest) > 0 && rec[0] > latest[0] {
			latest = rec
		}
	}

	shares := map[int]float64{}
	for i, label := range header {
		if i >= len(latest) {
			break
		}
		m := leadingIntRe.FindStringSubmatch(label)
		if m == nil { // "Date", "Other", etc.
			continue
		}
		v, _ := strconv.Atoi(m[1])
		share, err := strconv.ParseFloat(strings.TrimSpace(latest[i]), 64)
		if err != nil {
			continue
		}
		shares[v] += share
	}
	if len(shares) == 0 {
		return nil, fmt.Errorf("statcounter csv: no version columns recognized")
	}
	return shares, nil
}
