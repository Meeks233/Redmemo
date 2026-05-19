package versionintel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

// fakeTransport serves a canned body (or an error) for any request.
type fakeTransport struct {
	body string
	err  bool
}

func (f fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if f.err {
		return nil, fmt.Errorf("offline")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func trackerWith(now time.Time, rt http.RoundTripper) *Tracker {
	return &Tracker{
		httpClient: &http.Client{Transport: rt},
		now:        func() time.Time { return now },
	}
}

func TestTracker_Rotate_MinorOnly(t *testing.T) {
	now := date(2026, 12, 1)
	tr := trackerWith(now, fakeTransport{err: true})
	p := store.DeviceProfile{
		DeviceID: "dev-1", AndroidVersion: 15, AppVersion: "2026.06.0", Build: "2606110",
		DeviceBornAt: now.AddDate(0, -6, 0), DeviceLifespanDays: 1095,
		OSNextCheckAt: now.Add(24 * time.Hour), // future — no poll
	}
	got, changed := tr.Rotate(context.Background(), p)
	if !changed {
		t.Fatal("expected an app-version minor rotation")
	}
	if got.DeviceID != "dev-1" || got.AndroidVersion != 15 {
		t.Error("device_id / android version must not change outside a major rotation")
	}
}

func TestTracker_Rotate_PollSchedulesNextAndroid(t *testing.T) {
	now := date(2026, 6, 1)
	// 16 is the install-base leader (40%) but sits in a middle column and is
	// not the last row — exercising real argmax + latest-month selection.
	csv := "Date,12.0,16.0,13.0,14.0,15.0,Other\n" +
		"2026-04,8.0,30.0,15.0,20.0,18.0,9.0\n" +
		"2026-05,6.0,40.0,12.0,19.0,17.0,6.0\n"
	tr := trackerWith(now, fakeTransport{body: csv})
	p := store.DeviceProfile{
		DeviceID: "dev-1", AndroidVersion: 15, AppVersion: "2026.06.0", Build: "2606110",
		DeviceBornAt: now.AddDate(0, -6, 0), DeviceLifespanDays: 1095,
		OSNextCheckAt: now.Add(-time.Hour), // due
	}
	got, _ := tr.Rotate(context.Background(), p)
	if got.NextAndroidVersion != 16 {
		t.Errorf("NextAndroidVersion = %d, want 16 (true install-base leader > current)", got.NextAndroidVersion)
	}
	if !got.OSNextCheckAt.After(now) {
		t.Error("OSNextCheckAt should be rescheduled forward")
	}
}

func TestTracker_Rotate_PollIgnoresOlderPopular(t *testing.T) {
	now := date(2026, 6, 1)
	// Most popular is 14 — below the current 15, so nothing is scheduled.
	csv := "Date,13.0,14.0,15.0\n2026-05,20.0,55.0,25.0\n"
	tr := trackerWith(now, fakeTransport{body: csv})
	p := store.DeviceProfile{
		DeviceID: "dev-1", AndroidVersion: 15, AppVersion: "2026.06.0", Build: "2606110",
		DeviceBornAt: now.AddDate(0, -6, 0), DeviceLifespanDays: 1095,
		OSNextCheckAt: now.Add(-time.Hour),
	}
	got, _ := tr.Rotate(context.Background(), p)
	if got.NextAndroidVersion != 0 {
		t.Errorf("NextAndroidVersion = %d, want 0 (popular version not newer)", got.NextAndroidVersion)
	}
}

func TestTracker_Rotate_MajorRotation(t *testing.T) {
	now := date(2029, 6, 1)
	tr := trackerWith(now, fakeTransport{err: true})
	p := store.DeviceProfile{
		DeviceID: "old-device", AndroidVersion: 15, AppVersion: "2029.06.0", Build: "2906100",
		DeviceBornAt: now.AddDate(-4, 0, 0), DeviceLifespanDays: 1095, // 4y old, ~3y life
		NextAndroidVersion: 17,
		OSNextCheckAt:      now.Add(24 * time.Hour),
	}
	got, changed := tr.Rotate(context.Background(), p)
	if !changed {
		t.Fatal("expected a major rotation")
	}
	if got.DeviceID == "old-device" {
		t.Error("major rotation must mint a new device_id")
	}
	if got.AndroidVersion != 17 {
		t.Errorf("android = %d, want 17 (scheduled version adopted)", got.AndroidVersion)
	}
	if got.NextAndroidVersion != 0 {
		t.Error("scheduled version should be cleared after adoption")
	}
	if !got.DeviceBornAt.Equal(now) {
		t.Error("device birth should reset to now")
	}
	if got.DeviceLifespanDays < deviceLifespanMinDays || got.DeviceLifespanDays > deviceLifespanMaxDays {
		t.Errorf("lifespan = %d, want re-seeded in band", got.DeviceLifespanDays)
	}
	if got.UserAgent != BuildUserAgent(got.AppVersion, got.Build, got.AndroidVersion) {
		t.Errorf("UserAgent not rebuilt: %q", got.UserAgent)
	}
}

func TestTracker_Rotate_MajorRotationNoSchedule(t *testing.T) {
	now := date(2029, 6, 1)
	tr := trackerWith(now, fakeTransport{err: true})
	p := store.DeviceProfile{
		DeviceID: "old-device", AndroidVersion: 15, AppVersion: "2029.06.0", Build: "2906100",
		DeviceBornAt: now.AddDate(-4, 0, 0), DeviceLifespanDays: 1095,
		NextAndroidVersion: 0, // nothing was ever scheduled
		OSNextCheckAt:      now.Add(24 * time.Hour),
	}
	got, _ := tr.Rotate(context.Background(), p)
	if got.AndroidVersion != 15 {
		t.Errorf("android = %d, want 15 kept (no scheduled upgrade)", got.AndroidVersion)
	}
	if got.DeviceID == "old-device" {
		t.Error("device_id should still rotate even with no OS upgrade")
	}
}
