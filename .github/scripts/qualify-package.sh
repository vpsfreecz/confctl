#!/usr/bin/env bash
set -euo pipefail

system=${1:?Usage: qualify-package.sh SYSTEM [GITHUB_OUTPUT]}
case "$system:$(uname -m)" in
  x86_64-linux:x86_64|aarch64-linux:aarch64) ;;
  *) echo 'Native package architecture mismatch' >&2; exit 1 ;;
esac
test "$(nix config show system)" = "$system"
git diff --quiet
git diff --cached --quiet
revision=$(git rev-parse HEAD)
ref="git+file://$PWD?rev=$revision"
guards=(--store daemon --no-update-lock-file --no-write-lock-file
  --option allow-import-from-derivation false
  --option accept-flake-config false)
package="$ref#packages.$system.confctl"
alias="$ref#packages.$system.confctl-rust"
qualification="$package.tests.qualification"

printf 'Revision: %s\nSystem: %s\n' "$revision" "$system"
nix flake metadata "${guards[@]}" --json "$ref"
runtime_drv=$(nix eval "${guards[@]}" --raw "$package.drvPath")
test "$runtime_drv" = "$(nix eval "${guards[@]}" --raw "$alias.drvPath")"
test "$(nix eval "${guards[@]}" --json "$package.doCheck")" = false
test "$(nix eval "${guards[@]}" --json "$qualification.doCheck")" = true
test "$(nix eval "${guards[@]}" --raw "$package.src")" = "$(nix eval "${guards[@]}" --raw "$qualification.src")"
test "$(nix eval "${guards[@]}" --raw "$package.system")" = "$system"
printf 'Runtime derivation: %s\nQualification derivation: %s\n' "$runtime_drv" \
  "$(nix eval "${guards[@]}" --raw "$qualification.drvPath")"

# Qualification is explicit; it must never become a runtime dependency.
nix build "${guards[@]}" --no-link --print-out-paths --print-build-logs "$qualification"
package_out=$(nix build "${guards[@]}" --no-link --print-out-paths --print-build-logs "$package")
test "$package_out" = "$(nix eval "${guards[@]}" --raw "$package.outPath")"
test -d "$package_out"
printf 'Package output: %s\n' "$package_out"
nix build "${guards[@]}" --no-link --print-build-logs "$package.tests.smoke" "$package.tests.shellSelection"

for name in confctl confctl-rust; do
  "$package_out/bin/$name" --help > "$RUNNER_TEMP/$name-help"
  test -s "$RUNNER_TEMP/$name-help"
done
for name in confctl confctl-reference confctl-options.nix kexec-netboot; do
  manual="$package_out/share/man/man8/$name.8"
  test -s "$manual"
  grep -q '^\.TH ' "$manual"
done
for notice in LICENSE.txt Boost-1.0.txt; do
  test -s "$package_out/share/licenses/confctl/$notice"
done
unwanted=$(find "$package_out" \( -type d \( -name src -o -name tests -o -name spec -o -name oracles \) -o -type f -name '*.rb' \) -print)
test -z "$unwanted" || { printf 'Unexpected installed source: %s\n' "$unwanted" >&2; exit 1; }
nix path-info --store daemon --recursive "$package_out" > "$RUNNER_TEMP/confctl-runtime-closure"
cat "$RUNNER_TEMP/confctl-runtime-closure"
if sed 's|^/nix/store/[^-]*-||' "$RUNNER_TEMP/confctl-runtime-closure" | grep -E '^(ruby|bundler|pandoc)(-[0-9]|$)'; then
  echo 'Unexpected Ruby, Bundler or Pandoc runtime dependency' >&2
  exit 1
fi
git diff --quiet
git diff --cached --quiet
test "$(git rev-parse HEAD)" = "$revision"
if [ "$#" -ge 2 ]; then
  printf 'runtime=%s\nrevision=%s\n' "$package_out" "$revision" >> "$2"
fi
