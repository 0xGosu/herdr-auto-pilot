- Changed confirming an escalation with `--send`: the reply is now typed by the
  daemon instead of by whichever `hap` process you typed into. Two processes no
  longer drive the same agent pane, and an operator's answer finally goes
  through the never-auto screen and the per-agent lifecycle barrier that the
  daemon's own sends have always had. You still learn on the spot whether it
  landed.
- Changed `hap confirm --send` / `hap resolve --send` to require a running
  daemon, and to say so with the `hap daemon --ensure` remedy rather than
  recording a correction for a reply nothing will deliver. Confirming without
  `--send`, dismissing, and every config command still work with no daemon.
- Fixed a confirmed reply that a never-auto or suspected-irreversible rule
  refuses clearing its escalation anyway. Nothing was typed, the agent stayed
  blocked, and the row left the queue — so there was nothing left to look at.
  The refusal now leaves the escalation where a human will see it.
- Fixed hap opening the wrong SQLite database when its state directory path
  contained `?` or `#`. The path was pasted into a URI DSN unescaped, so it was
  truncated at that character — silently, with no error, and with every caller
  under such a path sharing one file somewhere else entirely.
