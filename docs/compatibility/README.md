# Compatibility catalogue and evidence

This catalogue describes the immutable Ruby reference at **cc40679d267165aecfa569128438bb55fe910268** and the two site workflows in configuration **ae670dc0d4a5d43adf9560da1a6a0a35925cd2e5**. It is a characterization inventory for the measurement-first experiment, not a claim that a replacement is complete. Source links use these revisions even when this worktree changes.

The accepted experiment is a separately named Go binary supporting help, `ls`, and `status --generation none`, plus an executable extension SDK and reimplementations of the two site workflows. Unsupported commands and status variants must fail before configuration evaluation, process execution or file mutation. Plain Ruby `status` builds; the experiment must not silently reinterpret it. Native deployment is outside this package. Deployment/rediscovery hook boundaries can be exercised by the harness without implementing deployment.

## Reading the evidence

- **S (source derived):** supported by linked reference code. Behavioral entries remain S unless explicitly marked U or promoted to O with evidence; provisional captures do not silently change this status.
- **O (runtime observed):** requires a checked-in observation record naming the reference revision, package derivation, fixture revision, environment, invocation, raw outputs/events/file manifests, normalizer version and result. An existing RSpec expectation alone is not O.
- **U (unverified):** a dependency, parser, signal, terminal or external-system detail needs execution against the packaged oracle. U is a coverage obligation, not permission to invent replacement behavior.
- **P (intentional new policy):** extension registration/protocol and removal of Ruby script loading are new API decisions. They do not establish parity for altered builtin defaults, resource limits or state semantics.

Fixture family IDs link to the [required case registry](fixtures.md#case-families); the separate [implemented case index](fixtures.md#implemented-case-index) pairs 41 exact inputs with their accepted **O** Ruby references: 39 packaged CLI cases and two original-hook adapter cases. The original [provenance](../../tests/compat/expected/provenance.json) and [SHA256SUMS](../../tests/compat/expected/SHA256SUMS), plus [supplemental provenance](../../tests/compat/expected/provenance-review-remediation.json) and [supplemental checksums](../../tests/compat/expected/SHA256SUMS-review-remediation), identify sources, packages, executables, fixtures, normalizers and retained raw captures. These packaged semantic-tool observations cover only the listed inputs. They establish neither candidate parity nor full family/command coverage, real Nix/SSH integration or performance. The harness implementer owns executable fixtures and observation files; the design owner owns this source inventory. Promote further cases only with accepted evidence. Keep S facts alongside contradictory observations and investigate the cause rather than rewriting the oracle to match the prototype.

## Index

| Contract | Catalogue | Required fixture families |
| --- | --- | --- |
| Parser, help, environment, prompts and output | [Commands](commands.md#shared-parser-and-option-contract), [interfaces](interfaces.md#startup-environment-and-ui) | `parser`, `ui`, `logging` |
| All 29 builtin leaf commands | [Command catalogue](commands.md) | `configuration`, `inputs`, `list`, `build`, `deploy`, `health`, `status`, `compare`, `ssh`, `generation`, `gen-data` |
| Inventory, Nix, SSH and process behavior | [Interfaces](interfaces.md#inventory-selection-and-order) | `selection`, `nix`, `process`, `status` |
| Local/remote generations, roots and recovery | [Persistence](interfaces.md#persisted-state-and-generations) | `generation`, `deploy`, `mutation` |
| Reimplemented site workflows | [Site contracts](interfaces.md#site-workflows-and-intentional-extension-policy) | `site-netboot`, `site-kernels`, `extension-policy` |
| Harness safety and measurement interpretation | [Fixtures and evidence](fixtures.md) | all families |

## Scope and decision boundary

Preserve builtin CLI/options, selection, ordering, streams, prompts, external argv, environment dependencies, files, remote helper protocols and recovery behavior. No compatibility with old Ruby extension source, GLI customization or internal Ruby object APIs is required. The two site behaviors are required through a new extension API; their old sources remain the behavioral oracle.

Do not call a new timeout, concurrency limit, automatic retry, cache, lock, sort order, output limit or default a drop-in change. First characterize the old behavior, then label and obtain acceptance for any intended difference. Existing suspicious behavior is documented explicitly: the deploy hook action field uses `opts[:action]`, generation rotation consults a surprising per-machine GC key, and some failures do not produce command failure.

The long-term objective includes a Ruby-free normal operational shell and replacing Overcommit. This experiment does not remove remaining service Ruby checks or fleet Ruby helpers. Effective inherited Overcommit 0.73.0 defaults still require an exact-source inventory before hook migration; listing the two explicit checks is insufficient.

Passing fixtures for the bounded prototype cannot establish complete replacement readiness. A later replacement requires all builtin families characterized and implemented, real local Nix/isolated SSH validation, persisted-state/rollback proof, accepted extension differences and independent review. No benchmark figures or performance benefit are asserted here.
