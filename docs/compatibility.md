# CLI and state compatibility during the Rust replacement

The Ruby compatibility baseline is
`cc40679d267165aecfa569128438bb55fe910268`. Its executable and library are
preserved in `tests/compat/core/baseline.tar.gz`, bound by a SHA256 and tree ID in
`baseline.json`. The [oracle runner](../tests/compat/core/README.md) executes
that source independently of the candidate. `coverage.json` is the authoritative
machine-readable route/option inventory; it contains all 29 action leaves,
argument signatures, aliases and defaults.

Keep command meaning, results, input requirements and side effects compatible.
A requested invocation timing appendix and the deliberate removal of Ruby
extension support are explicit changes. Exact parser quirks or baseline defects
must be decided from captured evidence before changing behavior; they must not
silently become the new parser's defaults.

The `confctl-rust` development binary provides the Rust parser and application
entrypoint. The default `confctl` remains Ruby during development. Typed requests
cover the 29 original command actions and the built-in `runtime-kernels update`
action. The concrete routes are `ls`, `inputs ls` and `inputs channel ls`;
other operation adapters remain incomplete and return an explicit unavailable
error. Help and version need no inventory, input broker or process supervisor.
Static original help text is preserved in `src/cli/help.json`, with source
provenance in that module; it does not load test snapshots or Ruby at runtime.
The immutable M0 archive and expectations remain the differential oracle.

`cargo test --locked cli::tests` checks the parser against the frozen help,
option and usage cases without executing operations. `cargo test --locked
domain::selection::tests` checks ordered name/key/filter selection and the Ruby
pattern adapter. Command side effects require separate differential checks.
The declared development shell provides the pinned Rust tools alongside the
Ruby tools and hooks while both implementations are under development.

## Observable CLI contract

| Surface | Contract and high-value executable cases |
| --- | --- |
| Global/parser | Color always/never/auto, help/default help/completion, version, unique/ambiguous command prefix, unknown command, leaf help, option ordering, Integer validation, negative aliases, surplus arguments. Baseline framework is locked GLI 2.22.2. |
| Configuration | Init directory whitelist and generated files/modes; add nested paths; rename is a directory move; rediscover writes sorted cluster inventory. Hooks are a separate migration boundary. |
| Selection/list | Exact name before flake key before Ruby extglob; default managed set; unmanaged/all and runnable distinctions; repeated attr/tag AND, negation and dotted values. Table columns/header/empty rendering are observable. |
| Inputs | Root/channel/machine mapping, shared-input rejection, resolved revision output, all requires lock, explicit update names, Git commit controls, preserved upstream references and cached-GitHub-metadata refusal. |
| Build/deploy | No implicit input update, existing generation reuse, Nix argv/cores/jobs/trace, confirmation before effects, generation selection, action differences, copy-only, already-current skip, partial effects and failure. |
| Health/status | Health result summary and failure status; status offline/unknown/outdated rows do not themselves cause failure. Generation `none` skips target build/toplevel comparison. |
| SSH/connection | Literal local argv versus remote shell quoting; input modes; sequential/parallel and aggregate output. Failed connection hosts and remote nonzero command results do not universally become confctl nonzero status. |
| Generations/GC | Local default, remote or both, distinct selectors, current resolution, rejected formats untouched, retention minima/current, optional remote GC and partial failure. Preserve explicit baseline errors pending decision. |
| Data generation | All/container/network routes execute the locked client against a confined mock API: descriptor discovery, protected challenge, fixed token lifetime, authenticated includes query, container/node metadata and IPv4/IPv6 files. Auth-error, redirect and negative HTTP/socket cases retain failure evidence. The client issues one list call; this is not proof of arbitrary server pagination or TLS behavior. |

Ordinary result tables, progress and prompts use stdout. Error diagnostics and
log locators use stderr. Capture streams separately. Help-related errors can
print help to stdout while reporting an error to stderr. GLI usage errors and
ordinary exceptions have different exits; exact output and version failure are
recorded, not inferred from a new CLI framework.

Available operations open an invocation log before constructing the command
or reading default settings. Logs use the configuration directory and
`.confctl/logs/YYYY-MM-DD--HH-MM-SS-confctl-<command-path>.log`, with spaces in
the command path replaced by hyphens. Help, parser errors and unavailable
development routes do not open a log. Successful listing operations remove their
logs; failures retain them and report the path on stderr.

`inputs channel ls` loads the lock snapshot before its two independent settings
reads and channel evaluation. Invocation defaults supply the captured jobs value;
the client's settings control evaluation impurity. Ordinary names/globs sort;
explicit brace/comma selectors retain listed order and duplicates. Role mappings
retain object/array iteration order and Ruby's one-level array destructuring.
Only string inputs look up lock metadata; numeric inputs retain their display
value without looking up a numeric-looking string key. Rows are collected before
any table is emitted. This read-only route neither creates nor acquires the
mutation lock, including when another writer holds it. Surplus arguments are
operational Usage (status 64 with route help), after flake preflight and before
lock reading or Nix construction.

The focused channel oracle preserves 20 reviewed Ruby observations separately
from the original 372 cases. Integer mapping failures and native ENOENT lock
reads retain their original primary messages; ENOENT includes the attempted
absolute path's native bytes. The boxed lock context has redacted Debug/Display
and retains the typed source. **LOCK-JSON-01** intentionally changes only the
observed corrupt first-object-key diagnostic to
`invalid JSON in flake.lock at line {line} column {column}`, using structured
serde coordinates. Its full candidate expectation is separate from the unchanged
Ruby result. This one case establishes no general Ruby/serde parse acceptance,
coordinate, other syntax, scalar or I/O diagnostic equivalence. The comparator
checks complete streams/status, child order/argv/cwd and non-log file effects,
plus retained log names/modes; raw log bodies remain separate evidence.

Nix child failures with complete captures preserve the baseline command escaping,
native exit or signal value and separate stdout/stderr diagnostics. Error
`Debug` and `Display` keep command arguments and captured bytes redacted. After
process owners have joined, the app appends labeled fatal captures, including
incomplete prefixes, to the invocation log. It verifies the appended bytes,
syncs and closes the file, then checks the path's identity and contents before
releasing process captures. Any uncertainty keeps fallback evidence and reports
the log error after the original failure. A log filename alone does not prove
capture retention; partial captures do not establish a complete stream.
Presentation replay offsets and pending bytes retain their separate evidence path.

Confirmation displays `Continue? [y/N]: ` but only explicit `y`/`n` terminates
the loop; blank input loops, EOF errors. `--yes` bypasses the confirmation's
selection-description block too. Interactive per-host questions may override
yes. Redirected output never grants consent. Preserve explicit input ownership
when switching between live display, prompts and interactive SSH.

TTY admission requires both stdout and stderr to be terminals and
`CONFCTL_TTY` not equal to `0`. Color and paging have their own controls;
nonempty NO_COLOR affects auto color, PAGER must be nonblank for paging.
Static captured pipe output does not establish live redraw/resize/signal parity.

## State and external interfaces

Preserve `.confctl/generations/<escaped-host>/<generation>/generation.json`,
input/toplevel/auto_rollback links and `current`. Accepted records have explicit
`mode: "flakes"`, ISO8601 date, toplevel, optional auto_rollback, inputs and
inputs_info; reads also accept inputsInfo. Unsupported or malformed records and
their GC roots remain untouched. Numeric selection can refuse a mixed inventory;
invalid current must not become an implicit newest-generation fallback.

Rust state and generation writers require a borrowed guard for the same
canonical configuration root. The persistent private `.confctl/mutation.lock`
uses an exclusive advisory flock; acquisition is cancelable and has no expiry.
Writers keep the lock inode without truncating, unlinking or replacing it.
Read-only routes must not acquire this lock. The additional lock file is an
accepted compatibility difference for mutating routes. Ruby does not
participate, so concurrent Ruby and Rust mutation is unsupported.

An outer mutation must acquire once after prompts and retain that guard across
the source write, fresh Nix evaluation and generated-file publication. A later
failure preserves an earlier successful write; the sequence provides no
rollback transaction. Integration of these app-level workflows remains a
separate acceptance gate from the guarded writer and service tests.

GC roots incorporate real configuration path hash, login, host, generation and
suffix. Local fixtures use a pre-existing generation to test reuse without
writing host gcroots. New-generation publication/root failures and bidirectional
Ruby/Rust writes still need their owning state package tests. Moving the
configuration directory or collecting retained closures affects rollback and
is not authorized by test completion.

Login preserves the baseline `Etc.getlogin` contract: use libc login first,
then raw `USER` only if libc returns no login. If that fallback is needed, empty
USER remains empty and absent USER produces an error before GC-root effects.
Neither `LOGNAME` nor a username derived from the UID supplies a fallback.
Preserve Ruby's native-byte `File.join` behavior for leading and repeated
slashes, dot segments and non-UTF-8 login bytes. A USER value beginning with `/`
does not discard the per-user prefix.

The Rust adapter runs pinned GNU `logname` with a duplicate of the original
stdin descriptor, without reading it, and captures both output streams. Only
the qualified no-login result permits USER fallback. Spawn, signal, capture
and malformed-output failures remain infrastructure errors. Keep the lexical
`bin/logname` entry: resolving its multicall symlink to `bin/coreutils` changes
dispatch. This read-only child is an accepted login-lookup intervention; other
children do not gain permission to inherit stdin through this path.

Flake-lock input arrays preserve the baseline first-segment interpretation:
`["node", "nested"]` reads `nodes["node"]` and ignores the suffix. Setter
restoration uses the temporary lock's first-segment mapping too. Recursive
follows resolution would change this behavior. Eight additional immutable
observations cover varied suffixes, missing first nodes, original metadata
restoration, distinct nested leaves and exact result/error streams.

Nix output names, channels/overrides, machine keys, evaluation impurity,
no-lock-write/update flags and recognized retry policy are interfaces. The fake
Nix child records exact argv and serves only declared responses; it does not
prove real Nix evaluation or build compatibility. Remote system/carrier profiles,
inputs/configuration metadata, rollback handshake and netboot index/boot files
must remain readable across mixed versions. Separate helper fixtures and later
disposable VM tests own those contracts.

## Baseline discrepancies and acceptance gates

The baseline App does not register manual-only `deploy --outdated`. Channel set
registers `allow_shared` while docs/errors spell it with a hyphen; both spellings
are executable cases. GC's progress consumer accesses `.line` while its progress
object lacks that reader. **GC-01** is an approved narrow correctness exception:
successful GC child plus progress produces a summary and exit 0; genuine child
nonzero records failed hosts, joins other scheduled jobs and produces confctl
exit 1. Unknown progress remains observational. Renderer/log/spawn/drain errors
are infrastructure errors and must not mask an earlier child failure. Preserve
selection, argv, effects and caller ordering. The raw Ruby traceback remains a
separate defect observation, not a byte-exact candidate expectation. Candidate
success, failed-child, mixed-host and renderer-fault tests remain required.
The two named observations cover successful-child progress and failed-child
empty-stream flush; neither volatile traceback is an ordinary expected snapshot.

The core fixture layer uses fake child services, an owned loopback HTTP API and
temporary state only. A test-only transport preload checks the original hardcoded
API URL, redirects its Communicator to loopback and rejects all other HTTP/socket
origins including redirects. Auth/client/GenData logic stays unchanged; events
omit synthetic credential values. It does not contact production, execute actual
SSH, activate systems, collect host garbage or build closures. It must preserve
separate raw streams, native status, child order/argv/input and final file/link
effects. Temporary paths/logger times
and bounded file nonces may be normalized; machine values, fixed generation
dates, exits, diagnostics and result ordering may not be broadly normalized.

Before replacing Ruby, require independently checked candidate comparison,
focused domain/state/process tests, real PTY cases, candidate API authentication
and data-generation equivalence, package closure/platform checks and disposable
mixed-version VM acceptance. A complete route manifest or successful `--help` run does not
satisfy operational acceptance. The intended timing footer requires a strict
validated terminal suffix splitter and direct timing assertions, while retaining
the rest of stderr. Help/list/input/comparison/data routes must not accidentally
acquire a timing report.
