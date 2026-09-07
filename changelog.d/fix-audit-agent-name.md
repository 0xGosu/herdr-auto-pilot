- Fixed the TUI Audit tab and `hap audit` labelling another machine's rows with a local
  agent's name: a herdr pane id repeats on every node sharing the store, so audit rows
  now resolve the same `name@node` identity the Escalations tab already used — and an
  audit row from another node no longer borrows a local agent's type
- Added an `agent=` column to `hap audit`, which never had one; it is appended beside
  `node=` so nothing parsing the existing tab-separated fields moves
- Added the node to an audit/escalation detail view when the row came from another machine
