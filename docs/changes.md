# Compare deployed inputs

`confctl diff [options] [machine-pattern [input-pattern]]` compares deployed
inputs with the current configuration. `confctl changelog` uses the same
selection and shows Git history for changed inputs. Both select managed hosts,
including carried machines, with the usual attribute and tag filters. An explicit
null target stays selected and can fail target validation. Confirm the machine
table before comparison, or use `--yes` to skip the initial preview and prompt.

Current target inputs are evaluated for every selected machine before host
queries begin. There is no system build. `--generation SELECTOR` instead reads
saved inputs, using the normal saved-generation selection and missing-host
confirmation. Here `none` is a literal saved-generation name. An empty current
selection can finish after its preview; a saved selection with no matching
generations fails. Build job and core options do not affect these comparisons.

Hosts are visited in order. Each query reads uptime and deployed input metadata,
without reading current-system paths or enumerating profiles. Carried hosts use
their carrier profile metadata, including the existing profile-file fallback.
Offline hosts are reported and skipped. Input roles are sorted and filtered by
the optional input pattern. Full revision values determine equality; identical
short revisions can still differ. Unknown input state remains distinct from a
changed revision. `--downgrade` reverses the displayed endpoints and Git range.

Diff performs no Git or mirror-cache operation. Changelog uses the target's URL,
with the deployed URL as fallback, and preserves that URL without rewriting it.
Each changed role separately clones a missing mirror or fetches an existing one,
then reads its log. Repeated URLs still cause separate fetches in role order.
The default log is oneline with at most 50 commits. `--verbose` removes both
format and count options; `--patch` adds patches and removes the count limit.

Mirrors live in `$XDG_CACHE_HOME/confctl/git-mirrors/SHA256(url)`, or the current
user's `~/.cache/confctl/git-mirrors/SHA256(url)` when XDG_CACHE_HOME is unset.
An explicitly empty XDG_CACHE_HOME retains Ruby's literal `/confctl/git-mirrors`
location; an empty HOME similarly selects `/.cache` when XDG_CACHE_HOME is unset.
A cache leaf must be a real directory. Confctl does not repair, reset or delete
mirrors. Partial clone or fetch effects remain after failure or an early quit;
inspect the cache and connectivity before rerunning.

Reports remain visible in redirected stdout. When [paging](pager.md) is enabled,
the pager starts on the first report bytes. Quitting stops later host and Git
work, then joins the same pager. Completed observations and cache effects remain.
Offline hosts, unknown inputs, ordinary native Git failures and ordinary pager
exits permit invocation exit 0; failed roles retain diagnostics and native status.
Spawn, capture, output and cancellation errors fail the invocation. Cancellation
joins owned children. Confctl leaves configuration locks, profiles and saved
generations unchanged. The Rust development entrypoint is `confctl-rust`.
