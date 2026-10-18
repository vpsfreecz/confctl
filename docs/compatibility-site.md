# Site and remote-helper compatibility oracle

The two site scripts and four installed helpers have executable Ruby oracles
under `tests/compat/site/` and `tests/compat/helpers/`. They complement the
[main CLI contract](compatibility.md). These are test dependencies; no Ruby
fallback is introduced into the future Rust runtime.

`sources.json` in each directory records repository revision, original path and
SHA256. The `.rb.txt` files are exact source bytes, including template tokens.
Do not update a snapshot to make a candidate pass. The site snapshot is from
`a4847bd6bbf2a62c107cff30b47452a60548a33e`; both script files agree with the
earlier inventoried site revision. The helper snapshot is from confctl
`cc40679d267165aecfa569128438bb55fe910268`.

Run from confctl's declared development environment:

```sh
nix develop
python3 tests/compat/core/run.py --export-baseline /tmp/confctl-ruby-oracle
export CONFCTL_RUBY_ORACLE_ROOT=/tmp/confctl-ruby-oracle
bundle exec rspec tests/compat/site/oracle_spec.rb tests/compat/helpers/oracle_spec.rb
```

The export destination must be empty and owned by this verification run. Use a
fresh directory for each independent export. Before the Ruby source is retired,
the site runner also accepts the current source root as its default; for
differential verification always select the immutable export explicitly. Its
Gemfile.lock dependencies must remain available in the declared test environment.
This is separate from the installed CLI/helper runtime closure.

Set `CONFCTL_COMPAT_CAPTURE_DIR` to an owned evidence directory to retain every
scenario's independent raw stdout/stderr, exit/signal, command events, file
contents/symlinks and Ruby/gem-version provenance. Raw output is never replaced
by table parsing or a normalized final frame. Temporary test paths are retained
in evidence for interpretation, then the disposable files are removed.

The locked Bundler 2.5.7 redefines eight `Gem::Platform` constants that Ruby
3.4.9 already defines. An empty `ruby -e ''` in the same environment establishes
this startup diagnostic before an oracle source runs. The harness accepts only
that complete 16-line block, or empty startup stderr; unknown startup messages
fail. `stderr` retains the complete raw bytes, `startup_stderr` records the empty
process result, and `application_stderr` removes that exact prefix only when
present. No application error, exception, log locator or arbitrary warning is
discarded. The bootstrap warning remains visible in aggregate test logs.

## Boundaries and covered behavior

The site driver loads the real confctl Ruby CLI and the unmodified two scripts
from their snapshots. It replaces Nix inventory/settings and the external
command runner with deterministic fakes. The real parser, filters, confirmation,
machine metadata, hook dispatch, kernel merge/normalization, discovery and
formatters execute. Downstream deployment/build stages record their inputs and
kernel-file contents instead of executing. Unexpected commands fail closed;
machine targets are fake `.invalid` names. No real SSH, Nix daemon, Git operation
or production configuration is used.

The helper driver performs only the normal Nix token substitutions in the
snapshots, writes the substituted script to the disposable directory and loads
it. `/run/current-system`, `/proc/cmdline`, carrier profiles and lock paths are
mapped to controlled inputs. Process/HTTP boundaries record calls and return
fixtures. Synthetic local child exit codes set Ruby's real `$?`; they execute no
commands. A fake `ln -sfn` publishes real local symlinks. HTTP responses are
in-memory `Net::HTTP` response objects; no socket is opened. Filesystem writes
are confined to the disposable root.

| Surface | Assertions |
| --- | --- |
| Runtime kernel command | CLI/help, argument failure, exact-name/key/brace selection, repeated attr/tag filters, managed/carried distinction, yes/no/blank/EOF, ordering, stdout normalization, permissive state and nonfatal host failures |
| Deployment refresh | Confirmed managed scope narrowed to non-carried vpsAdminOS; four actions; generation lookup/build ordering; copy-only/one-by-one and later already-current handling; empty subset and standalone build exclusion |
| Netboot inventory | Rediscover/add/rename stage, fresh core list, spin/managed/carried selection, exact generated data, empty list and partial failure |
| Auto rollback | CLI/help/errors; `switching` → `switched` → external `confirmed`, removal on confirmation, timeout rollback to captured old system with same action |
| Carrier environment | Required profile/action, set/delete argv, callback order, first-command and callback failure, surplus-argument defect |
| Netboot builder | Profile generations/current, both spins, HTTP/TFTP trees, ordered generations, variants, `inputs_info`, kernel/init/httproot params, MAC mapping, ISO/memtest menus, lock use, publication and old-root preservation on failure |
| Kexec reader | Exec/unload/no-sync, index derivation/server override, current/numeric/negative-offset selection, requested variant, interactive defaults/decline, HTTP/JSON/download/load and cleanup/error behavior |

The oracle exposes legacy behavior rather than approving accidental defects.
In particular, carrier-env surplus arguments reference an undefined `parser`;
missing requested kexec variants warn but still load without a variant; explicit
server URL still requires `httproot` in cmdline. Any departure needs a bounded
design decision with retained raw evidence.

## Remaining acceptance gates

Fake-service baseline success is not native helper acceptance. M3/M5 must prove
watchdog survival across real SSH transport loss and broken logging pipes,
process-tree cancellation, OS lock contention, interrupted publication, real
architecture packages and old/new helper interoperability. The automatic
rollback fixture does not establish remote detachment by faking `spawn`/`wait`.
The netboot fixture records lock opening and normal file effects; it does not
test competing processes. HTTP fixtures do not establish TLS/proxy behavior.

Rust feature verification must also cover disabled defaults, validated output
paths, legacy-script migration refusal, mutation locking and atomic writes,
cache invalidation and both file-format directions. The site oracle is the
preserved legacy behavior reference; it is not itself a Rust-candidate adapter.
Do not remove original baseline snapshots, weaken result assertions or declare
native mixed-version support based only on these Ruby tests.
