// Package streamlog is the machine-local event log behind `hap stream
// orchestrator`: an append-only, sequence-numbered record of the changes an
// orchestrating agent reacts to (see domain.StreamEvent).
//
// It is a SQLite file of its own in the state directory, deliberately NOT a
// table in the main store:
//
//   - The stream describes what happened ON THIS MACHINE, the way config.toml
//     does, so it has no business in a database several nodes share. A table
//     there would need node scoping, schema-lease DDL under turso, and would
//     arm one debounced Turso Cloud push per event.
//   - Every hap process on the machine can open a plain file, including a CLI
//     under the turso engine, where only the daemon may open the store.
//
// The counter is SQLite's AUTOINCREMENT rowid: strictly INCREASING, never
// reused even after a prune, and one namespace for every writer on the machine
// because they all write the same file. A reader resumes from the last seq it
// handled and never infers a lost event from a skipped number; lost events are
// reported against the retained floor instead.
//
// Dedupe keys are MARKS in a table of their own, not a column on the event:
// an event ages out after Retention, but "this escalation was announced" must
// hold for as long as the escalation stays pending — however long that is —
// or pruning its event would announce it again. A mark is removed only by
// ForgetMarks, which the daemon calls with the set still worth remembering.
//
// Appending is best-effort BY CONTRACT (see ports.StreamLog): the change an
// event describes has already been committed, so a failed append is logged by
// the caller and never fails the operation it describes.
package streamlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"

	_ "modernc.org/sqlite" // the store's driver; linked into every hap binary already
)

// FileName is the log's file name inside the state directory.
const FileName = "orchestrator-events.db"

// Retention is how long an event stays replayable through `--resume`.
const Retention = 7 * 24 * time.Hour

const schema = `
CREATE TABLE IF NOT EXISTS events (
	seq    INTEGER PRIMARY KEY AUTOINCREMENT,
	at     INTEGER NOT NULL,
	kind   TEXT    NOT NULL,
	author TEXT    NOT NULL DEFAULT '',
	fields TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_at ON events(at);
CREATE TABLE IF NOT EXISTS marks (
	key TEXT PRIMARY KEY,
	at  INTEGER NOT NULL
);
`

// errClosed is returned by every call on a closed Log.
var errClosed = errors.New("stream log is closed")

// Log is a lazily opened handle on the event log. Construction touches
// nothing: every hap command builds one, and only the few that emit or read
// events should pay for opening a database.
type Log struct {
	path string

	mu     sync.Mutex
	db     *sql.DB
	closed bool
}

// New returns a handle on the log at path, opened on first use.
func New(path string) *Log { return &Log{path: path} }

// InStateDir returns a handle on the log in stateDir.
func InStateDir(stateDir string) *Log { return New(filepath.Join(stateDir, FileName)) }

// Path is the log's file path.
func (l *Log) Path() string { return l.path }

// open opens and migrates the database once. A failure is NOT cached: a state
// directory that was briefly unwritable must not disable the stream for the
// rest of a long-lived process such as the daemon. A closed Log stays closed —
// a late append during shutdown must not quietly reopen the file.
func (l *Log) open() (*sql.DB, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errClosed
	}
	if l.db != nil {
		return l.db, nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return nil, fmt.Errorf("stream log dir: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(l.path))
	if err != nil {
		return nil, fmt.Errorf("open stream log: %w", err)
	}
	db.SetMaxOpenConns(4)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate stream log: %w", err)
	}
	l.db = db
	return db, nil
}

// migrate creates the schema, retrying a moment on SQLITE_BUSY. Two processes
// opening a FRESH file at once race to switch it to WAL, and SQLite answers
// the loser BUSY at once — without its busy handler — because waiting could
// deadlock; the winner is done within milliseconds.
func migrate(db *sql.DB) error {
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		if _, err = db.Exec(schema); err == nil || !busy(err) {
			return err
		}
		time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
	}
	return err
}

func busy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// dsn mirrors the store's sqlite settings where they matter to a
// multi-process writer: WAL, a busy timeout, and the write lock taken at
// BEGIN (see store.sqliteDSN for the SQLITE_BUSY_SNAPSHOT reasoning).
//
// Spelled out rather than built with net/url: internal/privacy bans that
// import outside the egress allowlist, and these values need no escaping.
func dsn(path string) string {
	p := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + p + "?_txlock=immediate" +
		"&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

// Append records ev and returns the sequence number it was given, or 0 when
// ev.Dedupe names a mark already set. The mark and the event are written in
// one transaction, so neither exists without the other.
func (l *Log) Append(ctx context.Context, ev domain.StreamEvent) (int64, error) {
	db, err := l.open()
	if err != nil {
		return 0, err
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("append stream event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if ev.Dedupe != "" {
		res, err := tx.ExecContext(ctx, `INSERT INTO marks (key, at) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`,
			ev.Dedupe, at.UnixMilli())
		if err != nil {
			return 0, fmt.Errorf("append stream event: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events (at, kind, author, fields) VALUES (?, ?, ?, ?)`,
		at.UnixMilli(), ev.Kind, ev.Author, domain.RenderStreamFields(ev.Fields))
	if err != nil {
		return 0, fmt.Errorf("append stream event: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("append stream event: %w", err)
	}
	return seq, nil
}

// Seen reports whether the dedupe mark is set. A read, so a caller
// re-examining state on a timer can skip the write lock an append would take.
func (l *Log) Seen(ctx context.Context, dedupe string) (bool, error) {
	db, err := l.open()
	if err != nil {
		return false, err
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM marks WHERE key = ?`, dedupe).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ForgetMarks removes every dedupe mark starting with prefix that keep does
// not claim, and reports how many. The caller must have seen the WHOLE set
// worth keeping: a mark forgotten for something still live lets it be
// recorded a second time.
func (l *Log) ForgetMarks(ctx context.Context, prefix string, keep func(key string) bool) (int64, error) {
	db, err := l.open()
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT key FROM marks WHERE substr(key, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return 0, err
	}
	var drop []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !keep(key) {
			drop = append(drop, key)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, key := range drop {
		if _, err := tx.ExecContext(ctx, `DELETE FROM marks WHERE key = ?`, key); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(drop)), nil
}

// Head is the highest sequence number ever assigned (0 for a fresh log). It
// reads SQLite's AUTOINCREMENT high-water mark rather than MAX(seq), so a log
// pruned empty still reports where it got to — which is what a resuming
// reader compares against.
func (l *Log) Head(ctx context.Context) (int64, error) {
	db, err := l.open()
	if err != nil {
		return 0, err
	}
	var head sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return head.Int64, nil
}

// Floor is the lowest sequence number still in the log, or 0 when it holds
// none.
func (l *Log) Floor(ctx context.Context) (int64, error) {
	db, err := l.open()
	if err != nil {
		return 0, err
	}
	var floor sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(seq) FROM events`).Scan(&floor); err != nil {
		return 0, err
	}
	return floor.Int64, nil
}

// Since returns up to limit events with a sequence number above after, oldest
// first.
func (l *Log) Since(ctx context.Context, after int64, limit int) ([]domain.StreamEvent, error) {
	db, err := l.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT seq, at, kind, author, fields FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.StreamEvent
	for rows.Next() {
		var ev domain.StreamEvent
		var at int64
		if err := rows.Scan(&ev.Seq, &at, &ev.Kind, &ev.Author, &ev.Rendered); err != nil {
			return nil, err
		}
		ev.At = time.UnixMilli(at)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Prune deletes every event recorded before cutoff and reports how many.
// Dedupe marks are untouched (see ForgetMarks).
func (l *Log) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	db, err := l.open()
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, `DELETE FROM events WHERE at < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Close releases the database, if it was ever opened, and refuses every later
// call.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.db == nil {
		return nil
	}
	err := l.db.Close()
	l.db = nil
	return err
}
