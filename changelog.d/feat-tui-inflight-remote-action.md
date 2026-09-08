- Added immediate feedback on the Escalations tab when you answer a row. The
  row dims and its first column shows `»` the instant you press the key, and
  the banner names where the answer is going ("answering escalation #41 on node
  laptop — queued for its daemon…"). Answering another machine's escalation
  files the request for that node's daemon and waits up to 45s for its verdict,
  and for that whole window the row used to look untouched — so there was
  nothing to tell "it is on its way" from "the key did not register".
- Fixed a second press on a row already being answered queueing the work twice.
  It is now refused, saying the answer is already in flight. Covers every row
  action: confirm+send, confirm-only, correct, retry LLM and delete.
