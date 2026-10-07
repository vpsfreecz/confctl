---
name: confctl-release
description: Prepare and publish confctl version releases, including changelog, dependency metadata, final release commit and verified RubyGems artifact. Use for confctl releases, not releases of configurations managed by confctl.
---

# confctl releases

Read the repository's `AGENTS.md` and applicable workspace instructions first.
Distinguish release preparation from authorized publication. If the user asks
to prepare a release and wait, stop before default-branch integration, tagging
and RubyGems upload. Updating a release candidate does not expand that approval.

## Prepare the changes

1. Confirm the target version and inspect the latest release tag, current remote
   default branch and RubyGems versions. Summarize material operator-visible
   changes from the previous tag to the selected source, checking current code
   and project guides. Record runtime requirements, upgrade steps and explicit
   deprecation decisions. Preserve the dated `CHANGELOG.md` format.
2. Finish requested dependency updates and release-procedure changes before
   the release commit. For a full lock refresh, run `bundle update` within the
   existing Gemfile/gemspec constraints. Resolve using the minimum supported
   Ruby from `confctl.gemspec`, not just the development shell's newer Ruby.
   Inspect updates for runtime compatibility; changing declared constraints or
   Ruby support is a separate compatibility decision.
3. Regenerate `gemset.nix` with Bundix after lock changes:

   ```sh
   nix develop -c nix shell --inputs-from . nixpkgs#bundix -c bundix
   nix develop -c nixfmt gemset.nix
   ```

   `gemset.nix` is generated dependency metadata. Do not maintain it by hand.
   `nix/package.nix` and the flake RSpec check pass `gemdir` to `bundlerEnv`,
   which imports that directory's `gemset.nix`. The development shell instead
   runs Bundler directly, so passing shell tests does not validate the Nix
   package's gemset. Keep a dependency refresh in its own commit.
4. Update `lib/confctl/version.rb`, refresh the confctl path-gem version in
   `Gemfile.lock` with Bundler, and regenerate `gemset.nix` with Bundix again.
   Add the changelog entry. Verify all four release identifiers agree:
   changelog, version constant, lockfile's `confctl` entry and gemset's
   `confctl.version`. Other gems can legitimately have the old release's
   version number; do not replace matching numbers globally.
5. Commit this release unit with subject `Version <version>` and a concise
   rationale. The release commit must be the final commit, after dependency
   and procedure changes. If preparation needs a correction, consolidate the
   unmerged history so this remains true; preserve published default-branch
   history. Run the declared hooks rather than bypassing them.

## Verify the candidate

Run quick metadata, syntax, whitespace and hook checks, then follow the
applicable independent final-review procedure for the complete committed
branch. Provide its full commit series, final diff and migration inventory.
After review, run the repository's RSpec, RuboCop and Nix formatting checks,
including the minimum supported Ruby when dependencies changed. Build the
flake's actual `confctl` package as well as the Ruby gem. Verify Nix's installed
confctl gem metadata matches the release version.

The gemspec uses a directory glob when `.git` is not a directory. In a Git
worktree this can include local `.gems`, `.bin`, caches or the `.git` file.
Build from a clean `git archive` export of the final commit, reusing the
provisioned Bundler dependency paths, then run `bundle exec rake build` there.
The rake build task generates the manual and HTML pages. Inspect the archive's
file list against committed source plus those generated files. Verify version,
Ruby requirement, executable and changelog, and retain the artifact's checksum.
Do not run `rake release` to prepare a candidate: it also tags, pushes and uploads.

For a packaged CLI smoke test, extract the gem and load its libraries directly.
Clear Bundler injection with `Bundler.with_unbundled_env` (or an equivalent
clean child environment), retain dependency `GEM_HOME`/`GEM_PATH`, and set
`RUBYLIB` to the extracted `lib`. Assert `ConfCtl.root` and the loaded
`confctl.rb` come from the extracted tree before running `--help`. In the
current CLI, GLI's `--version` is unset; inspect `ConfCtl::VERSION` and gemspec
metadata rather than accepting a version check that only exercises source code.

Publish the development branch according to repository/workspace policy and
check CI for its exact final head. When replacing a published candidate,
use the expected old head as the force-with-lease condition and handle
superseded CI under the applicable workspace rules. Present the final head,
diff, review/check results and artifact checksum for approval.

## Publish after authorization

Confirm authorization covers the repository's default branch, release tag and
RubyGems upload. Recheck the remote default branch and existing tag/version.
Integrate the approved branch under the repository/workspace Git policy. If
the source changes, rebuild and verify the artifact and reconcile approval
scope before publication. Keep the final release commit at the tip.

Create an annotated `v<version>` tag at the approved release commit and push
the authorized branch and tag over SSH. Upload the exact verified gem with
`gem push <artifact>` using the configured RubyGems authentication; never print
credentials. Verify that RubyGems reports the expected version, Ruby requirement
and artifact SHA-256, and that the remote tag resolves to the approved commit.

If any publication step fails, inspect which branch, tag and gem writes
succeeded before retrying. RubyGems versions cannot be overwritten. Do not
delete a tag, yank a gem, substitute a rebuilt artifact or bump the version
as an automatic recovery step. Record the partial result and obtain direction
for any action outside the approved release. Keep the session available for
follow-up under its lifecycle policy.
