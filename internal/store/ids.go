package store

import (
	"sync"
	"time"
)

// IDAllocator hands out the INTEGER PRIMARY KEY of a new row. When the store
// has none, the database assigns rowids itself (AUTOINCREMENT, LastInsertId) —
// the sqlite engine's behaviour, unchanged.
type IDAllocator interface {
	Next() int64
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
	// The sequence range this allocator owns within a millisecond. The
	// daemon takes the lower half and a front end's FALLBACK allocator the
	// upper half, so the two never mint the same id on one node even when
	// the front end could not reach the daemon for its ids.
	seqBase, seqLimit int64
}

// seqPerMs is the sequence space per millisecond; seqHalf splits it between
// the daemon (lower) and a front end's fallback (upper).
const (
	seqPerMs = 1 << 10
	seqHalf  = seqPerMs / 2
)

// NewTimeOrderedIDs returns the DAEMON's allocator for the given 12-bit node
// value: sequence 0..511 in each millisecond. now defaults to time.Now.
func NewTimeOrderedIDs(node uint16, now func() time.Time) *TimeOrderedIDs {
	return newTimeOrderedIDs(node, now, 0, seqHalf)
}

// NewFallbackTimeOrderedIDs returns the allocator a FRONT END uses when the
// daemon cannot hand out ids: the same node bits, sequence 512..1023, so an id
// minted here can never equal one the daemon minted in the same millisecond.
// Two front ends both falling back in the same millisecond remain a
// collision, which is why the fallback is the exception and logged.
func NewFallbackTimeOrderedIDs(node uint16, now func() time.Time) *TimeOrderedIDs {
	return newTimeOrderedIDs(node, now, seqHalf, seqPerMs)
}

func newTimeOrderedIDs(node uint16, now func() time.Time, base, limit int64) *TimeOrderedIDs {
	if now == nil {
		now = time.Now
	}
	return &TimeOrderedIDs{node: node & 0xFFF, now: now, lastMs: -1, seqBase: base, seqLimit: limit}
}

// Next returns the next id.
func (g *TimeOrderedIDs) Next() int64 {
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
		if g.seq >= g.seqLimit {
			ms++
			g.seq = g.seqBase
		}
	} else {
		g.seq = g.seqBase
	}
	g.lastMs = ms
	return ms<<22 | int64(g.node)<<10 | g.seq
}

// nextID is what every INSERT into an INTEGER PRIMARY KEY table binds for its
// id column: an allocated id when the store has an allocator, otherwise NULL,
// which makes SQLite assign the rowid exactly as an omitted column would — so
// LastInsertId keeps working and there is one code path for both engines.
func (s *Store) nextID() any {
	if s.ids == nil {
		return nil
	}
	return s.ids.Next()
}
