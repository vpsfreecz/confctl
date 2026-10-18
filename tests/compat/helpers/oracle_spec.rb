# frozen_string_literal: true

require_relative '../site/support'

RSpec.describe 'Ruby remote helper behavior oracle' do
  def helper(name, id, argv = [], input: '', **options)
    CompatibilityOracle.run('helpers', { 'helper' => name, 'id' => id, 'argv' => argv }.merge(options.transform_keys(&:to_s)), input: input)
  end

  def events(result, name)
    result.fetch('observation').fetch('events').select { |event| event.fetch('event') == name }
  end

  def generation(number, current: false)
    {
      'generation' => number, 'current' => current, 'time_s' => '2023-11-14 22:13:20',
      'shortrev' => '012345678', 'kernel_version' => '6.12.3', 'kernel_params' => ['console=ttyS0'],
      'boot_files' => { 'bzImage' => "http://boot.invalid/node/#{number}/bzImage", 'initrd' => "http://boot.invalid/node/#{number}/initrd" },
      'variants' => [{ 'name' => 'default', 'label' => 'Default', 'kernel_params' => ['runlevel=default'] },
                     { 'name' => 'rescue', 'label' => 'Rescue', 'kernel_params' => ['runlevel=rescue'] }]
    }
  end

  def machines
    [{ 'fqdn' => 'node.invalid', 'spin' => 'vpsadminos', 'generations' => [generation(9), generation(8, current: true), generation(6)] }]
  end

  def tree_file(result, path)
    root = result.fetch('observation').fetch('files').fetch('http').fetch('symlink')
    result.fetch('observation').fetch('files').fetch("#{root}/#{path}").fetch('text')
  end

  def profiles
    [{ 'name' => 'node', 'generation' => 9, 'current' => true }, { 'name' => 'node', 'generation' => 7 },
     { 'name' => 'nixos', 'generation' => 3, 'current' => true, 'spin' => 'nixos', 'boot_files' => %w[bzImage initrd],
       'metadata' => { 'macs' => ['02:00:00:00:00:02'] } }]
  end

  it 'verifies immutable provenance for all four original helper sources' do
    manifest = CompatibilityOracle.verify_sources(__dir__)
    expect(manifest.fetch('sources').length).to eq(4)
    expect(manifest.fetch('revision')).to eq('cc40679d267165aecfa569128438bb55fe910268')
  end

  it 'keeps auto-rollback help and argument validation on their original streams' do
    help = helper('auto-rollback', 'rollback-help', ['--help'])
    expect(help.fetch('exit_status')).to eq(0)
    expect(help.fetch('stdout')).to include('Deploying with auto-rollback', '--timeout', '--help')
    expect(help.fetch('application_stderr')).to eq('')
    missing = helper('auto-rollback', 'rollback-missing-args')
    expect(missing.fetch('exit_status')).to eq(1)
    expect(missing.fetch('stderr')).to include('Invalid arguments', 'Usage:')
    expect(events(missing, 'spawn')).to eq([])
  end

  it 'writes switching then switched and removes a confirmed check file' do
    result = helper('auto-rollback', 'rollback-confirmed', ['-t', '60', '@ROOT@/new-system', 'switch', '@ROOT@/check'], confirm: true)
    expect(result.fetch('exit_status')).to eq(0)
    expect(events(result, 'check.write').map { |event| event['value'] }).to eq(%w[switching switched confirmed])
    expect(events(result, 'spawn').map { |event| event['argv'] }).to eq([["#{result.fetch('root')}/new-system/bin/switch-to-configuration", 'switch']])
    expect(result.fetch('stdout')).to include('Configuration confirmed')
    expect(result.fetch('observation').fetch('check')).to be_nil
  end

  it 'rolls back to the captured old system with the same action on timeout' do
    result = helper('auto-rollback', 'rollback-timeout', ['--timeout', '0', '@ROOT@/new-system', 'boot', '@ROOT@/check'])
    expect(result.fetch('exit_status')).to eq(1)
    expect(events(result, 'spawn').map { |event| event['argv'].last }).to eq(%w[boot boot])
    expect(events(result, 'spawn').last.fetch('argv').first).to end_with('/old-system/bin/switch-to-configuration')
    expect(result.fetch('observation').fetch('check')).to eq('switched')
    expect(result.fetch('stdout')).to include('Timeout occurred, rolling back', 'Rollback complete')
  end

  {
    'none' => [[], 'No action specified'],
    'profile' => [['--set', '/nix/store/fixture'], 'Profile not set']
  }.each do |label, (argv, message)|
    it "validates carrier-env #{label}" do
      result = helper('carrier-env', "carrier-invalid-#{label}", argv)
      expect(result.fetch('exit_status')).to eq(1)
      expect(result.fetch('stderr')).to include(message)
      expect(events(result, 'system')).to eq([])
    end
  end

  { 'set' => ['--set', '/nix/store/fixture'], 'delete' => ['--delete-generations', '3 4'] }.each do |label, action|
    it "calls nix-env before the carrier callback for #{label}" do
      result = helper('carrier-env', "carrier-#{label}", ['-p', '@ROOT@/profile'] + action)
      expect(result.fetch('exit_status')).to eq(0)
      commands = events(result, 'system').map { |event| event.fetch('argv') }
      expect(commands).to eq([['nix-env', '-p', "#{result.fetch('root')}/profile"] + action, ["#{result.fetch('root')}/on-change"]])
      expect(result.fetch('stdout')).to eq('')
      expect(result.fetch('application_stderr')).to eq('')
    end
  end

  it 'does not run a carrier callback after nix-env failure' do
    result = helper('carrier-env', 'carrier-nix-failure', %w[--profile @ROOT@/profile --set /nix/store/fixture], nix_status: 3)
    expect(result.fetch('exit_status')).to eq(1)
    expect(events(result, 'system').length).to eq(1)
    expect(result.fetch('stderr')).to include('nix-env', 'failed')
  end

  it 'reports callback failure after the profile operation without inventing rollback' do
    result = helper('carrier-env', 'carrier-callback-failure', %w[-p @ROOT@/profile --set /nix/store/fixture], callback_status: 9)
    expect(result.fetch('exit_status')).to eq(1)
    expect(events(result, 'system').length).to eq(2)
    expect(result.fetch('stderr')).to include('on-change failed')
  end

  it 'records the carrier surplus-argument parser NameError as a known baseline defect' do
    result = helper('carrier-env', 'carrier-surplus-args', %w[-p @ROOT@/profile --set /nix/store/fixture surplus])
    expect(result.fetch('exit_status')).to eq(1)
    expect(result.fetch('stderr')).to include('Too many arguments', 'parser')
    expect(result.fetch('observation').fetch('exception')).to eq('NameError')
    expect(events(result, 'system')).to eq([])
  end

  it 'publishes both netboot roots, JSON, variants, metadata and PXE menus' do
    result = helper('build-netboot-server', 'netboot-full-tree', profiles: profiles, old_roots: true, memtest: true, iso: true)
    expect(result.fetch('exit_status')).to eq(0), result.fetch('stderr')
    expect(events(result, 'lock.open').length).to eq(1)
    expect(events(result, 'system').length).to eq(2)
    files = result.fetch('observation').fetch('files')
    expect(files.fetch('http')).to eq('symlink' => 'http.abcdef')
    expect(files.fetch('tftp')).to eq('symlink' => 'tftp.abcdef')
    expect(files.keys.grep(/\.old/)).to eq([])
    index = JSON.parse(tree_file(result, 'machines.json'))
    node = index.fetch('machines').find { |entry| entry.fetch('fqdn') == 'node.invalid' }
    expect(node.fetch('generations').map { |entry| entry['generation'] }).to eq([9, 7])
    current = node.fetch('generations').first
    expect(current.fetch('current')).to be(true)
    expect(current.fetch('kernel_version')).to eq('6.12.3')
    expect(current.fetch('inputs_info')).to eq('vpsadminos' => { 'rev' => 'abcdef' })
    expect(current.fetch('variants').map { |variant| variant['name'] }).to eq(%w[default nopools nostart rescue single])
    expect(current.fetch('kernel_params')).to eq(['httproot=http://boot.fixture.invalid/node.invalid/9/root.squashfs', 'console=ttyS0'])
    expect(files.fetch('tftp.abcdef/pxelinux.cfg/01-02-00-00-00-00-01')).to eq('symlink' => '../pxeserver/machines/node.invalid/auto.cfg')
    expect(files.fetch('tftp.abcdef/pxelinux.cfg/default').fetch('text')).to include('Memtest86', 'ISO images', 'Local Boot')
    expect(files.fetch('tftp.abcdef/pxeserver/machines/node.invalid/auto.cfg').fetch('text')).to include('runlevel=rescue', 'TIMEOUT 50')
    nixos = JSON.parse(tree_file(result, 'nixos.invalid/3/generation.json'))
    expect(nixos.fetch('kernel_params')).to include("init=#{result.fetch('root')}/systems/nixos-3/init")
  end

  it 'honors kernel-params files and a current generation older than the newest' do
    custom = [{ 'name' => 'node', 'generation' => 9 }, { 'name' => 'node', 'generation' => 7, 'current' => true, 'params_file' => "init=keep custom=1 httproot=old\n" }]
    result = helper('build-netboot-server', 'netboot-old-current', profiles: custom)
    expect(result.fetch('exit_status')).to eq(0), result.fetch('stderr')
    node = JSON.parse(tree_file(result, 'machines.json')).fetch('machines').first
    expect(node.fetch('generations').map { |entry| [entry['generation'], entry['current']] }).to eq([[9, false], [7, true]])
    expect(node.fetch('generations').last.fetch('kernel_params')).to eq(['httproot=http://boot.fixture.invalid/node.invalid/7/root.squashfs', 'init=keep', 'custom=1'])
  end

  it 'keeps old roots on corrupt metadata and missing-current failures' do
    [{ 'corrupt' => true, 'current' => true }, {}].each_with_index do |extra, index|
      result = helper('build-netboot-server', "netboot-invalid-#{index}", profiles: [{ 'name' => 'node', 'generation' => 1 }.merge(extra)], old_roots: true)
      expect(result.fetch('exit_status')).to eq(1)
      expect(result.fetch('observation').fetch('files').fetch('http')).to eq('symlink' => 'http.old')
      expect(events(result, 'system')).to eq([])
    end
  end

  it 'preserves old published roots when the publication command fails' do
    result = helper('build-netboot-server', 'netboot-publish-failure', profiles: profiles, old_roots: true, publish_failure: true)
    expect(result.fetch('exit_status')).to eq(1)
    expect(result.fetch('observation').fetch('files').fetch('http')).to eq('symlink' => 'http.old')
    expect(result.fetch('observation').fetch('files').fetch('tftp')).to eq('symlink' => 'tftp.old')
    expect(result.fetch('stderr')).to include('Failed to install new root')
  end

  %w[exec unload].each do |action|
    it "handles kexec --#{action} without HTTP and retains --no-sync" do
      result = helper('kexec-netboot', "kexec-#{action}", ["--#{action}", '--no-sync'])
      expect(result.fetch('exit_status')).to eq(0)
      expect(events(result, 'http')).to eq([])
      expect(events(result, 'system').first.fetch('argv').first).to end_with("--#{action} --no-sync")
    end
  end

  it 'rejects kexec unload/exec together before side effects' do
    result = helper('kexec-netboot', 'kexec-conflict', %w[-e -u])
    expect(result.fetch('exit_status')).to eq(1)
    expect(result.fetch('stderr')).to include('use either --unload or --exec')
    expect(events(result, 'system')).to eq([])
  end

  it 'requires httproot even with an explicit server URL' do
    result = helper('kexec-netboot', 'kexec-missing-httproot', %w[-s http://override.invalid], cmdline: 'console=ttyS0')
    expect(result.fetch('exit_status')).to eq(1)
    expect(result.fetch('stderr')).to include("Could not find 'httproot='")
    expect(events(result, 'http')).to eq([])
  end

  it 'derives the HTTP index, selects current generation, appends parameters and cleans downloads' do
    result = helper('kexec-netboot', 'kexec-load-current', %w[-a debug=1], machines: machines)
    expect(result.fetch('exit_status')).to eq(0), result.fetch('stderr')
    expect(events(result, 'http').first.fetch('url')).to eq('http://boot.fixture.invalid/machines.json')
    expect(events(result, 'http').map { |event| event['url'] }).to include('http://boot.invalid/node/8/bzImage')
    command = events(result, 'system').first.fetch('argv').first
    expect(command).to include('--load', '--append="console=ttyS0 runlevel=default debug=1"', '--no-sync')
    expect(result.fetch('observation').fetch('temporary_files')).to eq([])
  end

  { '-1' => 8, '-2' => 6, '9' => 9 }.each do |selector, selected|
    it "keeps kexec generation selector #{selector} semantics" do
      result = helper('kexec-netboot', "kexec-generation-#{selector.tr('-', 'm')}", ['--generation', selector], machines: machines)
      expect(result.fetch('exit_status')).to eq(0)
      expect(events(result, 'http').map { |event| event['url'] }).to include("http://boot.invalid/node/#{selected}/bzImage")
    end
  end

  it 'fails a missing generation without downloading or loading' do
    result = helper('kexec-netboot', 'kexec-generation-missing', %w[-g -3], machines: machines)
    expect(result.fetch('exit_status')).to eq(1)
    expect(result.fetch('stderr')).to include('not found (or invalid)', 'No generation found/selected')
    expect(events(result, 'system')).to eq([])
    expect(events(result, 'http').length).to eq(1)
  end

  it 'warns about a missing requested variant but still loads without one' do
    result = helper('kexec-netboot', 'kexec-variant-missing', %w[-v unknown -m node.invalid -s http://override.invalid], machines: machines)
    expect(result.fetch('exit_status')).to eq(0)
    expect(result.fetch('stderr')).to include("Requested variant 'unknown' not found")
    expect(events(result, 'http').first.fetch('url')).to eq('http://override.invalid/machines.json')
    expect(events(result, 'system').first.fetch('argv').first).to include('--append="console=ttyS0 "')
  end

  it 'uses interactive default machine/current generation/default variant and explicit y' do
    result = helper('kexec-netboot', 'kexec-interactive', ['-i'], machines: machines, input: "\n\n\ny\n")
    expect(result.fetch('exit_status')).to eq(0), result.fetch('stderr')
    expect(result.fetch('stdout')).to include('Select a machine', 'Available generations', 'Choose variant', 'Continue?')
    expect(events(result, 'system').length).to eq(1)
  end

  it 'does not download or load after interactive decline' do
    result = helper('kexec-netboot', 'kexec-decline', %w[-i -m node.invalid -g 8 -v rescue], machines: machines, input: "n\n")
    expect(result.fetch('exit_status')).to eq(1)
    expect(result.fetch('stderr')).to include('Aborting')
    expect(events(result, 'http').length).to eq(1)
    expect(events(result, 'system')).to eq([])
  end

  it 'records HTTP, JSON and kexec command failures separately' do
    http = helper('kexec-netboot', 'kexec-http-failure', machines: machines, http_status: 404)
    expect(http.fetch('exit_status')).to eq(1)
    expect(http.fetch('stderr')).to include('HTTP 404')
    json = helper('kexec-netboot', 'kexec-json-failure', bad_json: true)
    expect(json.fetch('exit_status')).to eq(1)
    expect(json.fetch('stderr')).to include('Could not parse JSON')
    command = helper('kexec-netboot', 'kexec-command-failure', ['-e'], kexec_status: 7)
    expect(command.fetch('exit_status')).to eq(1)
    expect(command.fetch('stderr')).to include('kexec --exec failed')
  end
end
