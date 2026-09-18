package frontend

import "sync"

// Concurrently runs every fn at once and returns when all have.
//
// It exists for the reads a surface makes to paint one screen. Each is an
// independent store query, and under a shared engine each is a round trip —
// to the daemon over its socket and, under libsql, on to a remote server at
// ~200 ms apiece. Issued one after another, GetStatus alone paid ~20 of them
// (~5 s) before the TUI could draw its first row; issued together they cost
// about the slowest one. Under the local sqlite engine the same fan-out is
// merely harmless.
//
// Every fn writes only to variables of its own; the caller assembles the
// results after this returns, which is what keeps the fan-out free of locks.
func Concurrently(fns ...func()) {
	var wg sync.WaitGroup
	wg.Add(len(fns))
	for _, fn := range fns {
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	wg.Wait()
}
