---
name: confctl-release
description: Prepare and publish confctl version releases using Cargo and Nix metadata, generated references, reviewed source and verified packages. Use for confctl releases, not releases of configurations managed by confctl.
---

# confctl releases

Read the repository's `AGENTS.md`, [packaging guide](../../docs/rust-packaging.md)
and applicable workspace instructions first. Separate release preparation from
publication. If asked to prepare a candidate, stop before default-branch
integration, tagging or uploading. The installed application does not use a gem
or load Ruby extensions; repository-tools Ruby and immutable compatibility
fixtures remain separate.

## Prepare the source

1. Confirm the target version and previous release tag. Review the actual
   changes, compatibility constraints, operator steps and rollback boundaries.
   Preserve the dated `CHANGELOG.md` format. Describe site script/settings/pin
   migration and paired helper producer/backend rollback when applicable.
2. Finish requested dependency or procedure changes before the release commit.
   Preserve `Cargo.lock` and the declared MSRV unless a separate change is
   approved. Use normal Cargo dependency resolution for an assigned update;
   inspect the locked graph and licenses. Do not refresh dependencies merely
   to make a release build pass.
3. Update the version in `Cargo.toml`, the owning package entry in `Cargo.lock`
   and `nix/rust-package.nix`, and add the changelog entry. Check that those
   identities agree; do not replace every matching dependency version.
4. Regenerate the CLI reference with the existing Cargo example into a complete
   temporary candidate. Refresh the options manual only from an actual
   `#moduleOptions` export, never synthetic fixture JSON. Follow the packaging
   guide's locked-input export and generation procedure.
5. Install `.githooks/pre-commit` through the documented local `core.hooksPath`
   setting. Commit the coherent release unit with subject `Version <version>`
   and a short rationale. Keep the release commit last after dependency and
   procedure changes. Consolidate unmerged corrections under the workspace Git
   policy and preserve published history. Run the declared hooks.

## Verify the candidate

Run ordinary formatting, locked all-target/all-feature check and strict Clippy,
plus reference renderer and CLI drift checks. Arrange independent final review
of the complete committed history, final diff and migration inventory before
longer checks. Then run the default suite and the assigned package/native gates.
All-feature compilation is not permission to execute qualification mains.

Build the explicit `packages.<system>.confctl.tests.qualification` derivation for
both supported native platforms in the approved builder environment. It enables
the full default test phase on the same recipe/source/lock/packages as the
ordinary runtime. The public CLI and helper packages deliberately have
`doCheck = false`; their successful build alone is not full-suite evidence.
Also build the actual public runtime package and its smoke and shell-selection
checks. Verify the installed CLI/helper names, manuals, license notices and
runtime references.
No Ruby application, Bundler, test source or compatibility archive belongs in
the installed product closure. Preserve site tools-mode Ruby as a separate
consumer. Prove the locked graph on the declared MSRV as well as the normal
CI toolchain. A cross-build alone does not establish native execution.

Release identity comes from the reviewed Cargo/lock/Nix package metadata and
its exact source revision. Use installed executable `--help` for the public
entrypoint smoke test. The parser deliberately retains its existing
`--version` error; do not invent a successful version flag or use that error as
release metadata proof. Installed wrappers execute the absolute compiled
backend; the source `bin/confctl` launcher may compile and is not an installed
artifact check.

Record exact source/package identities and any explicitly requested artifact
checksums with the actual review/check results. Publish a development branch
under workspace policy and check the exact head's CI when authorized. Keep
native mixed-helper/transport, site migration and operator acceptance distinct
from hosted Rust tests. Present remaining gaps honestly before publication.

## Publish after authorization

Confirm authorization for each repository/default branch, release tag and any
artifact destination. Recheck the final source, target branch and existing tag.
Integrate only under the workspace Git policy. Rebuild and reconcile approval
if the approved source or artifact changes.

Create an annotated `v<version>` tag at the approved release commit and push
only the authorized branch/tag over SSH. Publish the exact verified artifact
only to an explicitly selected destination; no registry upload is implied by
release preparation. Verify remote tag/source and artifact identities.

If a step fails, inspect the actual partial effects before retrying. Do not
rewrite a published release, delete a tag, replace an artifact or bump a version
as automatic recovery. Report the partial result and obtain direction for any
action outside the approved scope. Leave the development session open.
