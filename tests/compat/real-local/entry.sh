#!/bin/bash
# Foreground fixture lifetime only. No host store, daemon or network fallback.
set -euo pipefail
# BEGIN fixture preseed comparator
fixture_preseed_gate() {
  "$CONFCTL_FIXTURE_RUBY" - /etc/confctl-fixture-packages.json "$@" <<'RUBY'
require 'json'
require 'digest'
require 'find'
require 'open3'

def refuse(field, detail)
  raise "#{field}: #{detail}"
end

def object(value, field)
  refuse(field, 'expected object') unless value.is_a?(Hash)
  value
end

def fields(value, names, field)
  object(value, field)
  refuse(field, "expected fields #{names.sort.join(',')}") unless value.keys.sort == names.sort
end

def digest(value, field)
  refuse(field, 'expected SHA256') unless value.is_a?(String) && value.match?(/\A[0-9a-f]{64}\z/)
end

def absolute_path(value, field)
  refuse(field, 'expected absolute path') unless value.is_a?(String) && value.start_with?('/') && !value.include?("\0")
end

def identity(value, field)
  fields(value, %w[outputPath drvPathSha256], field)
  absolute_path(value['outputPath'], "#{field}.outputPath")
  digest(value['drvPathSha256'], "#{field}.drvPathSha256")
end

def configuration_source(value, field)
  fields(value, %w[sourcePath narHash], field)
  absolute_path(value['sourcePath'], "#{field}.sourcePath")
  refuse("#{field}.narHash", 'expected nonempty NAR hash') unless value['narHash'].is_a?(String) && !value['narHash'].empty?
end

def runtime_record(value, count, expected: false)
  field = "scale #{count}"
  names = %w[settings machineKeys metadata seed]
  names += %w[metadataSha256] if expected
  fields(value, names, field)
  object(value['settings'], "#{field}.settings")
  object(value['machineKeys'], "#{field}.machineKeys").each do |name, key|
    refuse("#{field}.machineKeys", 'expected nonempty names/keys') unless name.is_a?(String) && !name.empty? && key.is_a?(String) && !key.empty?
  end
  identity(value['metadata'], "#{field}.metadata")
  digest(value['metadataSha256'], "#{field}.metadataSha256") if expected
  if count == '1'
    fields(value['seed'], %w[key toplevel autoRollback], "#{field}.seed")
    key = value['seed']['key']
    refuse("#{field}.seed.key", 'expected nonempty key') unless key.is_a?(String) && !key.empty?
    identity(value['seed']['toplevel'], "#{field}.seed.toplevel")
    identity(value['seed']['autoRollback'], "#{field}.seed.autoRollback")
  else
    refuse("#{field}.seed", 'expected null') unless value['seed'].nil?
  end
end

def equal(actual, expected, field)
  refuse(field, "type mismatch #{actual.class}/#{expected.class}") unless actual.class == expected.class
  case expected
  when Hash
    refuse(field, 'object keys differ') unless actual.keys.sort == expected.keys.sort
    expected.each { |key, value| equal(actual[key], value, "#{field}.#{key}") }
  when Array
    refuse(field, 'array lengths differ') unless actual.length == expected.length
    expected.each_with_index { |value, i| equal(actual[i], value, "#{field}[#{i}]") }
  else
    refuse(field, "expected #{expected.inspect}, got #{actual.inspect}") unless actual == expected
  end
end

def json_file(path)
  JSON.parse(File.read(path))
rescue JSON::ParserError, SystemCallError => e
  refuse(path, e.message)
end

def file_hash(path)
  refuse(path, 'expected regular file') unless File.lstat(path).file?
  Digest::SHA256.file(path).hexdigest
end

def input_files(root)
  refuse(root, 'expected real directory') unless File.lstat(root).directory?
  files = {}
  Find.find(root) do |path|
    stat = File.lstat(path)
    next if stat.directory?
    refuse(path, 'expected regular input file') unless stat.file?
    files[path.delete_prefix("#{root}/")] = { 'sha256' => file_hash(path),
                                             'ownerExecutable' => (stat.mode & 0o100) != 0 }
  end
  files.sort.to_h
end

def check_work_files(root, files, count)
  refuse(root, 'expected real directory') unless File.lstat(root).directory?
  files.each do |relative, record|
    parts = relative.split('/')
    refuse("scale #{count}.inputs", 'invalid relative path') if parts.any? { |part| ['', '.', '..'].include?(part) }
    parent = root
    parts[0...-1].each do |part|
      parent = File.join(parent, part)
      refuse(parent, 'expected real directory') unless File.lstat(parent).directory?
    end
    path = File.join(root, relative)
    stat = File.lstat(path)
    actual = { 'sha256' => file_hash(path), 'ownerExecutable' => (stat.mode & 0o100) != 0 }
    equal(actual, record, "scale #{count}.inputs.#{relative}")
  end
end

def compare_runtime(manifest, count, path)
  expected = manifest.fetch('expectedRuntime').fetch('scales').fetch(count)
  actual = json_file(path)
  runtime_record(actual, count)
  expected.each do |field, value|
    next if field == 'metadataSha256'
    equal(actual[field], value, "scale #{count}.#{field}")
  end
  return unless count == '1'

  equal(ENV['CONFCTL_FIXTURE_KEY'], expected['seed']['key'], 'scale 1.seed.environment.key')
  equal(ENV['CONFCTL_FIXTURE_SEED'], expected['seed']['toplevel']['outputPath'], 'scale 1.seed.environment.toplevel')
  equal(ENV['CONFCTL_FIXTURE_ROLLBACK'], expected['seed']['autoRollback']['outputPath'], 'scale 1.seed.environment.autoRollback')
end

def outputs(expected)
  paths = [expected.fetch('metadata').fetch('outputPath')]
  if expected['seed']
    paths += %w[toplevel autoRollback].map { |name| expected['seed'][name]['outputPath'] }
  end
  paths
end

def check_metadata(expected, count)
  equal(file_hash(expected['metadata']['outputPath']), expected['metadataSha256'], "scale #{count}.metadataSha256")
end

begin
  manifest_path, operation, *args = ARGV
  manifest = json_file(manifest_path)
  object(manifest, 'manifest')
  expected_runtime = manifest['expectedRuntime']
  fields(expected_runtime, %w[schema scales], 'expectedRuntime')
  equal(expected_runtime['schema'], 1, 'expectedRuntime.schema')
  scales = %w[1 10 100 1000]
  fields(expected_runtime['scales'], scales, 'expectedRuntime.scales')
  fields(manifest['configs'], scales, 'configs')
  fields(manifest['configurationSources'], scales, 'configurationSources')
  scales.each do |count|
    runtime_record(expected_runtime['scales'][count], count, expected: true)
    absolute_path(manifest['configs'][count], "configs.#{count}")
    configuration_source(manifest['configurationSources'][count], "configurationSources.#{count}")
  end
  if operation == 'final'
    artifacts, work = args
    refuse('final', 'expected artifact and work directories') unless args.length == 2
    runtime_hashes = {}
    locks = {}
    inventories = {}
    scales.each do |count|
      runtime_path = File.join(artifacts, "runtime-preseed-#{count}.json")
      compare_runtime(manifest, count, runtime_path)
      source_metadata = json_file(File.join(artifacts, "config-#{count}-source-metadata.json"))
      source = manifest['configurationSources'][count]
      field = "scale #{count}.configurationSource"
      object(source_metadata, field)
      locked = object(source_metadata['locked'], "#{field}.locked")
      equal(source_metadata['path'], source['sourcePath'], "#{field}.path")
      equal(locked['narHash'], source['narHash'], "#{field}.locked.narHash")
      inventory_path = File.join(artifacts, "original-inputs-#{count}.json")
      inventory = json_file(inventory_path)
      original = { 'schema' => 1, 'scale' => count, 'source' => manifest['configs'][count], 'files' => input_files(manifest['configs'][count]) }
      equal(inventory, original, "scale #{count}.originalInventory")
      root = File.join(work, "config-#{count}")
      check_work_files(root, inventory['files'], count)
      tracked, stderr, status = Open3.capture3('git', '-C', root, 'ls-files', '-z')
      refuse("scale #{count}.trackedInputs", stderr) unless status.success?
      equal(tracked.split("\0").sort, inventory['files'].keys.sort, "scale #{count}.trackedInputs")
      check_metadata(expected_runtime['scales'][count], count)
      runtime_hashes[count] = file_hash(runtime_path)
      locks[count] = file_hash(File.join(root, 'flake.lock'))
      inventories[count] = file_hash(inventory_path)
    end
    receipt = { 'schema' => 1, 'status' => 'passed', 'packageManifestSha256' => file_hash(File.realpath(manifest_path)),
                'runtimeRecordSha256s' => runtime_hashes, 'lockSha256s' => locks,
                'originalFileInventorySha256s' => inventories }
    File.write(File.join(artifacts, 'preseed-gate.json'), "#{JSON.pretty_generate(receipt)}\n")
  else
    count, *rest = args
    refuse('scale', 'unsupported scale') unless scales.include?(count)
    expected = expected_runtime['scales'][count]
    case operation
    when 'snapshot'
      root, inventory_path = rest
      refuse('snapshot', 'expected work directory and inventory path') unless rest.length == 2
      files = input_files(manifest['configs'][count])
      equal(input_files(root), files, "scale #{count}.initialInputs")
      inventory = { 'schema' => 1, 'scale' => count, 'source' => manifest['configs'][count], 'files' => files }
      File.write(inventory_path, "#{JSON.pretty_generate(inventory)}\n")
    when 'compare'
      refuse('compare', 'expected runtime record path') unless rest.length == 1
      compare_runtime(manifest, count, rest.first)
    when 'outputs'
      refuse('outputs', 'unexpected argument') unless rest.empty?
      puts outputs(expected)
    when 'available'
      refuse('available', 'expected query path') unless rest.length == 1
      nodes = object(json_file(rest.first), "scale #{count}.pathInfo")
      outputs(expected).each { |path| object(nodes[path], "scale #{count}.pathInfo.#{path}") }
      nodes.each do |path, node|
        absolute_path(path, "scale #{count}.pathInfo.path")
        object(node, "scale #{count}.pathInfo.#{path}")
        refs = node['references']
        refuse("scale #{count}.pathInfo.#{path}.references", 'expected array') unless refs.is_a?(Array)
        refs.each do |reference|
          absolute_path(reference, "scale #{count}.pathInfo.reference")
          object(nodes[reference], "scale #{count}.pathInfo.#{reference}")
        end
      end
      check_metadata(expected, count)
    when 'metadata'
      refuse('metadata', 'unexpected argument') unless rest.empty?
      check_metadata(expected, count)
    else
      refuse('operation', 'unsupported operation')
    end
  end
rescue StandardError => e
  warn "preseed gate: #{e.message}"
  exit 1
end
RUBY
}
# END fixture preseed comparator
[ "$(id -u)" = 0 ]
[ $# = 1 ] && [ "$1" = /artifacts/run.json ]
[ -f "$1" ]
[ -z "${SSH_AUTH_SOCK-}" ]
[ ! -S /nix/var/nix/daemon-socket/socket ]
. /etc/confctl-fixture.env
export USER=root HOME=/root NIX_REMOTE=local CONFCTL_SSH_CONFIG=/run/fixture/ssh_config CONFCTL_REAL_FIXTURE=1
export PATH=/bin CONFCTL_TTY=0 NO_COLOR=1 PAGER= TZ=UTC LANG=C.UTF-8
mkdir -p /run/fixture /root/.ssh /etc/confctl /nix/var/nix/profiles /nix/var/nix/gcroots/per-user/root
chmod 700 /run/fixture
# network=none must provide loopback; any other interface/default route fails.
ip -j address > /artifacts/network.json
ip -j route > /artifacts/routes.json
if ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 | grep -v '^lo$'; then echo 'Unexpected non-loopback interface' >&2; exit 1; fi
[ -z "$(ip route show default)" ]
ip link show lo | grep 'UP' > /dev/null
cat /proc/self/uid_map > /artifacts/uid-map
cat /proc/self/gid_map > /artifacts/gid-map
cat /proc/self/mountinfo > /artifacts/mountinfo
cat /proc/self/limits > /artifacts/limits
cp /etc/confctl-fixture-packages.json /artifacts/packages.json
cp /etc/nix/nix.conf /artifacts/nix.conf
# No outside bind is permitted except the declared artifact mount.
if awk '$5 ~ /^\/nix\/store\/|^\/root\// { found=1 } END { exit !found }' /proc/self/mountinfo; then echo 'Unexpected host store/home mounts' >&2; exit 1; fi
ssh-keygen -q -t ed25519 -N '' -f /run/fixture/host_ed25519
ssh-keygen -q -t ed25519 -N '' -f /run/fixture/client_ed25519
cp /run/fixture/client_ed25519.pub /run/fixture/authorized_keys
chmod 600 /run/fixture/authorized_keys
read -r fixture_key_type fixture_key_data _ < /run/fixture/host_ed25519.pub
printf 'confctl-fixture %s %s\n' "$fixture_key_type" "$fixture_key_data" > /run/fixture/known_hosts
cat > /run/fixture/sshd_config <<'SSHD'
Port 2222
ListenAddress 127.0.0.1
HostKey /run/fixture/host_ed25519
PidFile /run/fixture/sshd.pid
AuthorizedKeysFile /run/fixture/authorized_keys
PermitRootLogin prohibit-password
PubkeyAuthentication yes
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PermitEmptyPasswords no
AllowUsers root
StrictModes yes
UseDNS no
AllowAgentForwarding no
AllowTcpForwarding no
X11Forwarding no
PermitTunnel no
PermitUserEnvironment no
MaxStartups 2048:100:2048
PerSourceMaxStartups none
PerSourcePenalties no
LogLevel ERROR
SSHD
cat > /run/fixture/ssh_config <<'SSH'
Host 127.0.0.1
  HostName 127.0.0.1
  HostKeyAlias confctl-fixture
  IdentityFile /run/fixture/client_ed25519
  IdentitiesOnly yes
  IdentityAgent none
  UserKnownHostsFile /run/fixture/known_hosts
  GlobalKnownHostsFile /dev/null
  StrictHostKeyChecking yes
  BatchMode yes
  ProxyCommand none
  ProxyJump none
  ControlMaster no
  ControlPath none
  LogLevel ERROR
SSH
/bin/sshd -t -f /run/fixture/sshd_config
/bin/sshd -T -f /run/fixture/sshd_config > /artifacts/sshd-effective.conf
cp /run/fixture/ssh_config /artifacts/ssh_config
/bin/sshd -D -e -f /run/fixture/sshd_config > /artifacts/sshd.stdout 2> /artifacts/sshd.stderr &
fixture_sshd_pid=$!
trap 'kill "$fixture_sshd_pid" 2>/dev/null || true; wait "$fixture_sshd_pid" 2>/dev/null || true' EXIT
fixture_ready=0
for attempt in $(seq 1 100); do
  if ssh -F /run/fixture/ssh_config -l root -p 2222 127.0.0.1 'id -u' > /run/fixture/remote-uid 2>/run/fixture/readiness.stderr && [ "$(cat /run/fixture/remote-uid)" = 0 ]; then fixture_ready=1; break; fi
  kill -0 "$fixture_sshd_pid"
  sleep 0.05
done
[ "$fixture_ready" = 1 ]
ss -ltn > /artifacts/listeners
if ss -H -ltn | awk '{print $4}' | grep -v '^127.0.0.1:2222$'; then echo 'Unexpected listener' >&2; exit 1; fi
# BEGIN fixture preseed setup
for count in 1 10 100 1000; do
  cd "/work/config-$count"
  fixture_preseed_gate snapshot "$count" "$PWD" "/artifacts/original-inputs-$count.json"
  git init -q
  git config user.name 'confctl fixture'
  git config user.email 'fixture@example.invalid'
  git add .
  GIT_AUTHOR_DATE='2026-10-09T00:00:00Z' GIT_COMMITTER_DATE='2026-10-09T00:00:00Z' git commit -qm 'Freeze synthetic configuration'
  fixture_seed_projection=null
  if [ "$count" = 1 ]; then
    fixture_seed_projection='let k = c.machineKeys."lab/nodes/node0001"; in { key = k; toplevel = identity c.build.${k}.toplevel; autoRollback = identity c.build.${k}.autoRollback; }'
  fi
  fixture_projection="c: let identity = d: { outputPath = d.outPath; drvPathSha256 = builtins.hashString \"sha256\" d.drvPath; }; in { settings = c.settings; machineKeys = c.machineKeys; metadata = identity c.machinesJson; seed = $fixture_seed_projection; }"
  nix eval --offline --json --no-write-lock-file --no-update-lock-file --max-jobs 0 .#confctl --apply "$fixture_projection" > "/artifacts/runtime-preseed-$count.json" 2> "/artifacts/runtime-preseed-$count.stderr"
  fixture_preseed_gate compare "$count" "/artifacts/runtime-preseed-$count.json"
done
# Compare every scale's identity before reading metadata or building any cache.
for count in 1 10 100 1000; do
  cd "/work/config-$count"
  fixture_output_lines="$(fixture_preseed_gate outputs "$count")"
  mapfile -t fixture_outputs <<< "$fixture_output_lines"
  nix path-info --offline --json --json-format 1 --recursive "${fixture_outputs[@]}" > "/artifacts/preseed-path-info-$count.json" 2> "/artifacts/preseed-path-info-$count.stderr"
  fixture_preseed_gate available "$count" "/artifacts/preseed-path-info-$count.json"
  mkdir -p .confctl/logs
  nix build --offline --no-link --no-write-lock-file --no-update-lock-file --max-jobs 0 .#confctl.machinesJson > "/artifacts/cache-$count.stdout" 2> "/artifacts/cache-$count.stderr"
  fixture_preseed_gate metadata "$count"
  git status --porcelain=v1 --untracked-files=all > "/artifacts/config-$count-git-status"
  nix flake metadata --offline --json --no-write-lock-file --no-update-lock-file > "/artifacts/config-$count-source-metadata.json"
  sha256sum flake.nix flake.lock count.nix cluster/inventory.nix > "/artifacts/config-$count.sha256"
done
cd /work/config-1
# Only preseeded tiny distinct build attribute; full system toplevel excluded.
nix build --offline --no-link --no-write-lock-file --no-update-lock-file --max-jobs 0 ".#confctl.build.$CONFCTL_FIXTURE_KEY.toplevel" ".#confctl.build.$CONFCTL_FIXTURE_KEY.autoRollback"
[ "$("$CONFCTL_FIXTURE_RUBY" -retc -e 'puts(Etc.getlogin || ENV["USER"])')" = root ]
"$CONFCTL_REAL_ORACLE" build --yes --max-jobs 0 lab/nodes/node0001 > /artifacts/seed-build.stdout 2>/artifacts/seed-build.stderr
ln -s "$CONFCTL_FIXTURE_SEED" /run/current-system
ln -s "$CONFCTL_FIXTURE_SEED" /nix/var/nix/profiles/system
ln -s "$CONFCTL_FIXTURE_SEED" /nix/var/nix/profiles/system-1-link
cp "$CONFCTL_FIXTURE_SEED/etc/confctl/inputs-info.json" /etc/confctl/inputs-info.json
nix path-info --recursive --json "$(dirname "$(dirname "$CONFCTL_REAL_ORACLE")")" "$(dirname "$(dirname "$CONFCTL_REAL_NATIVE")")" > /artifacts/closure.json
sha256sum "$CONFCTL_REAL_ORACLE" "$CONFCTL_REAL_NATIVE" > /artifacts/binaries.sha256
# Separate trace capture; tracing is outside all measured intervals.
strace -f -tt -s 65536 -e trace=process,execve -o /artifacts/ruby-process.trace "$CONFCTL_REAL_ORACLE" status --yes --generation none > /artifacts/ruby-gate.stdout 2>/artifacts/ruby-gate.stderr
strace -f -tt -s 65536 -e trace=process,execve -o /artifacts/native-process.trace "$CONFCTL_REAL_NATIVE" status --yes --generation none > /artifacts/native-gate.stdout 2>/artifacts/native-gate.stderr
fixture_preseed_gate final /artifacts /work
compat-real "$1"
