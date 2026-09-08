- Changed the Escalations tab's "prune old" (`X`) and `hap escalations prune` to
  retire every machine's aged escalations, not just this one's. The list they
  prune has always spanned the fleet under a shared `turso` database, so a
  node-scoped prune cleared a fraction of what was on screen and reported the
  count as though it had cleared all of it. Both surfaces now name the scope
  ("across 3 nodes"), and `hap escalations prune --node <label|id>` prunes a
  single machine.
- Fixed `f` on another machine's task list saying "no live agent matches this
  task source". It now focuses that node's agent, the way `f` on the Agents and
  Escalations tabs already did; a list the owning node never named an agent for
  says that instead.
- Fixed `K`/`J` refusing to reorder another machine's task list with "this task
  source is no longer loaded" — about a list that was on screen. Reordering
  works on any node's list.
- Fixed `x` on another machine's task-source header doing nothing at all. It now
  explains that a task source lives in that machine's `config.toml`, which never
  enters the shared database, and points at `hap config task-source remove`
  there.
- Fixed the README's fleet section still saying renaming, enabling, disabling or
  focusing a remote agent is refused — `--node` has filed all four for the
  owning daemon since the fleet view landed. Changing a remote agent's
  permission mode is the one that really is refused.
- Fixed `--node=` with no value being read as "no `--node` at all" on every verb
  that takes the flag. It was a silent no-op there, and would have been the
  wider action on the new fleet prune; it is now refused like the bare `--node`.
- Fixed the Tasks tab treating a fleet task-list read FAILURE as another
  machine's list: focus, send and source-removal named an empty node and
  pointed at a machine that does not exist. They report the store error now.
