package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"

	"uptime-battle/internal/protocol"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS machines (
	id                TEXT PRIMARY KEY,
	name              TEXT NOT NULL,
	name_key          TEXT NOT NULL UNIQUE,
	public_key        BLOB NOT NULL UNIQUE,
	created_at        INTEGER NOT NULL,
	boot_hash         TEXT NOT NULL DEFAULT '',
	streak_start      INTEGER NOT NULL DEFAULT 0,
	last_seen         INTEGER NOT NULL DEFAULT 0,
	last_uptime       INTEGER NOT NULL DEFAULT 0,
	last_challenge    INTEGER NOT NULL DEFAULT 0,
	gap_credit        INTEGER NOT NULL DEFAULT 0,
	best_ms           INTEGER NOT NULL DEFAULT 0,
	total_ms          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS machines_boot ON machines(boot_hash);
CREATE INDEX IF NOT EXISTS machines_best ON machines(best_ms DESC);

CREATE TABLE IF NOT EXISTS streaks (
	id         INTEGER PRIMARY KEY,
	machine_id TEXT NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	started_at INTEGER NOT NULL,
	ended_at   INTEGER NOT NULL,
	reason     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS streaks_machine ON streaks(machine_id, id DESC);
`

// historyPerMachine bounds the rows a misbehaving agent can create.
const historyPerMachine = 20

var (
	ErrNameTaken    = errors.New("name already taken")
	ErrKeyTaken     = errors.New("key already enrolled")
	ErrFull         = errors.New("server is full")
	ErrUnknown      = errors.New("unknown machine")
	ErrBadSignature = errors.New("invalid signature")
	ErrReplay       = errors.New("challenge already used")
	ErrTooSoon      = errors.New("heartbeat too soon")
	ErrDuplicate    = errors.New("this boot already runs under another machine")
)

// Store owns all persistent state. Writes go through rw, limited to a single
// connection so each transaction is serialised; pages read from ro.
type Store struct {
	rw, ro *sql.DB
	cfg    Config
	now    func() time.Time
}

func OpenStore(cfg Config, now func() time.Time) (*Store, error) {
	dsn := "file:" + (&url.URL{Path: cfg.DBPath}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"
	rw, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	rw.SetMaxOpenConns(1)
	if _, err := rw.Exec(schema); err != nil {
		rw.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	ro, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
	if err != nil {
		rw.Close()
		return nil, err
	}
	ro.SetMaxOpenConns(4)
	return &Store{rw: rw, ro: ro, cfg: cfg, now: now}, nil
}

func (s *Store) Close() {
	s.ro.Close()
	s.rw.Close()
}

// Secret returns the challenge HMAC key, creating it on first start.
func (s *Store) Secret() ([]byte, error) {
	fresh := make([]byte, 32)
	rand.Read(fresh)
	if _, err := s.rw.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('secret', ?)`, fresh); err != nil {
		return nil, err
	}
	var secret []byte
	err := s.rw.QueryRow(`SELECT value FROM meta WHERE key = 'secret'`).Scan(&secret)
	return secret, err
}

// Tick records that the server is alive.
func (s *Store) Tick() error {
	_, err := s.rw.Exec(`INSERT INTO meta(key, value) VALUES ('alive', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, s.now().UnixMilli())
	return err
}

// CreditDowntime runs at startup. Machines cannot reach a stopped server, so
// the time it was down must not count as silence on their side.
func (s *Store) CreditDowntime() (time.Duration, error) {
	var alive int64
	err := s.rw.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'alive'`).Scan(&alive)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	down := s.now().UnixMilli() - alive
	if down <= 0 {
		return 0, nil
	}
	_, err = s.rw.Exec(`UPDATE machines SET gap_credit = gap_credit + ?
		WHERE streak_start > 0 AND last_seen + gap_credit + ? >= ?`,
		down, s.cfg.MaxGap.Milliseconds(), alive)
	return time.Duration(down) * time.Millisecond, err
}

// Enroll registers a machine whose proof of work and signature were checked.
func (s *Store) Enroll(ctx context.Context, name string, pub ed25519.PublicKey) (string, error) {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM machines`).Scan(&count); err != nil {
		return "", err
	}
	if count >= s.cfg.MaxMachines {
		return "", ErrFull
	}
	var hits int
	err = tx.QueryRow(`SELECT COUNT(*) FROM machines WHERE public_key = ?`, []byte(pub)).Scan(&hits)
	if err != nil {
		return "", err
	}
	if hits > 0 {
		return "", ErrKeyTaken
	}
	err = tx.QueryRow(`SELECT COUNT(*) FROM machines WHERE name_key = ?`, protocol.NameKey(name)).Scan(&hits)
	if err != nil {
		return "", err
	}
	if hits > 0 {
		return "", ErrNameTaken
	}
	id := protocol.MachineID(pub)
	_, err = tx.Exec(`INSERT INTO machines(id, name, name_key, public_key, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, name, protocol.NameKey(name), []byte(pub), s.now().UnixMilli())
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// BeatResult is what a player learns from an accepted heartbeat.
type BeatResult struct {
	StreakMs, BestMs int64
	Reset            string
}

// Heartbeat validates a signed beat and advances the machine's streak. The
// challenge token must already be verified; issuedMs is its timestamp.
func (s *Store) Heartbeat(ctx context.Context, req protocol.HeartbeatRequest, issuedMs int64) (BeatResult, error) {
	var res BeatResult
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	var (
		pub           []byte
		st            streak
		lastChallenge int64
	)
	err = tx.QueryRow(`SELECT public_key, boot_hash, streak_start, last_seen, last_uptime,
			gap_credit, best_ms, total_ms, last_challenge
		FROM machines WHERE id = ?`, req.MachineID).
		Scan(&pub, &st.BootHash, &st.Start, &st.LastSeen, &st.LastUptime,
			&st.GapCredit, &st.BestMs, &st.TotalMs, &lastChallenge)
	if errors.Is(err, sql.ErrNoRows) {
		return res, ErrUnknown
	}
	if err != nil {
		return res, err
	}

	msg := protocol.HeartbeatMessage(req.MachineID, req.Challenge, req.BootHash, req.UptimeMs)
	if !ed25519.Verify(pub, msg, req.Signature) {
		return res, ErrBadSignature
	}
	now := s.now().UnixMilli()
	if st.Start != 0 && now-st.LastSeen < s.cfg.MinInterval.Milliseconds() {
		return res, ErrTooSoon
	}
	if issuedMs <= lastChallenge {
		return res, ErrReplay
	}

	maxGap := s.cfg.MaxGap.Milliseconds()
	reason := st.classify(req.BootHash, req.UptimeMs, now, maxGap)
	if reason != ReasonNone {
		// Several identities on one boot would let a single machine fill the
		// board. Only a running streak holds a boot.
		var dup int
		err = tx.QueryRow(`SELECT COUNT(*) FROM machines
			WHERE boot_hash = ? AND id <> ? AND streak_start > 0
			AND last_seen + gap_credit + ? >= ?`, req.BootHash, req.MachineID, maxGap, now).Scan(&dup)
		if err != nil {
			return res, err
		}
		if dup > 0 {
			return res, ErrDuplicate
		}
		if st.Start != 0 {
			_, err = tx.Exec(`INSERT INTO streaks(machine_id, started_at, ended_at, reason) VALUES (?, ?, ?, ?)`,
				req.MachineID, st.Start, st.LastSeen, reason)
			if err != nil {
				return res, err
			}
			_, err = tx.Exec(`DELETE FROM streaks WHERE machine_id = ? AND id NOT IN
				(SELECT id FROM streaks WHERE machine_id = ? ORDER BY id DESC LIMIT ?)`,
				req.MachineID, req.MachineID, historyPerMachine)
			if err != nil {
				return res, err
			}
		}
	}

	st.apply(reason, req.BootHash, req.UptimeMs, now)
	_, err = tx.Exec(`UPDATE machines SET boot_hash = ?, streak_start = ?, last_seen = ?, last_uptime = ?,
			gap_credit = ?, best_ms = ?, total_ms = ?, last_challenge = ?
		WHERE id = ?`, st.BootHash, st.Start, st.LastSeen, st.LastUptime,
		st.GapCredit, st.BestMs, st.TotalMs, issuedMs, req.MachineID)
	if err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return BeatResult{StreakMs: st.currentMs(), BestMs: st.BestMs, Reset: reason}, nil
}

// Purge frees names held by machines that left without ever scoring much.
func (s *Store) Purge() error {
	cutoff := s.now().Add(-30 * 24 * time.Hour).UnixMilli()
	_, err := s.rw.Exec(`DELETE FROM machines WHERE max(created_at, last_seen) < ? AND best_ms < ?`,
		cutoff, time.Hour.Milliseconds())
	return err
}

// Entry is one machine as shown to the public. It deliberately has no boot hash
// or key: the boot hash must stay private or anyone could squat a boot.
type Entry struct {
	Name       string
	CreatedAt  int64
	Start      int64
	LastSeen   int64
	LastUptime int64
	GapCredit  int64
	BestMs     int64
	TotalMs    int64
}

const entryColumns = `name, created_at, streak_start, last_seen, last_uptime, gap_credit, best_ms, total_ms`

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	err := row.Scan(&e.Name, &e.CreatedAt, &e.Start, &e.LastSeen, &e.LastUptime, &e.GapCredit, &e.BestMs, &e.TotalMs)
	return e, err
}

func (s *Store) queryEntries(ctx context.Context, query string, args ...any) ([]Entry, error) {
	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Racing lists machines whose streak is still alive, longest first.
func (s *Store) Racing(ctx context.Context) ([]Entry, error) {
	return s.queryEntries(ctx, `SELECT `+entryColumns+` FROM machines
		WHERE streak_start > 0 AND last_seen + gap_credit + ? >= ?
		ORDER BY last_seen - streak_start DESC, created_at ASC`,
		s.cfg.MaxGap.Milliseconds(), s.now().UnixMilli())
}

// Records lists the best streaks ever.
func (s *Store) Records(ctx context.Context, limit int) ([]Entry, error) {
	return s.queryEntries(ctx, `SELECT `+entryColumns+` FROM machines WHERE best_ms > 0
		ORDER BY best_ms DESC, created_at ASC LIMIT ?`, limit)
}

// Totals are the site-wide figures.
type Totals struct {
	Machines int
	TotalMs  int64
}

func (s *Store) Totals(ctx context.Context) (Totals, error) {
	var t Totals
	err := s.ro.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(total_ms), 0) FROM machines`).Scan(&t.Machines, &t.TotalMs)
	return t, err
}

// Machine fetches one machine by name.
func (s *Store) Machine(ctx context.Context, name string) (Entry, error) {
	e, err := scanEntry(s.ro.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM machines WHERE name_key = ?`, protocol.NameKey(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrUnknown
	}
	return e, err
}

// PastStreak is a finished streak.
type PastStreak struct {
	Start, End int64
	Reason     string
}

func (s *Store) History(ctx context.Context, name string) ([]PastStreak, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT started_at, ended_at, reason FROM streaks
		WHERE machine_id = (SELECT id FROM machines WHERE name_key = ?)
		ORDER BY id DESC`, protocol.NameKey(name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PastStreak
	for rows.Next() {
		var p PastStreak
		if err := rows.Scan(&p.Start, &p.End, &p.Reason); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
