# Report pager

Comparison report producers use `operations::pager::Pager`. Construct it once
with `Pager::new(context, task)`, write report bytes in production order, and
always await `finish`, including after a write or producer error. A producer
error stays primary; append any finish error with `with_secondary`.

`write(&mut self, &[u8])` returns `Written` only after the pager pipe accepts the
bytes, or the stdout event consumer acknowledges them when paging is disabled.
This acknowledges delivery to the pipe, not display to the reader. `Closed`
means an actual pager pipe write returned EPIPE. Stop later report generation,
host reads and Git work, then finish the same pager. Channel loss, leader exit,
other input errors and cancellation do not mean `Closed`.

Paging requires both stdout and stderr to be terminals, `CONFCTL_TTY` to differ
from exactly `0`, and a nonblank `PAGER`. Input TTY state, `TERM`, color and live
rendering do not affect eligibility. Selection is frozen at construction. A
nonblank `GIT_PAGER` takes priority over `PAGER`; otherwise `PAGER` is used. Blank
means empty after Ruby's ASCII whitespace and NUL stripping rules.

The configured string runs once through `sh -c` when the first nonempty report
bytes are written. Only this operator-provided string is shell code; report
content goes to stdin. An empty report starts no child. There is no executable
search, Git pager configuration query, startup probe, argument stripping or
fallback pager. When paging is disabled, all report bytes go through the existing
acknowledged stdout event path, including redirected output.

The pager uses the existing supervisor and terminal owner. Its terminal lease
duplicates original stdout and uses that descriptor for validation, attachment,
stop/resume and restoration. Prompt input retains its original descriptor and
single broker. A terminal outside the application's controlling session or
without the existing suspend owner fails before paging.

`finish(self)` closes input and joins the single supervised request. It returns
`None` for unpaged or empty reports, or the real `ProcessOutcome` for a pager.
An ordinary nonzero exit produces a failed pager task and a diagnostic while
allowing the command's normal public exit 0 policy. Spawn, infrastructure,
cancellation, output and terminal-restoration errors fail. Raw captures and
native status remain available in the outcome or error; inherited terminal
outputs are explicitly unavailable captures. Callers must not infer successful
display from a partial write or ordinary exit.

This support does not change normal Bytes/File/SSH stdin handling, configuration
formats, local generation records or the default Ruby entry point. A later read
can use a corrected pager configuration; no state migration or repair is needed.
