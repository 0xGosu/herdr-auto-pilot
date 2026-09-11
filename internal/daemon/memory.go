package daemon

import (
	"runtime/debug"
	"runtime/metrics"
	"sync"
)

// rebuildGCPercent is the GC target while the knowledge index is rebuilt.
//
// A rebuild is the daemon's one large allocation burst: loading every stored
// signature and building a fresh bleve/FAISS index allocates ~40MB for a
// 787-rule store in well under a second, against a live heap of a few MB. At
// the default target the heap is allowed to grow to twice the live data in
// between collections, which put the daemon's resident set at ~125MB for its
// first minute and pinned its high-water mark there. Collecting more often for
// that half-second costs a few milliseconds of CPU; doing it for the whole
// process (GOGC=25) was measured at roughly double the idle daemon's CPU.
const rebuildGCPercent = 25

// rebuildGC is the process-wide GC setting shared by overlapping rebuilds (a
// reload and a fleet pull can both be building); guarded by its mutex.
var rebuildGC struct {
	sync.Mutex
	active    int
	prev      int
	tightened bool // the first burst lowered the target; restore it at the last
	enabled   bool // GC was on, so handing memory back is meaningful
}

// tightGC lowers the GC target for an allocation burst and returns the
// function that ends it. The last burst to end restores the previous target
// and returns the freed heap to the OS at once — the runtime would otherwise
// hand it back over the next minute or so. It only ever LOWERS: an operator's
// tighter GOGC stays as it is, and GOGC=off is not touched at all (not even by
// the handback, which would force a collection). Overlapping calls nest; the
// returned function is safe to call twice.
func tightGC() (release func()) {
	rebuildGC.Lock()
	if rebuildGC.active == 0 {
		cur := gcPercent()
		rebuildGC.prev, rebuildGC.enabled = cur, cur >= 0
		rebuildGC.tightened = cur > rebuildGCPercent
		if rebuildGC.tightened {
			debug.SetGCPercent(rebuildGCPercent)
		}
	}
	rebuildGC.active++
	rebuildGC.Unlock()
	return sync.OnceFunc(func() {
		rebuildGC.Lock()
		rebuildGC.active--
		last := rebuildGC.active == 0
		if last && rebuildGC.tightened {
			debug.SetGCPercent(rebuildGC.prev)
		}
		handBack := last && rebuildGC.enabled
		rebuildGC.Unlock()
		if handBack {
			debug.FreeOSMemory()
		}
	})
}

// gcPercent reads the current GC target without changing it (-1 = off).
func gcPercent() int {
	s := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 100
	}
	v := int64(s[0].Value.Uint64())
	if v < 0 || v > 1<<20 {
		return -1
	}
	return int(v)
}
