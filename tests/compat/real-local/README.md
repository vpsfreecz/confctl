# Isolated real local Nix/OpenSSH tier

The rootless private-storage namespace probe passed in the development session;
source-only default metadata evaluation passed separately. Image construction,
OCI startup, store/cache gates, SSH authentication and measurements are separate
checks. None of those runtime checks is implied by the probe or source gate.

`evaluate.nix` imports the pinned original `mk-confctl-outputs.nix` directly and
uses real cluster/settings modules. Its default template lock has placeholders
and is only for source evaluation. `fixture.nix` generates immutable
flakes for 1/10/100/1000 machines with actual source path/narHash locks.
`image.nix` returns those configurations, metadata derivations, the distinct
single-node no-op build/autoRollback seeds, and a layered image with
`includeNixDB=true`. The pinned Nixpkgs implementation supports that argument.
The prepared `rootfs` directory is the sole image `contents` entry merged into
`/`. Its package manifest and environment file retain the package, source,
configuration, metadata and seed closures under `/nix/store`; `includeNixDB`
registers that closure for the local Nix store.

The fixture interpolates each source once to a contextful string and reuses it
for the flake URL, both lock paths and `fetchTree`. The lock keeps the fetched
source NAR hash. Nix can normalize the fetched tree to a different store name;
source assertions compare the raw-input and canonical-input normalized trees
and their NAR hashes. The configuration files must also pass a byte-level
URL/lock consistency check. Offline runtime evaluation must then agree with the
direct image preseed without updating the lock before timing begins.

Direct preseed also passes confctl as a fetched input object, matching the
runtime flake input. This keeps the rollback helper's dependency on the whole
source tree consistent in both evaluations. The original module import and
measured client packages still use their unchanged source inputs.

Direct preseed evaluates a normalized source tree of each complete generated
configuration. The tree retains the same files, executable bits and NAR hash,
while its registration has no references inherited from the configuration
derivation. Its lock can therefore be read as JSON without changing the reader
or lock bytes. The manifest retains both the generated configuration and its
`configurationSources` record with the exact `sourcePath` and `narHash`.

The package manifest carries `expectedRuntime` schema 1, generated from the
same direct module evaluation as the image seeds. Each scale records settings,
the complete machine-key map, metadata output and derivation digest, and the
metadata content digest. N1 also records the key and both tiny seed identities.
Each JSON identity is an object with exactly `outputPath` and `drvPathSha256`.
The field name avoids Nix's string coercion for attribute sets containing
`outPath`, which would discard the digest. Explicit host construction inventories
use `drvPath` and `outputPath`; package and configuration references in the
manifest remain strings. This unreleased schema 1 rejects string-shaped identity
records and requires no migration or dual-field decoder.
Derivation paths are hashed without their dependency context so this receipt
does not pull additional derivation closures into the image. Rootfs construction
checks the realized metadata bytes against the recorded content digests.

On every fresh container start, the entry records each original input file's
hash and boolean for the owner execute bit used by NAR from the immutable
configuration, and checks the working copy before Git freezing. Owner write
permissions may differ for the writable copy. It evaluates all four runtime
identity projections and compares their exact JSON fields before reading
metadata or building caches. A recursive, offline local-store query must show
every matched output and its references available; it does not realize missing
outputs. Metadata bytes are checked before and after the unchanged cache builds.
N1 seed paths and key must also match the
fixture environment before seed preparation.

Immediately before `compat-real`, the gate compares all records again, verifies
the original file bytes, owner-executable bits and tracked file set, and rechecks
metadata digests. It also requires each captured Git flake source path and root
locked NAR hash to match the corresponding `configurationSources` record.
A successful invocation writes
`preseed-gate.json`, with the package manifest,
runtime records, locks and original-file inventories identified by SHA256.
The receipt resolves the image-owned manifest symlink and hashes its regular
referent. Configuration input checks require regular files at their original
paths.
Mismatch, missing output, failed query or input mutation stops before timing.
Raw records and diagnostics remain in the artifact directory. This gate is
untimed setup; source checks and stub tests do not prove runtime agreement.

The pinned image builder joins `contents` with symlinks. Its customization step
replaces only the four `/work/config-N` trees' file links with regular copies of
the exact immutable configurations. Build assertions reject symlinks and other
nonregular leaves, compare the complete trees byte for byte before changing
permissions, and require owner-write permission afterward. Git can then track
the working files and Nix evaluates the flake inside that repository; source
paths, locks and bytes stay unchanged. Store closures and executable links retain
their original topology.

Before building the image, a watcher inspects the exact derivation plan for the
metadata and two tiny seed outputs. A missing base toolchain/Ruby closure or an
unexpected kernel build is a blocker to escalate; the plan does not authorize a
full OS build. The source gate is:

```sh
nix eval --impure --json --expr '
  import ./tests/compat/real-local/evaluate.nix {
    oracleSrc = /nix/store/IMMUTABLE-ORACLE-SOURCE;
    nixpkgsSrc = /nix/store/PINNED-NIXPKGS-SOURCE;
  }'
```

Construct the tier with pinned `pkgs`, the unchanged original package function,
standard native package and current tools package; inspect `tier.metadata`,
`tier.seed.toplevel` and `tier.seed.autoRollback` with `nix build --dry-run
--json` before realizing `tier.image`. Preserve source/package/lock/hash evidence
and roots. There is no host-store or host-daemon mount and no runtime download.

The host launcher requires Linux, Bash, GNU coreutils (`cat` and `du`), Podman
and Python 3 with its standard library. Python parses the JSON records, reads
available filesystem capacity and holds the nonblocking store lock. It is a
host requirement; the image and measurement clients do not gain a dependency.

After committed review of the shared-storage launcher, a watcher supplies the
verified session binding and initializes one new private loaded-image tuple:

```sh
export DEV_SESSION_SLUG=2026-10-09-evaluate-confctl-rewrite-language
export DEV_SESSION_WORKSPACE=/home/aither/workspace/ai/vpsfree.cz
bash tests/compat/real-local/run.sh init "$image_tar" "$immutable_sha256_image_id" "$import_capacity_json"
bash tests/compat/real-local/run.sh run "$printed_storage_root" "$run_json" "$run_capacity_json"
```

`init` allocates only a new
`/tmp/2026-10-09-evaluate-confctl-rewrite-language-real-storage.*` root and prints
it before loading. It retains setup evidence and a schema1 owner manifest with
the root device/inode, uid/gid, store ID, archive hash, image ID, Podman version
and fixed storage/runroot/runtime/tmp paths. The manifest becomes ready only
after load, exact image inspection and import accounting succeed. `run` accepts
that ready tuple, validates its manifest and live Podman facts, and inspects the
same exact image ID before every fresh container. It never reloads a missing
image. The launcher refuses arbitrary, symlinked, foreign, rebound or incomplete stores.

Capacity JSON contains exactly `schema: 1`, positive integer `reserveBytes`,
`reserveInodes`, `budgetBytes`, `budgetInodes`, and a nonempty `basis` explaining
the estimate and intended import or run-specification hash. All values must be
supplied. The accepted first import and bounded N1 calibration budgets are
48GiB (51539607552 bytes) and 26,000,000 inodes, with 20GiB (21474836480 bytes)
and 5,000,000 inodes reserved. The inode budget exceeds twice the reported used
inode count of the filesystem holding the retained import; it does not prove
peak temporary usage. Use an exact run-specification hash in the calibration
basis. Later budgets require observed growth and a scale-appropriate margin;
the launcher provides no capacity defaults. It records the invoking user's
available bytes/inodes for every distinct filesystem
holding the tuple or evidence and requires reserve plus budget before load/run.
The launcher retains capacity readings and refuses operations when inode
capacity is unknown or space is insufficient.
After recursive accounting, it retains fresh enforced byte/inode readings
immediately before container creation and again before foreground start. These
checks do not reserve space against other filesystem users.

One nonblocking exclusive lock covers setup or the whole foreground run and
evidence finalization. Each run allocates a new private evidence directory,
copies its specification/capacity inputs, and records store/manifest/inspection
hashes plus fresh container name and ID. A running, interrupted or unknown prior
marker blocks reuse until the lead diagnoses it. The launcher uses
`podman create --rm` with the fixed isolation flags, validates the full ID from
`podman.cid`, and fsyncs a separate `container.id` before
`podman start --attach --sig-proxy=true EXACT_ID`. Podman removes its cidfile with
the container; the separate identity capture remains in the evidence directory.
Creation or pre-start failure retains the refusal marker and any captured ID.
The attached start keeps the fixed fixture's noninteractive streams and forwards
signals under the same inherited store lock. A foreground return below 125 with
a known container ID records completion and its exit status, including workload
failures. Podman launcher errors (125-127), signal-style statuses and missing
container IDs retain the refusal marker.
Earlier run evidence remains intact. Allocated bytes/inodes and raw `du` readings
are recorded after import and outside each foreground workload. The watcher
records transient high-water consumption using filesystem queries during the
run; the launcher performs no recursive scans during sample timing.

Example run specification:

```json
{
  "schema": 1,
  "tier": "real-local-container",
  "scales": [1, 10, 100, 1000],
  "workflows": ["help", "ls", "status-none"],
  "rubyOnly": ["status-current", "status-cached-noop-build"],
  "samplesPerCell": 30,
  "sessionIndex": 1,
  "pairOrderSeed": 20261009,
  "cacheMode": "warm",
  "trace": false
}
```

Repeat with sessionIndex 2 in another fresh container/evidence/key set using the
same loaded-image tuple serially. Sharing image data and host page cache does
not make these independent cold disks; HOME, configuration, profiles and the
container's writable local Nix state are fresh for each run.
Selections can be narrowed for bounded commands; recommendations still require
30 pairs per selected cell in two sessions. Paired workflows reject unsupported
commands. Ruby-only existing-generation/cached-no-op cells are separate baseline
diagnostics. The seed output contains synthetic inputs metadata, not a bootable
OS. No deploy/profile activation, generation removal/rotation or GC occurs.

The launcher refuses host uid0, requires rootless=true and private vfs storage,
uses network=none, publishes no ports, and mounts only the new artifact directory.
`--no-hosts` preserves the image's localhost-only `/etc/hosts`. The fixture uses
literal loopback addresses and requires no external DNS resolver. The
mount-safety gate also applies to generated mounts onto resolved image store
paths.
The failed `real.AVa9kJ` root is never adopted or changed. The launcher retains
the shared tuple and every setup/run artifact for lead-owned disposition. The
foreground container is removed when its test lifetime ends; this is not a
development session lifecycle action. Namespace, OCI, loopback, store or
authentication failures stop this tier. There is no rootful, host-network or
host-Nix fallback.

Focused launcher checks use command and filesystem stubs, with no real Podman:

```sh
python3 -B tests/compat/real-local/run_test.py
python3 -B tests/compat/real-local/preseed_gate_test.py
```

Run as the unprivileged caller in the declared experimental environment with
the host Python requirement supplied. The tests retain their tiny stub artifacts
and drive the actual launcher through import/run, lock, ownership, image,
interruption and byte/inode refusal paths. Shell syntax can be checked with
`bash -n tests/compat/real-local/run.sh`. Runtime isolation/cache/SSH acceptance
still requires watcher execution after the affected review lanes pass.
The preseed tests execute the embedded Ruby comparator and the actual entry
setup segment with fixed filesystem locations rebound to disposable files and
Nix/trace/client command stubs. They preserve command flags, assert comparison
ordering before cache builds, and require a passed receipt before the sentinel.
No real Nix, SSH or container is run by these tests. Check entry syntax with
`bash -n tests/compat/real-local/entry.sh`.

The entry creates fixture-only keys and a private root sshd on 127.0.0.1:2222,
records effective sshd limits and disables fixture per-source penalties/startup
throttling explicitly. Every synthetic machine shares that service. The original
absolute executable and its fixed real Nix/OpenSSH/Git paths remain intact.
Real cat/bash/realpath/stat execute via SSH against seeded synthetic files.
Client/server configs, namespace maps, mounts/routes/listeners, package closures,
binary/fixture hashes, limits and resource snapshots are artifacts.

Audited synthetic sources/locks are tracked in real disposable Git repositories
with deterministic dummy identity/dates and no remotes. `.confctl/` is ignored,
so command logging does not change the flake source hash. Git prep and separate
strace diagnostic runs occur outside timed samples. Warm application state is
per tool; fresh mode copies the same frozen seeded configuration and resets HOME
for every invocation. Store outputs remain preseeded and OS page cache is not
flushed. Dirty state/source identities must be retained when diagnosing a cache
failure; no global source-hash normalizer is permitted.

`compat-real` captures raw/normalized CLI bytes/status/state, first stdout and
wall/CPU/RSS per sample, and affinity/cgroup/load snapshots around cells. Failed
or mismatched samples fail the run and remain available for analysis. Real-tier
samples are raw evidence; the fixture benchmark's accepted summary is separate.
Timed wall includes owned-tree start/finish, recursive procfs observation every
5ms and cleanup. That observer stays active with diagnostic tracing disabled;
child rusage excludes its controller CPU. Preparation, state snapshots and
normalization are outside the wall interval. Observer cost varies with process
topology; subtracting child CPU or external spans does not isolate removable
local work.
Rootless namespace/vfs and one-server contention add costs. These measurements
cannot establish WAN latency, independent-host fleet scaling, full site system
build cost, aggregate live memory peak or deployment compatibility.
