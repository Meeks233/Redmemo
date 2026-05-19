package store

import (
	"database/sql"
	"fmt"
	"time"
)

// DeviceProfile is the spoofed Android device identity. Exactly one row exists
// (id = 1).
//
// The device_id and android_version are stable for the life of a "device" and
// change only at a major rotation — every ~3 years, modelling the user
// replacing their phone. The app version (AppVersion/Build) re-derives on
// every token mint (the minor rotation). UserAgent is rebuilt whenever any of
// these moves. See internal/versionintel.
type DeviceProfile struct {
	DeviceID       string
	UserAgent      string
	AndroidVersion int
	AppVersion     string
	Build          string
	CreatedAt      time.Time

	// DeviceBornAt is when the current device identity was created (reset at
	// every major rotation); DeviceLifespanDays is its randomized ~3-year life.
	DeviceBornAt       time.Time
	DeviceLifespanDays int
	// NextAndroidVersion is the Android version a monthly StatCounter poll has
	// scheduled for the next major rotation (0 = nothing scheduled).
	NextAndroidVersion int
	// OSNextCheckAt gates the monthly popular-version poll.
	OSNextCheckAt time.Time
}

type DeviceProfileStore struct {
	db *sql.DB
}

func NewDeviceProfileStore(db *sql.DB) *DeviceProfileStore {
	return &DeviceProfileStore{db: db}
}

// Get returns the pinned profile, or nil if none has been created yet.
func (s *DeviceProfileStore) Get() (*DeviceProfile, error) {
	p := &DeviceProfile{}
	err := s.db.QueryRow(`
		SELECT device_id, user_agent, android_version, app_version, build, created_at,
		       device_born_at, device_lifespan_days, next_android_version, os_next_check_at
		FROM device_profile
		WHERE id = 1`,
	).Scan(
		&p.DeviceID, &p.UserAgent, &p.AndroidVersion, &p.AppVersion, &p.Build, &p.CreatedAt,
		&p.DeviceBornAt, &p.DeviceLifespanDays, &p.NextAndroidVersion, &p.OSNextCheckAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get device profile: %w", err)
	}
	return p, nil
}

// Insert creates the single device_profile row. It is a no-op if the row
// already exists, so the profile is written exactly once on first boot.
func (s *DeviceProfileStore) Insert(p *DeviceProfile) error {
	_, err := s.db.Exec(`
		INSERT INTO device_profile (id, device_id, user_agent, android_version, app_version, build,
		                            device_born_at, device_lifespan_days, next_android_version, os_next_check_at)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO NOTHING`,
		p.DeviceID, p.UserAgent, p.AndroidVersion, p.AppVersion, p.Build,
		p.DeviceBornAt, p.DeviceLifespanDays, p.NextAndroidVersion, p.OSNextCheckAt,
	)
	if err != nil {
		return fmt.Errorf("insert device profile: %w", err)
	}
	return nil
}

// Update persists the mutable fields of the profile. device_id IS updated
// here — a major rotation mints a new one. created_at is never touched.
func (s *DeviceProfileStore) Update(p *DeviceProfile) error {
	_, err := s.db.Exec(`
		UPDATE device_profile SET
			device_id            = $1,
			user_agent           = $2,
			android_version      = $3,
			app_version          = $4,
			build                = $5,
			device_born_at       = $6,
			device_lifespan_days = $7,
			next_android_version = $8,
			os_next_check_at     = $9
		WHERE id = 1`,
		p.DeviceID, p.UserAgent, p.AndroidVersion, p.AppVersion, p.Build,
		p.DeviceBornAt, p.DeviceLifespanDays, p.NextAndroidVersion, p.OSNextCheckAt,
	)
	if err != nil {
		return fmt.Errorf("update device profile: %w", err)
	}
	return nil
}
