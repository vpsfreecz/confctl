# frozen_string_literal: true

require_relative 'support'

RSpec.describe 'Ruby site behavior oracle' do
  def machine(name, spin: 'vpsadminos', managed: true, carrier: nil, netboot: true, target: "#{name.tr('/#', '--')}.invalid", tags: [])
    {
      'name' => name, 'key' => "key_#{name.tr('/#', '__')}", 'clusterName' => name, 'carrier' => carrier,
      'metaConfig' => {
        'spin' => spin, 'managed' => managed, 'netboot' => { 'enable' => netboot },
        'host' => { 'target' => target, 'port' => 22 }, 'tags' => tags, 'healthChecks' => {}
      }
    }
  end

  def inventory
    [machine('nodes/a', tags: ['staging']), machine('nodes/b', managed: false),
     machine('carrier#b', carrier: 'carrier'), machine('nixos/live', spin: 'nixos'),
     machine('nodes/off', netboot: false)]
  end

  def run_site(id, argv, machines: inventory, files: {}, input: '', **extra)
    CompatibilityOracle.run('site', { 'id' => id, 'argv' => argv, 'machines' => machines, 'files' => files }.merge(extra.transform_keys(&:to_s)), input: input)
  end

  def events(result, name)
    result.fetch('observation').fetch('events').select { |event| event.fetch('event') == name }
  end

  def file(result, path)
    result.fetch('observation').fetch('files').fetch(path)
  end

  def kernels(result)
    JSON.parse(file(result, 'configs/node/kernels.json'))
  end

  it 'verifies the exact two site sources and their repository revision' do
    manifest = CompatibilityOracle.verify_sources(__dir__)
    expect(manifest.fetch('sources').length).to eq(2)
    expect(manifest.fetch('revision')).to eq('a4847bd6bbf2a62c107cff30b47452a60548a33e')
  end

  it 'registers static-equivalent help without machine evaluation or state access' do
    result = run_site('site-help', %w[runtime-kernels update --help])
    expect(result.fetch('exit_status')).to eq(0)
    expect(result.fetch('stdout')).to include('machine-pattern', '--[no-]show-trace', '--attr', '--tag', '--[no-]yes')
    expect(result.fetch('application_stderr')).to eq('')
    expect(result.fetch('observation').fetch('events')).to eq([])
    expect(file(result, 'configs/node/kernels.json')).to be_nil
  end

  it 'updates explicit unmanaged/carried entries and retains unselected state' do
    result = run_site('site-explicit-scope', %w[runtime-kernels update -y], files: { 'configs/node/kernels.json' => '{"other":"5.10.1"}' })
    expect(result.fetch('exit_status')).to eq(0)
    expect(events(result, 'exec').map { |event| event['name'] }).to contain_exactly('nodes/a', 'nodes/b', 'carrier#b', 'nodes/off')
    expect(kernels(result)).to include('other' => '5.10.1', 'carrier#b' => '6.12.3-extra', 'nodes/b' => '6.12.3-extra')
    expect(result.fetch('stdout')).not_to include('Continue?', 'will be queried')
    expect(result.fetch('stdout').lines.first).to eq("#{'HOST'.ljust(50)} KERNEL\n")
    expect(file(result, 'configs/node/kernels.json')).not_to end_with("\n")
  end

  {
    'name' => ['nodes/a', ['nodes/a']],
    'key' => ['key_nodes_a', ['nodes/a']],
    'brace' => ['{nodes/a,nodes/b}', %w[nodes/a nodes/b]],
    'empty' => ['absent/*', []]
  }.each do |label, (pattern, names)|
    it "preserves #{label} selection" do
      result = run_site("site-selection-#{label}", ['runtime-kernels', 'update', '-y', pattern])
      expect(result.fetch('exit_status')).to eq(0)
      expect(events(result, 'exec').map { |event| event['name'] }).to match_array(names)
      expect(kernels(result).keys).to eq(names)
    end
  end

  it 'combines repeated attribute and positive/negative tag filters' do
    result = run_site('site-filters', %w[runtime-kernels update -y --show-trace -a managed=true -a spin!=nixos -t staging -t ^production])
    expect(result.fetch('exit_status')).to eq(0)
    expect(kernels(result).keys).to eq(['nodes/a'])
  end

  it 'loops on blank and yes words, confirms on y and prints the configured preview' do
    result = run_site('site-confirm-loop', %w[runtime-kernels update nodes/a], input: "\nyes\ny\n")
    expect(result.fetch('exit_status')).to eq(0)
    expect(result.fetch('stdout').scan('Continue?').length).to eq(3)
    expect(result.fetch('stdout')).to include('The following machines will be queried:', 'HOST.TARGET')
    expect(kernels(result)).to eq('nodes/a' => '6.12.3-extra')
  end

  ['n', ''].each do |answer|
    it "aborts before querying on #{answer.empty? ? 'EOF' : 'decline'}" do
      result = run_site("site-confirm-#{answer.empty? ? 'eof' : 'decline'}", %w[runtime-kernels update nodes/a], input: answer.empty? ? '' : "n\n")
      expect(result.fetch('exit_status')).to eq(1)
      expect(result.fetch('stderr')).to include(answer.empty? ? 'end of file' : 'Aborted')
      expect(events(result, 'exec')).to eq([])
      expect(file(result, 'configs/node/kernels.json')).to be_nil
    end
  end

  it 'rejects excess arguments with framework exit 64 before inventory' do
    result = run_site('site-excess-args', %w[runtime-kernels update -y nodes/a surplus])
    expect(result.fetch('exit_status')).to eq(64)
    expect(result.fetch('stderr')).to include('unknown argument: surplus')
    expect(events(result, 'exec')).to eq([])
  end

  it 'keeps table selection order when commands complete in reverse order' do
    result = run_site('site-completion-order', %w[runtime-kernels update -y], machines: inventory.first(2), responses: { 'nodes/a' => { 'delay' => 0.03 } })
    expect(result.fetch('exit_status')).to eq(0)
    expect(events(result, 'exec.complete').map { |event| event['name'] }).to eq(%w[nodes/b nodes/a])
    expect(result.fetch('stdout').lines.drop(1).map { |line| line.split.first }).to eq(%w[nodes/a nodes/b])
  end

  it 'deletes a failed selected host, preserves other keys and still succeeds' do
    result = run_site('site-host-failure', %w[runtime-kernels update -y], machines: inventory.first(2),
                                                                          files: { 'configs/node/kernels.json' => '{"nodes/a":"old","nodes/b":"old","unselected":"keep"}' },
                                                                          responses: { 'nodes/a' => { 'status' => 255, 'stderr' => 'fixture SSH failure' } })
    expect(result.fetch('exit_status')).to eq(0)
    expect(result.fetch('application_stderr')).to eq('')
    expect(result.fetch('stdout')).to include('Error on nodes/a:', 'exit status: 255')
    expect(kernels(result)).to eq('nodes/b' => '6.12.3-extra', 'unselected' => 'keep')
  end

  it 'normalizes dot components without semver parsing and keeps literal error unchanged' do
    responses = { 'nodes/a' => { 'stdout' => " 6.9..extra.more\n", 'stderr' => 'not kernel data' },
                  'nodes/b' => { 'stdout' => '' }, 'carrier#b' => { 'stdout' => "error\n" },
                  'nodes/off' => { 'stdout' => "1.2...\n" } }
    result = run_site('site-normalization', %w[runtime-kernels update -y], responses: responses,
                                                                           files: { 'configs/node/kernels.json' => '{"carrier#b":"keep"}' })
    expect(result.fetch('exit_status')).to eq(0)
    expect(kernels(result)).to eq('carrier#b' => 'keep', 'nodes/a' => '6.9.', 'nodes/b' => '', 'nodes/off' => '1.2')
  end

  it 'does not replace malformed JSON or start queries after the decode failure' do
    result = run_site('site-corrupt-json', %w[runtime-kernels update -y], files: { 'configs/node/kernels.json' => '{bad' })
    expect(result.fetch('exit_status')).to eq(1)
    expect(events(result, 'exec')).to eq([])
    expect(file(result, 'configs/node/kernels.json')).to eq('{bad')
  end

  it 'reserializes a scalar unchanged when no machines match' do
    result = run_site('site-zero-scalar', %w[runtime-kernels update -y absent], files: { 'configs/node/kernels.json' => '17' })
    expect(result.fetch('exit_status')).to eq(0)
    expect(file(result, 'configs/node/kernels.json')).to eq('17')
  end

  it 'fails an invalid state shape when a selected success needs string-key assignment' do
    result = run_site('site-scalar-selected', %w[runtime-kernels update -y nodes/a], files: { 'configs/node/kernels.json' => '17' })
    expect(result.fetch('exit_status')).to eq(1)
    expect(file(result, 'configs/node/kernels.json')).to eq('17')
  end

  it 'records local versus SSH routing with explicit port and config' do
    machines = [machine('local', target: 'localhost'), machine('remote')]
    machines.last['metaConfig']['host']['port'] = 2222
    result = run_site('site-ssh-routing', %w[runtime-kernels update -y], machines: machines, ssh_config: '/fixture/ssh.conf')
    expect(result.fetch('exit_status')).to eq(0)
    expect(events(result, 'exec').map { |event| event['argv'] }).to contain_exactly(
      %w[uname -r], ['ssh', '-F', '/fixture/ssh.conf', '-l', 'root', '-p', '2222', 'remote.invalid', 'uname -r']
    )
  end

  %w[switch boot test dry-activate].each do |action|
    it "refreshes before build for #{action}, using only managed non-carried OS entries" do
      result = run_site("site-deploy-#{action}", ['deploy', '-y', '*', action])
      expect(result.fetch('exit_status')).to eq(0)
      expect(events(result, 'exec').map { |event| event['name'] }).to contain_exactly('nodes/a', 'nodes/off')
      expect(JSON.parse(events(result, 'build').first.fetch('kernels')).keys).to eq(%w[nodes/a nodes/off])
      trace = result.fetch('observation').fetch('events').map { |event| event['event'] }
      expect(trace.index('exec.complete')).to be < trace.index('build')
      expect(trace.index('build')).to be < trace.index('skip-current')
    end
  end

  it 'refreshes before selected-generation lookup, copy-only and already-current filtering' do
    result = run_site('site-deploy-generation', %w[deploy -y -g current --copy-only --one-by-one], already_current: true)
    expect(result.fetch('exit_status')).to eq(0)
    expect(events(result, 'build')).to eq([])
    expect(JSON.parse(events(result, 'generation.lookup').first.fetch('kernels')).keys).to eq(%w[nodes/a nodes/off])
    expect(events(result, 'deploy.bulk')).to eq([])
  end

  it 'does not access corrupt state when the deploy hook subset is empty' do
    result = run_site('site-deploy-empty-subset', %w[deploy -y], machines: [inventory[2], inventory[3]], files: { 'configs/node/kernels.json' => '{bad' })
    expect(result.fetch('exit_status')).to eq(0)
    expect(events(result, 'exec')).to eq([])
    expect(result.fetch('stdout')).not_to include('Updating runtime')
    expect(file(result, 'configs/node/kernels.json')).to eq('{bad')
  end

  it 'does not invoke the prepare hook on standalone build' do
    result = run_site('site-build-no-hook', %w[build -y], files: { 'configs/node/kernels.json' => '{bad' })
    expect(result.fetch('exit_status')).to eq(0)
    expect(events(result, 'exec')).to eq([])
  end

  it 'does not invoke the hook for invalid deployment action' do
    result = run_site('site-invalid-action', %w[deploy -y nodes/a invalid])
    expect(result.fetch('exit_status')).to eq(64)
    expect(events(result, 'exec')).to eq([])
  end

  %w[rediscover add rename].each do |operation|
    it "refreshes netboot after core list publication through #{operation}" do
      files = { 'cluster/existing/config.nix' => '{}', 'cluster/existing/module.nix' => '{}' }
      argv = { 'rediscover' => ['rediscover'], 'add' => %w[add new], 'rename' => %w[rename existing renamed] }.fetch(operation)
      result = run_site("site-netboot-#{operation}", argv, files: files)
      expect(result.fetch('exit_status')).to eq(0)
      core_list = file(result, 'cluster/cluster.nix')
      expect(events(result, 'inventory').last.fetch('core_list')).to eq(core_list)
      expect(core_list).to include('./new/module.nix') if operation == 'add'
      expect(core_list).to include('./renamed/module.nix') if operation == 'rename'
      expect(file(result, 'cluster/netbootable.nix')).to eq("# This file is auto-generated on confctl rediscover, changes will be lost\n[\n  \"nodes/a\"\n  \"nodes/b\"\n  \"nixos/live\"\n]\n")
    end
  end

  it 'writes an empty netboot list and preserves prior core writes on hook failure' do
    empty = run_site('site-netboot-empty', ['rediscover'], machines: [])
    expect(empty.fetch('exit_status')).to eq(0)
    expect(file(empty, 'cluster/netbootable.nix')).to end_with("[\n]\n")
    failed = run_site('site-netboot-failure', ['rediscover'], inventory_error: true, files: { 'cluster/netbootable.nix' => 'old list' })
    expect(failed.fetch('exit_status')).to eq(1)
    expect(file(failed, 'cluster/cluster.nix')).to include('generated by confctl')
    expect(file(failed, 'cluster/netbootable.nix')).to eq('old list')
  end
end
