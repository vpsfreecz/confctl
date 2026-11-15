# Site and remote-helper compatibility evidence

Frozen observations under `tests/compat/{core,site,helpers}` preserve the original
source revisions, SHA256 values, inputs, separate streams, native outcomes and
filesystem effects. They are data, not an application or an executable Ruby
oracle. Ordinary Ruby replay, the core source archive and unused script copies
are retired after moving their assertions to the existing Rust tests. Original
sources remain recoverable from the recorded Git revisions and preserved history;
the JSON manifests and observations remain byte-for-byte unchanged.

Candidate acceptance preserves invocation, consent, targets, ordered effects,
native results, state, supported-reader meaning and useful information on its
correct stream. Human wording, padding and diagnostic coordinates may improve.
See the [consumer-specific contract](compatibility.md#candidate-acceptance-by-consumer).
Unknown or uninterpreted changes stay unqualified. Frozen data is never rewritten
from a candidate, and deleting an old recorder does not promote a capture to
native acceptance.

## Assertion ownership after replay retirement

The old site and helper assertions map to these maintained tests. Their frozen
inputs and outputs retain the historical reference independently of Ruby.

| Retired assertion group | Maintained owner |
| --- | --- |
| Retired API transport/preload, auth challenges, protected inputs and gen-data publication | `src/vpsadmin/tests.rs` and `src/app/gen_data_tests.rs` use confined Rust transports |
| CLI/help, option/argument selection, confirmation and failure before effects | `src/cli/tests.rs`, `src/domain/selection.rs`, `src/terminal` tests and actual command fixtures |
| Runtime kernel normalization, partial host failures, scalar/malformed state, unselected keys, local/SSH routing, reverse completion order and publication | `src/runtime_kernels/tests.rs` and `src/app/runtime_kernels_tests.rs` |
| Deploy refresh before build/generation selection, four actions, copy-only and empty scope | `src/app/build_tests.rs` and `src/operations/deploy/tests.rs` |
| Add/rename/rediscover netboot ordering, exact Nix data, empty list and retained earlier effects | `src/netboot_inventory/tests.rs` and `tests/configuration_command.rs` |
| Auto rollback argument errors, switching/switched/confirmed protocol, captured old system/action, timeout and pipe drain | `src/remote/auto_rollback.rs` and the retained watchdog fixture |
| Carrier required flags, set/delete argv, native command/callback order and failures, surplus-argument decision | `src/remote/carrier_env.rs` and `tests/rust_helpers_package.rs` |
| Netboot both-spin trees, menus, variants, current/older generations, parameter files, JSON, publication and old roots | `src/remote/netboot_server/tests.rs` compares complete retained trees and tests failure boundaries |
| Kexec options, generation offsets, server/variant selection, prompts/decline, exact native argv, HTTP/JSON/native errors and cleanup | `src/remote/kexec_netboot.rs`, `tests/kexec_options.rs` and `tests/rust_helpers_package.rs` |
| JSON number/string/collection compatibility and codec framing | `src/remote/json.rs`, its frozen numeric fixtures and the existing kexec body/HTTP tests |

The mapping replaces executable replay assertions, not supported compatibility.
The Rust conversions named after Ruby remain product behavior. Recorded baseline
defects remain explicit decisions: carrier surplus arguments do not gain hidden
side effects; a missing requested kexec variant still warns and loads without it;
an explicit server URL does not bypass the cmdline `httproot` requirement.

## Retained native test exception

The external vpsAdminOS test framework, the four native suites and
`tests/runner/extensions` remain unchanged. Two original helper sources remain
executable test fixtures:

- `tests/compat/helpers/baseline/auto-rollback.rb.txt` proves new-CLI/old-helper
  confirmation in `tests/suite/auto_rollback.nix`.
- `tests/compat/helpers/baseline/build-netboot-server.rb.txt` and
  `tests/support/legacy-netboot-builder.nix` compare old and Rust builder output in
  `tests/suite/carrier/netboot.nix`.

These are explicit test dependencies. They are not installed with the application,
used by ordinary candidate replay, or a Ruby runtime fallback. Keep their exact
bytes and mixed-version assertions. Native confctl and installed helper paths
select the Rust package. This retirement does not replace the VM DSL or runner.

Fake-process and loopback tests do not establish remote detachment, transport
loss, process-tree survival, lock contention, interrupted publication, actual
architecture execution or mixed-version native interoperability. The retained
VM tests and explicitly assigned native qualification remain responsible for
those boundaries. Ordinary Cargo tests do not launch that infrastructure. No Rust TLS/proxy
fixture or general trailing/member decode policy is established by this map;
retiring the paired HTTP recorder leaves those limits explicit.
