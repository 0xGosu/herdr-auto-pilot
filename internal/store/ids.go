package store

import (
	"database/sql/driver"
	"fmt"
	"sync"
	"time"
)

// IDAllocator hands out the INTEGER PRIMARY KEY of a new row. When the store
// has none, the database assigns rowids itself (AUTOINCREMENT, LastInsertId) —
// the sqlite engine's behaviour, unchanged. An allocator that cannot answer
// (a front end whose daemon did not hand out an id) returns an error, and the
// insert that needed the id then FAILS with it — never a locally invented id
// that could collide with the daemon's or another front end's.
type IDAllocator interface {
	Next() (int64, error)
}

// idEpochMs is 2025-01-01T00:00:00Z. 41 bits of milliseconds above it run to
// the year 2094.
const idEpochMs = 1735689600000

// TimeOrderedIDs allocates 63-bit ids that sort by time across machines:
//
//	| 41 bits: ms since idEpochMs | 12 bits: node | 10 bits: sequence |
//
// Rows written on different machines can never collide (the node bits differ),
// and `ORDER BY id DESC` stays "newest first" fleet-wide to millisecond
// precision, which is what every "latest by id" query in the store assumes.
// It never sleeps: a burst past 1024 ids in one millisecond borrows the next
// millisecond, and a clock that steps backwards keeps counting from the last
// millisecond handed out.
type TimeOrderedIDs struct {
	mu     sync.Mutex
	node   uint16
	now    func() time.Time
	lastMs int64
	seq    int64
}

// seqPerMs is the sequence space per millisecond. There is exactly ONE
// allocator per node — the daemon's; front ends draw from it over the store
// socket (sqlbridge.RemoteIDs) and never mint locally — so the whole range is
// its own.
const seqPerMs = 1 << 10

// NewTimeOrderedIDs returns the node's allocator for the given 12-bit node
// value. now defaults to time.Now.
func NewTimeOrderedIDs(node uint16, now func() time.Time) *TimeOrderedIDs {
	if now == nil {
		now = time.Now
	}
	return &TimeOrderedIDs{node: node & 0xFFF, now: now, lastMs: -1}
}

// Next returns the next id. It never fails.
func (g *TimeOrderedIDs) Next() (int64, error) {
	ms := g.now().UnixMilli() - idEpochMs
	if ms < 0 {
		ms = 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if ms < g.lastMs {
		ms = g.lastMs
	}
	if ms == g.lastMs {
		g.seq++
		if g.seq >= seqPerMs {
			ms++
			g.seq = 0
		}
	} else {
		g.seq = 0
	}
	g.lastMs = ms
	return ms<<22 | int64(g.node)<<10 | g.seq, nil
}

// MustNext is Next for an allocator that cannot fail (TimeOrderedIDs), for
// callers that take a plain func() int64.
func (g *TimeOrderedIDs) MustNext() int64 {
	id, _ := g.Next()
	return id
}

// nextID is what every INSERT into an INTEGER PRIMARY KEY table binds for its
// id column: an allocated id when the store has an allocator, otherwise NULL,
// which makes SQLite assign the rowid exactly as an omitted column would — so
// LastInsertId keeps working and there is one code path for both engines.
func (s *Store) nextID() any {
	if s.ids == nil {
		return nil
	}
	id, err := s.ids.Next()
	if err != nil {
		// Bound as a driver.Valuer that fails: database/sql evaluates it when
		// the statement's arguments are converted, so the INSERT fails with
		// this error before anything reaches the database. Loud, and no id.
		return failedID{err: err}
	}
	return id
}

// failedID is the argument bound for an id the allocator could not provide.
// Its Value method fails, which fails the statement.
type failedID struct{ err error }

// Value implements driver.Valuer by refusing.
func (f failedID) Value() (driver.Value, error) {
	return nil, fmt.Errorf("store: no id for the new row: %w", f.err)
}
