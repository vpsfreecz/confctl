# Helper diagnostic compatibility

The Rust helper backend has two bounded diagnostic exceptions to the retained
Ruby helper behavior. They preserve the helper's exit status, preceding stdout,
child and HTTP order, and filesystem effects. They change only the stderr rows
listed below. They do not authorize removing traceback lines from comparisons
or accepting arbitrary library error text.

The original source revision is
`cc40679d267165aecfa569128438bb55fe910268`. Its four unmodified helper sources
remain in [the helper baseline](../tests/compat/helpers/baseline/), with source
hashes in [sources.json](../tests/compat/helpers/sources.json).
[The candidate expectations](../tests/compat/helpers/candidate/diagnostics-v1.json)
identify each original capture by SHA256 and retain its complete expected
stdout, native status, signal and effects. `@ROOT@` binds only that fixture's
temporary root; it is not a general path normalization rule.

## HELPER-DIAG-01

These five observed failures exit with status 1 and no terminating signal. Each
stderr message ends in exactly one LF byte.

| Original fixture | Exact candidate stderr before LF |
| --- | --- |
| `carrier-nix-failure` | `nix-env -p <profile> --set /nix/store/fixture failed` |
| `carrier-callback-failure` | `<on-change executable> failed` |
| `netboot-invalid-0` | `Invalid machine metadata JSON: <attempted metadata path>` |
| `netboot-invalid-1` | `No such file or directory: <attempted current-profile path>` |
| `netboot-publish-failure` | `Failed to install new root to <TFTP root>` |

The carrier command messages retain the original space-joined command primary.
The displayed string is never reparsed to run a command. A failed profile
command prevents the callback. A failed callback follows the completed profile
command and does not roll back its effects. Child stdout and stderr pass through
unchanged; the retained failure fixtures supply empty child streams.

The metadata diagnostic uses the lexical profile-link path used to read
`machine.json`, such as `profiles/confctl-node-1-link/machine.json`. It does not
substitute the canonical store path or claim that the path proves a race-free
file identity. The current-profile diagnostic applies only to ENOENT from
resolving the attempted `profiles/confctl-<name>` path. Other I/O errors retain
their own sources and do not become ENOENT.

Both metadata/current failures occur after taking the builder lock and before
the machine listing or staging. They leave the old HTTP/TFTP links and sentinel
files unchanged. The observed first-publication failure follows the full
listing and exactly one `ln -sfn` invocation for the TFTP root. It leaves both
staging trees and both old published roots in place. Its expectation compares
all 52 captured file entries, including sentinel paths reached through the
published links. It neither retries publication nor cleans failed staging trees.
The separately supported second-publication boundary can leave TFTP published
while HTTP still points at its old root; the pair is not a transaction.

Private concrete errors retain the native child status, parse error or I/O
source and its relevant path. Their debug representations omit paths, command
arguments and metadata contents. Only the approved renderer emits the contextual
command or path; it does not append parser prose or captured child bytes.

## KEXEC-03

The observed bad-object index body `{bad` has this exact stderr:

```text
ERROR: Could not parse JSON: invalid JSON at line 1 column 2
```

It retains empty stdout, helper status 1, no terminating signal and exactly one
index HTTP request. It creates no image download, child/kexec, file or temporary
file effect. The error retains the structured parser source privately. Rendering
uses its raw `line()` and `column()` values without clamping or reproducing
Ruby's parser implementation.

Only this bad-object expectation is qualified. Empty, whitespace-only, EOF,
truncated, multiline and non-ASCII input require their own original/candidate
table before broader acceptance. The input-only list in
[json-syntax-cases.json](../tests/compat/helpers/candidate/json-syntax-cases.json)
records those pending cases without asserting output. Transport, framing and
codec errors occur before JSON parsing and remain separate failures.

Other helper error branches do not inherit these exceptions. CARRIER-01,
KEXEC-01 and KEXEC-02 retain their independently bounded scopes. Native rollback
survival, locking, interrupted publication, TLS/codecs and wrapper/package
compatibility have separate verification requirements.
