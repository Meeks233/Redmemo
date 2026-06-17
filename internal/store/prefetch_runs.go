package store

import (
	"database/sql"
	"fmt"
	"time"
)

// PrefetchRunStore writes the unified L1/L2/L3 run ledger introduced by
// migration v31. Every layer emits a single row when work is scheduled and
// updates it again on completion, so debug queries can ask one table for the
// state of any layer instead of joining a settings blob with implicit
// in-memory counters.
type PrefetchRunStore struct {
	db *sql.DB
}

func NewPrefetchRunStore(db *sql.DB) *PrefetchRunStore {
	return &PrefetchRunStore{db: db}
}

type PrefetchRun struct {
	ID          int64
	Layer       string
	Bucket      sql.NullString
	Subreddit   sql.NullString
	PostID      sql.NullString
	CycleID     sql.NullString
	SubInterval sql.NullInt32
	ScheduledAt time.Time
	StartedAt   sql.NullTime
	FinishedAt  sql.NullTime
	Status      string
	Payload     []byte
	Error       sql.NullString
}

// Schedule inserts a row in 'pending' status and returns its id. Callers
// supply scheduledAt explicitly so L2's pre-computed wave offsets land on the
// row with no clock skew between scheduling and the eventual fire.
func (s *PrefetchRunStore) Schedule(layer, bucket, subreddit, postID, cycleID string, subInterval int, scheduledAt time.Time, payload []byte) (int64, error) {
	var id int64
	err := s.db.QueryRow(`
		INSERT INTO prefetch_runs
		    (layer, bucket, subreddit, post_id, cycle_id, sub_interval, scheduled_at, status, payload)
		VALUES ($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,''),
		        NULLIF($6, 0), $7, 'pending', $8)
		RETURNING id`,
		layer, bucket, subreddit, postID, cycleID, subInterval, scheduledAt, payload,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("schedule prefetch run: %w", err)
	}
	return id, nil
}

// MarkRunning stamps started_at and flips status to 'running'. Used by L2
// waves when their offset elapses so /debug can distinguish queued vs in
// flight without reading an in-memory ring buffer.
func (s *PrefetchRunStore) MarkRunning(id int64) error {
	_, err := s.db.Exec(`
		UPDATE prefetch_runs
		   SET status     = 'running',
		       started_at = COALESCE(started_at, NOW())
		 WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark prefetch run running: %w", err)
	}
	return nil
}

// MarkFinished writes the terminal status (ok|fail|skipped) and stamps
// finished_at. errStr is stored on 'fail'; 'ok'/'skipped' may pass "".
func (s *PrefetchRunStore) MarkFinished(id int64, status, errStr string) error {
	_, err := s.db.Exec(`
		UPDATE prefetch_runs
		   SET status      = $2,
		       finished_at = NOW(),
		       error       = NULLIF($3, '')
		 WHERE id = $1`, id, status, errStr)
	if err != nil {
		return fmt.Errorf("mark prefetch run finished: %w", err)
	}
	return nil
}

// Record is a one-shot insert for layers that have no separate schedule and
// execute step (L1 fetch outcomes, on-demand L3 fetches). The row lands with
// started_at = finished_at = NOW().
func (s *PrefetchRunStore) Record(layer, bucket, subreddit, postID, cycleID, status, errStr string, payload []byte) error {
	_, err := s.db.Exec(`
		INSERT INTO prefetch_runs
		    (layer, bucket, subreddit, post_id, cycle_id,
		     scheduled_at, started_at, finished_at, status, payload, error)
		VALUES ($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,''),
		        NOW(), NOW(), NOW(), $6, $7, NULLIF($8,''))`,
		layer, bucket, subreddit, postID, cycleID, status, payload, errStr,
	)
	if err != nil {
		return fmt.Errorf("record prefetch run: %w", err)
	}
	return nil
}

// ListPending returns every row still in 'pending' status. Used at scheduler
// startup to revive L2/L3 waves whose in-memory goroutine died with the
// previous container — once Schedule() persisted the row, the wave must fire
// regardless of process death.
func (s *PrefetchRunStore) ListPending() ([]PrefetchRun, error) {
	rows, err := s.db.Query(`
		SELECT id, layer, bucket, subreddit, post_id, cycle_id, sub_interval,
		       scheduled_at, started_at, finished_at, status, payload, error
		  FROM prefetch_runs
		 WHERE status = 'pending'
		 ORDER BY scheduled_at`)
	if err != nil {
		return nil, fmt.Errorf("list pending prefetch runs: %w", err)
	}
	defer rows.Close()
	var out []PrefetchRun
	for rows.Next() {
		var r PrefetchRun
		if err := rows.Scan(&r.ID, &r.Layer, &r.Bucket, &r.Subreddit, &r.PostID,
			&r.CycleID, &r.SubInterval, &r.ScheduledAt, &r.StartedAt, &r.FinishedAt,
			&r.Status, &r.Payload, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListWavesForActiveCycles returns every L2/L3 row belonging to a cycle that
// still has at least one pending wave. Used at reclaim time so the in-memory
// L2 cycle snapshot displayed on /debug can be rebuilt with the original 5
// wave offsets and the correct current-wave pointer (= count of waves that
// already completed before the restart).
func (s *PrefetchRunStore) ListWavesForActiveCycles() ([]PrefetchRun, error) {
	rows, err := s.db.Query(`
		SELECT id, layer, bucket, subreddit, post_id, cycle_id, sub_interval,
		       scheduled_at, started_at, finished_at, status, payload, error
		  FROM prefetch_runs
		 WHERE layer IN ('L2','L3')
		   AND sub_interval IS NOT NULL
		   AND cycle_id IN (
		       SELECT cycle_id FROM prefetch_runs
		        WHERE status = 'pending' AND layer IN ('L2','L3')
		          AND cycle_id IS NOT NULL
		   )
		 ORDER BY cycle_id, sub_interval`)
	if err != nil {
		return nil, fmt.Errorf("list waves for active cycles: %w", err)
	}
	defer rows.Close()
	var out []PrefetchRun
	for rows.Next() {
		var r PrefetchRun
		if err := rows.Scan(&r.ID, &r.Layer, &r.Bucket, &r.Subreddit, &r.PostID,
			&r.CycleID, &r.SubInterval, &r.ScheduledAt, &r.StartedAt, &r.FinishedAt,
			&r.Status, &r.Payload, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TryMarkRunning is MarkRunning guarded by a status='pending' predicate. It
// returns true only if the row was still pending at the moment of the update
// — used by wave fire paths so a row that was superseded by a fresher L1
// cycle (status flipped to 'skipped' under the goroutine's feet) cannot be
// resurrected by an in-flight wave.
func (s *PrefetchRunStore) TryMarkRunning(id int64) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE prefetch_runs
		   SET status     = 'running',
		       started_at = COALESCE(started_at, NOW())
		 WHERE id = $1 AND status = 'pending'`, id)
	if err != nil {
		return false, fmt.Errorf("try mark prefetch run running: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SupersedePending marks all pending L2/L3 rows for (layer, bucket, subreddit)
// whose cycle_id differs from keepCycleID as 'skipped' with the supplied
// reason. Called at the start of each fresh L2/L3 cycle so any previous-cycle
// waves left over from a crashed goroutine or an in-flight earlier cycle are
// discarded — L2/L3 work is bound to the most recent L1 fetch only.
func (s *PrefetchRunStore) SupersedePending(layer, bucket, subreddit, keepCycleID, reason string) (int64, error) {
	res, err := s.db.Exec(`
		UPDATE prefetch_runs
		   SET status      = 'skipped',
		       finished_at = NOW(),
		       error       = NULLIF($5,'')
		 WHERE status = 'pending'
		   AND layer = $1
		   AND bucket = NULLIF($2,'')
		   AND subreddit = NULLIF($3,'')
		   AND (cycle_id IS DISTINCT FROM NULLIF($4,''))`,
		layer, bucket, subreddit, keepCycleID, reason)
	if err != nil {
		return 0, fmt.Errorf("supersede pending prefetch runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PreviousL3CycleID returns the cycle_id of the L3 cycle for `subreddit` whose
// scheduled_at is the most recent strictly earlier than currentCycleID's. Used
// by L3 cycle-freeze dedup, now keyed on L3's OWN lineage rather than L1's: a
// post archived in the *previous* L3 cycle stays frozen for the current one
// (and unfreezes automatically two L3 cycles later when this query rolls
// forward). Returns "" with no error when no prior L3 cycle exists yet —
// callers treat that as "no freeze prior to current".
//
// Only cycles that actually ran count as "previous": 'skipped' (superseded /
// orphaned) and still-'pending' wave rows are excluded. This matters because a
// superseded cycle's wave rows keep their original (possibly future)
// scheduled_at, so without the filter such a no-op cycle could outrank the real
// previous cycle by MAX(scheduled_at) and silently disable the freeze (no post
// ever carries a skipped cycle's id, so freezing on it is a no-op → redundant
// comment re-fetches).
func (s *PrefetchRunStore) PreviousL3CycleID(subreddit, currentCycleID string) (string, error) {
	var prev sql.NullString
	err := s.db.QueryRow(`
		SELECT cycle_id
		  FROM prefetch_runs
		 WHERE layer = 'L3'
		   AND LOWER(subreddit) = LOWER($1)
		   AND cycle_id IS NOT NULL
		   AND cycle_id <> $2
		   AND status NOT IN ('skipped', 'pending')
		 GROUP BY cycle_id
		 ORDER BY MAX(scheduled_at) DESC
		 LIMIT 1`, subreddit, currentCycleID,
	).Scan(&prev)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("previous L3 cycle id: %w", err)
	}
	if !prev.Valid {
		return "", nil
	}
	return prev.String, nil
}

// LastCyclePostCount returns the post_count of the most recent L1-lineage cycle
// for (bucket, subreddit) — i.e. how many posts the last L1 listing round
// surfaced. reconcileLoop uses it to size a regenerated L2/L3 cycle the same way
// a real L1 round would, instead of over-counting the full archive backlog.
//
// It deliberately excludes standalone L3 cycles (cycle_id 'L3:%'): their
// post_count is itself derived from an L1 round, so reading it back to size the
// next regenerated L3 cycle would be circular (and would perpetuate a bad value
// once one slipped in). Only the L1/L2-lineage cycle_ids ('<tf>:<sub>:<unix>')
// carry the authoritative round size. Returns 0 (no error) when none exists yet.
func (s *PrefetchRunStore) LastCyclePostCount(bucket, subreddit string) (int, error) {
	var pc sql.NullInt64
	err := s.db.QueryRow(`
		SELECT COALESCE(NULLIF(payload->>'post_count', '')::int, 0)
		  FROM prefetch_runs
		 WHERE LOWER(subreddit) = LOWER($2)
		   AND (bucket = $1 OR $1 = '')
		   AND (cycle_id IS NULL OR cycle_id NOT LIKE 'L3:%')
		   AND payload ? 'post_count'
		   AND COALESCE(NULLIF(payload->>'post_count', '')::int, 0) > 0
		 ORDER BY id DESC
		 LIMIT 1`, bucket, subreddit,
	).Scan(&pc)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("last cycle post_count: %w", err)
	}
	return int(pc.Int64), nil
}

// FailStaleRunning terminates rows left in 'running' state by a dead process.
// If a wave was actually mid-fetch when the container died, its row was stuck
// at 'running' forever — flip those to 'fail' with a marker error so the
// ledger reflects reality and the ID is no longer ambiguous.
func (s *PrefetchRunStore) FailStaleRunning() (int64, error) {
	res, err := s.db.Exec(`
		UPDATE prefetch_runs
		   SET status      = 'fail',
		       finished_at = NOW(),
		       error       = COALESCE(error, 'container restarted while running')
		 WHERE status = 'running'`)
	if err != nil {
		return 0, fmt.Errorf("fail stale running runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountByLayerSince is a small helper for the debug page: how many rows of
// each layer landed in the last window. Returns zero for layers with no rows.
func (s *PrefetchRunStore) CountByLayerSince(window time.Duration) (map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT layer, COUNT(*)
		  FROM prefetch_runs
		 WHERE scheduled_at >= NOW() - $1::interval
		 GROUP BY layer`, fmt.Sprintf("%d seconds", int(window.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("count prefetch runs: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var layer string
		var n int
		if err := rows.Scan(&layer, &n); err != nil {
			return nil, err
		}
		out[layer] = n
	}
	return out, rows.Err()
}
