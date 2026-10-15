#!/usr/bin/env bash
# Feasibility only: private rootless Podman storage and nested namespace proof.
set -euo pipefail
if [ "$(id -u)" = 0 ]; then
  echo 'Refusing host root; this fixture requires rootless Podman' >&2
  exit 1
fi
fixture_probe_root=$(mktemp -d /tmp/2026-10-09-evaluate-confctl-rewrite-language-rootless-probe.XXXXXX)
install -d -m 700 "$fixture_probe_root/runtime" "$fixture_probe_root/storage" "$fixture_probe_root/runroot" "$fixture_probe_root/tmp"
printf '%s\n' "$fixture_probe_root"
fixture_podman() {
  env -u SSH_AUTH_SOCK -u CONTAINER_HOST -u CONTAINER_CONNECTION \
    XDG_RUNTIME_DIR="$fixture_probe_root/runtime" \
    podman --root "$fixture_probe_root/storage" --runroot "$fixture_probe_root/runroot" \
      --tmpdir "$fixture_probe_root/tmp" --storage-driver vfs \
      --cgroup-manager cgroupfs --events-backend file "$@"
}
fixture_podman info --format json > "$fixture_probe_root/info.json"
fixture_rootless=$(fixture_podman info --format '{{.Host.Security.Rootless}}')
if [ "$fixture_rootless" != true ]; then
  echo 'Podman did not prove rootless=true; refusing fallback' >&2
  exit 1
fi
fixture_podman unshare cat /proc/self/uid_map > "$fixture_probe_root/uid_map"
fixture_podman unshare cat /proc/self/gid_map > "$fixture_probe_root/gid_map"
printf 'Rootless namespace preflight passed: %s\n' "$fixture_probe_root"
