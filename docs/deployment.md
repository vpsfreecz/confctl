# Deployment

Deployment selects managed machines, validates the action and asks for initial
confirmation unless `--yes` is set. It runs `deploy.prepare` once before building
or resolving stored generations, then reloads the selected names from the fresh
inventory. A failed prepare hook prevents both paths.

The default action is `switch`. `--copy-only` transfers generation paths without
activation, reboot or health checks. Bulk mode finishes copies before activating;
`--one-by-one` finishes each target before starting the next. Copy workers must
be positive; large requests use no more workers than queued targets.

The client uses each generation's existing rollback helper and its connectivity
confirmation protocol. Health success does not confirm the rollback helper.
Carried machines update their carrier profile on `switch`; other actions leave
that carried profile alone. Successful standalone `boot` or `switch` activation
updates the system profile afterward. `test` and `dry-activate` do not.

`--reboot` requires `boot`. Waited reboot succeeds when the observed uptime falls
below its initial value. `wait` has no deadline; `nowait` sends the request without
polling. Canceling the local SSH child does not establish remote completion.

Deployment uses the existing health runner when it reaches nonempty checks. It
passes `--max-jobs` text to Nix unchanged. Health checks use five workers when the
option is absent; an explicit value uses the existing integer grammar. Values
such as `auto`, or nonpositive integers, fail there. They do not block paths
with health checks disabled, no checks or a declined health-check prompt. Interactive retries can repeat all checks
or only failed checks. Any failed attempt keeps the log, including when a later
retry succeeds or `--keep-going` permits completion.

Deployment is not a transaction. A later copy, activation, profile, reboot or
health failure leaves earlier effects in place. Inspect the retained log and
actual target/profile state before retrying. An interrupted operation can leave
remote state unknown; switching tool versions cannot undo those effects.
