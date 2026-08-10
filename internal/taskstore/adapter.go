package taskstore

// Adding a task-list backend
// ==========================
//
// Task storage is an adapter (ports-and-adapters) boundary. Everything hap does
// to a checklist goes through ONE interface — ports.TaskStore — and the config
// decides which adapter is behind it. Nothing outside this package names a
// concrete backend; TestOnlyTheRegistryImportsAConcreteBackend enforces that,
// so a new provider cannot leak into the daemon or the front-ends by accident.
//
// The interface is deliberately two methods:
//
//	Read(ctx, locator) ([]byte, error)
//	Mutate(ctx, locator, wait, fn) ([]domain.ChecklistItem, error)
//
// with two OPTIONAL capabilities a caller type-asserts and degrades without
// (the same convention the herdr port uses): EnsureCreator for create-on-demand,
// and RemoteTaskStore for "my calls leave the machine".
//
// The reason Mutate takes the transform rather than exposing Get/Put is
// correctness, not taste: every mutator hap has checks and claims in the same
// pass, so a Get/Put split would move the check outside the critical section
// and reintroduce the double-delivery race. Keeping fn inside also means a new
// adapter inherits the ENTIRE existing mutator layer — reserve, release,
// reclaim, the pre-delivery review, the kill-switch re-read — without writing
// any of it.
//
// To add one (say Linear), in order:
//
//  1. config: add the provider name to ValidTaskSourceProviders, and a settings
//     SUBTABLE under [task_source_provider] (never new top-level keys, so
//     nothing already on disk moves). Register its scalar keys in
//     frontend.ConfigFields — TestEveryConfigKeyIsRegistered will name them if
//     you forget. Per-source overrides go on TaskSource as flat `omitempty`
//     fields and must NOT be materialized by normalizeTaskSources.
//
//  2. tasklocator: add a scheme (e.g. "linear://") plus Parse/Build helpers,
//     teach Resolve to mint it, and teach Display to render it as an address a
//     human or an agent can act on. Canonical must return it verbatim —
//     filepath.Abs silently mangles a scheme'd string into a cwd-relative path,
//     and every hap process has a different cwd.
//
//  3. internal/taskstore/<name>: implement ports.TaskStore. If calls leave the
//     machine, implement RemoteTaskStore too and take taskfile.LockWithin
//     around the whole read-modify-write, keyed on the canonical locator —
//     that lock is hap's only cross-process serialization, and it is what a
//     backend without compare-and-swap depends on.
//
//  4. registry.go: one case in backend() and one in ForLocator(). ForLocator
//     must dispatch on the locator's SCHEME, not on configured provider, so
//     ledger rows written before a provider change still reach their backend.
//
//  5. internal/privacy: if it egresses, add its import path to
//     forbiddenImports and allowlist exactly the one file that talks to the
//     network, then update TestHTTPAllowlistStaysMinimal. The walker checks
//     DIRECT imports, so an SDK-only adapter would otherwise egress silently.
//
// Nothing in internal/daemon, internal/frontend, internal/cli or internal/tui
// should need to change.
