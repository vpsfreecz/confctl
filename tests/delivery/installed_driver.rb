# Evaluated by the existing RSpec VM runner. Every filesystem/process operation
# below is sent to its single guest; there is deliberately no host fallback.
require 'base64'
require 'json'
require 'shellwords'

# A small transport around the runner, not another capture/normalization layer.
def delivery_guest(source, timeout: 40)
  prefix = <<~RUBY_GUEST
    require 'json'
    require 'fileutils'
    require 'open3'
    require 'timeout'
    require 'base64'
    B = JSON.parse(Base64.strict_decode64(#{Base64.strict_encode64(JSON.generate(DELIVERY_BINDINGS)).inspect}))
    ROOT = B.fetch('root')
    abort 'not the disposable fixture VM' unless File.read('/etc/hostname').strip == 'delivery-fixture'
  RUBY_GUEST
  encoded = Base64.strict_encode64(prefix + source)
  command = [DELIVERY_BINDINGS.fetch('ruby'), '-rbase64', '-e', 'eval(Base64.strict_decode64(ARGV.fetch(0)), TOPLEVEL_BINDING, "installed-delivery-guest")', encoded]
  _, output = @delivery_machine.succeeds(Shellwords.join(command), timeout: timeout)
  JSON.parse(output)
end

before(:suite) do
  @delivery_machine = fixture
  @delivery_machine.start
  delivery_guest(<<~RUBY_GUEST)
    %w[carrier rollback assets generations tmp].each { |name| FileUtils.mkdir_p(File.join(ROOT, name)) }
    puts JSON.generate('guest' => File.read('/etc/hostname').strip, 'bindings' => B)
  RUBY_GUEST
end

describe 'installed helpers with explicit fixture bindings', order: :defined do
  it 'keeps installed program names, flat rollback output and kexec manual' do
    actual = delivery_guest(<<~'RUBY_GUEST')
      %w[carrier rollback builder kexec].each { |name| abort "missing installed #{name}" unless File.file?(B.fetch(name)) && File.executable?(B.fetch(name)) }
      abort 'rollback output became a directory' if File.directory?(B.fetch('rollback'))
      manual = File.expand_path('../share/man/man8/kexec-netboot.8.gz', File.dirname(B.fetch('kexec')))
      abort 'real manual producer output missing' unless File.size?(manual)
      # The builder ignores argv: no --help/version smoke execution here.
      puts JSON.generate('names' => %w[carrier-env auto-rollback build-netboot-server kexec-netboot], 'manual' => true)
    RUBY_GUEST
    expect(actual.fetch('manual')).to be(true)
  end

  it 'sets/deletes a profile before callbacks and keeps a successful effect on callback failure' do
    actual = delivery_guest(<<~RUBY_GUEST)
      root = File.join(ROOT, 'carrier')
      plan, trace = File.join(root, 'plan'), File.join(root, 'trace')
      [plan, trace].each { |path| FileUtils.mkdir_p(path) }
      profile = File.join(root, 'profile')
      rules = [
        { 'argv' => ['-p', profile, '--set', 'generation one'], 'writes' => { 'profile' => { 'text' => 'set one' } } },
        { 'argv' => ['-p', profile, '--delete-generations', '1 2'], 'writes' => { 'profile' => { 'text' => 'deleted 1 2' } } },
        { 'argv' => ['-p', profile, '--set', 'generation two'], 'writes' => { 'profile' => { 'text' => 'set two' } } }
      ].each_with_index.map { |rule, i| { 'index' => i, 'rule' => rule.merge('tool' => 'nix-env') } }
      File.write(File.join(plan, 'nix-env.json'), JSON.generate(rules))
      env = { 'PATH' => B.fetch('tools'), 'COMPAT_PLAN' => plan, 'COMPAT_TRACE' => trace, 'COMPAT_ROOT' => root }
      statuses = []
      statuses << Open3.capture3(env, B.fetch('carrier'), '-p', profile, '--set', 'generation one')[2].exitstatus
      statuses << Open3.capture3(env, B.fetch('carrier'), '-p', profile, '--delete-generations', '1 2')[2].exitstatus
      File.write(File.join(root, 'callback-fail'), 'yes')
      statuses << Open3.capture3(env, B.fetch('carrier'), '-p', profile, '--set', 'generation two')[2].exitstatus
      callbacks = File.readlines(File.join(root, 'callback.jsonl')).map { |line| JSON.parse(line).fetch('profile') }
      events = Dir.glob(File.join(trace, '*.json')).map { |path| JSON.parse(File.read(path)) }.sort_by { |e| e.fetch('start_ns') }
      puts JSON.generate('statuses' => statuses, 'callbacks' => callbacks, 'retained' => File.read(profile), 'requests' => events.map { |e| e.fetch('argv') })
    RUBY_GUEST
    expect(actual.fetch('statuses')).to eq([0, 0, 1])
    expect(actual.fetch('callbacks')).to eq(['set one', 'deleted 1 2', 'set two'])
    expect(actual.fetch('retained')).to eq('set two')
    expect(actual.fetch('requests').map { |argv| argv[2] }).to eq(['--set', '--delete-generations', '--set'])
  end

  it 'preserves the switching/switched/confirmed handshake and timeout rollback attempt' do
    actual = delivery_guest(<<~'RUBY_GUEST')
      root = File.join(ROOT, 'rollback')
      check, log = File.join(root, 'check'), File.join(root, 'activation.jsonl')
      original = File.readlink('/run/current-system')
      old, target = File.join(root, 'old'), File.join(root, 'new')
      [old, target].each do |top|
        FileUtils.mkdir_p(File.join(top, 'bin'))
        script = "#!#{B.fetch('ruby')}\nrequire 'json'\nFile.open(#{log.inspect}, 'a') { |f| f.puts(JSON.generate('top' => #{top.inspect}, 'argv' => ARGV, 'check' => File.read(#{check.inspect}))) }\nexit 7\n"
        File.write(File.join(top, 'bin/switch-to-configuration'), script)
        File.chmod(0755, File.join(top, 'bin/switch-to-configuration'))
      end
      File.unlink('/run/current-system')
      File.symlink(old, '/run/current-system')
      confirmed = nil
      begin
        # The unreaped direct child reserves its PID/group until this sole
        # waiter disarms cleanup. Both fixture switch scripts are finite.
        pid = Process.spawn(B.fetch('rollback'), '--timeout', '8', target, 'test', check, pgroup: true, out: File.join(root, 'stdout'), err: File.join(root, 'stderr'))
        wait_rollback = lambda do |seconds|
          deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + seconds
          loop do
            result = Process.waitpid2(pid, Process::WNOHANG)
            if result
              pid = nil
              break result[1].exitstatus
            end
            raise 'rollback fixture child did not exit' if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline

            sleep(0.02)
          end
        end
        begin
          Timeout.timeout(6) { sleep(0.02) until File.exist?(check) && File.read(check) == 'switched' }
          File.write(check, 'confirmed')
          confirmed = wait_rollback.call(6)
        ensure
          if pid
            begin
              Process.kill('KILL', -pid)
            rescue Errno::ESRCH
              nil
            end
            wait_rollback.call(3)
          end
        end
        confirmed_unlinked = !File.exist?(check)
        _out, _err, timeout_status = Open3.capture3(B.fetch('rollback'), '--timeout', '0', target, 'switch', check)
        rows = File.readlines(log).map { |line| JSON.parse(line) }
        puts JSON.generate('confirmed_status' => confirmed, 'confirmed_unlinked' => confirmed_unlinked, 'timeout_status' => timeout_status.exitstatus, 'retained' => File.read(check), 'activations' => rows)
      ensure
        File.unlink('/run/current-system')
        File.symlink(original, '/run/current-system')
      end
    RUBY_GUEST
    expect(actual.values_at('confirmed_status', 'confirmed_unlinked', 'timeout_status', 'retained')).to eq([0, true, 1, 'switched'])
    rows = actual.fetch('activations')
    expect(rows.map { |row| row.fetch('check') }).to eq(%w[switching switching switched])
    expect(rows.map { |row| row.fetch('argv') }).to eq([['test'], ['switch'], ['switch']])
    expect(rows.last.fetch('top')).to end_with('/old')
  end

  it 'builds JSON, links and menu meaning and replaces only generated old roots' do
    actual = delivery_guest(<<~'RUBY_GUEST')
      profiles = '/nix/var/nix/profiles'
      stores = File.join(ROOT, 'generations')
      FileUtils.mkdir_p(profiles)
      [1, 2].each do |id|
        generation, top = File.join(stores, id.to_s), File.join(ROOT, 'assets', "system-#{id}")
        FileUtils.mkdir_p([generation, top])
        File.write(File.join(generation, 'machine.json'), JSON.generate('toplevel' => top, 'spin' => 'nixos', 'fqdn' => 'node.test', 'label' => 'Fixture Node', 'revision' => "revision#{id}", 'kernelParams' => ['console=ttyS0'], 'inputs-info' => { 'os' => { 'rev' => "input#{id}" } }))
        File.symlink('/nix/store/fixture-linux-6.6.52/bzImage', File.join(top, 'kernel'))
        %w[bzImage initrd].each { |name| File.write(File.join(generation, name), "payload:#{id}:#{name}") }
        File.symlink(generation, File.join(profiles, "confctl-delivery-#{id}-link"))
      end
      current = File.join(profiles, 'confctl-delivery')
      File.symlink(File.join(stores, '1'), current)
      marker = File.join(ROOT, 'unrelated-keep')
      FileUtils.mkdir_p(marker)
      File.write(File.join(marker, 'sentinel'), 'keep')
      out, err, status = Open3.capture3(B.fetch('builder'))
      abort "builder failed: #{out} #{err}" unless status.success?
      http, tftp = File.join(ROOT, 'http'), File.join(ROOT, 'tftp')
      old_roots = [File.realpath(http), File.realpath(tftp)]
      index = JSON.parse(File.read(File.join(http, 'machines.json')))
      machine = index.fetch('machines').fetch(0)
      abort 'new index contract' unless machine.fetch('fqdn') == 'node.test' && machine.fetch('generations').all? { |g| g.fetch('generation').is_a?(Integer) && g.fetch('boot_files').values.all? { |url| url.start_with?('http://127.0.0.1:18080/') } }
      first_current = machine.fetch('generations').find { |g| g.fetch('current') }.fetch('generation')
      File.unlink(current)
      File.symlink(File.join(stores, '2'), current)
      out, err, status = Open3.capture3(B.fetch('builder'))
      abort "second builder failed: #{out} #{err}" unless status.success?
      final = JSON.parse(File.read(File.join(http, 'machines.json'))).fetch('machines').fetch(0)
      params = final.fetch('generations').find { |g| g.fetch('current') }.fetch('kernel_params')
      menu = File.read(File.join(tftp, 'pxeserver/machines/node.test/auto.cfg'))
      puts JSON.generate('first_current' => first_current, 'final_current' => final.fetch('generations').find { |g| g.fetch('current') }.fetch('generation'), 'params' => params, 'menu_has_payload' => menu.include?('boot/node.test/2/bzImage') && menu.include?('boot/node.test/2/initrd') && menu.include?('console=ttyS0'), 'current_links' => [File.readlink(File.join(http, 'node.test/current')), File.readlink(File.join(tftp, 'boot/node.test/current'))], 'payload' => File.read(File.join(http, 'node.test/2/bzImage')), 'inputs_info' => final.fetch('generations').find { |g| g.fetch('current') }.fetch('inputs_info'), 'old_roots_removed' => old_roots.none? { |path| File.exist?(path) }, 'outside_untouched' => File.read(File.join(marker, 'sentinel')))
    RUBY_GUEST
    expect(actual.values_at('first_current', 'final_current', 'menu_has_payload', 'old_roots_removed', 'outside_untouched')).to eq([1, 2, true, true, 'keep'])
    expect(actual.fetch('payload')).to eq('payload:2:bzImage')
    expected_params = [
      'httproot=http://127.0.0.1:18080/node.test/2/initrd',
      "init=#{DELIVERY_BINDINGS.fetch('root')}/assets/system-2/init",
      'console=ttyS0'
    ]
    expect(actual.fetch('params')).to eq(expected_params)
    expect(actual.fetch('current_links')).to eq(%w[2 2])
    expect(actual.fetch('inputs_info')).to eq('os' => { 'rev' => 'input2' })
  end

  it 'loads downloaded payload through fake kexec, declines, and cleans a failed partial download' do
    @delivery_machine.succeeds('systemctl start delivery-http.service')
    # Type=simple service startup precedes the Python listener binding.
    probe = [DELIVERY_BINDINGS.fetch('ruby'), '-rsocket', '-e', "Socket.tcp('127.0.0.1', 18080, connect_timeout: 1) { |socket| socket.close }"]
    @delivery_machine.wait_until_succeeds(Shellwords.join(probe), timeout: 10)
    actual = delivery_guest(<<~'RUBY_GUEST')
      # Reuse the old netboot index as input; only loopback transport locations
      # are rebound. The generation IDs, current flags and payload fields stay.
      old = JSON.parse(File.read(B.fetch('originalIndex')))
      node = old.fetch('machines').find { |m| m.fetch('fqdn') == 'node.test' }
      node.fetch('generations').each do |g|
        g['boot_files'] = %w[bzImage initrd].to_h { |name| [name, "http://127.0.0.1:18080/node.test/#{g.fetch('generation')}/#{name}"] }
        FileUtils.mkdir_p(File.join(ROOT, 'http/node.test', g.fetch('generation').to_s))
        g.fetch('boot_files').each_key { |name| File.write(File.join(ROOT, 'http/node.test', g.fetch('generation').to_s, name), "old-payload:#{g.fetch('generation')}:#{name}") }
      end
      File.write(File.join(ROOT, 'http/machines.json'), JSON.generate('machines' => [node]))
      env = { 'TMPDIR' => File.join(ROOT, 'tmp') }
      argv = [B.fetch('kexec'), '--server-url', 'http://127.0.0.1:18080', '--machine', 'node.test', '--generation', '6', '--append', 'literal option=value with spaces']
      out, err, status = Open3.capture3(env, *argv)
      abort "kexec fixture load failed: #{out} #{err}" unless status.success?
      loaded = JSON.parse(File.readlines(File.join(ROOT, 'kexec.jsonl')).last)
      # Explicit machine/generation and empty variants bypass earlier prompts; n refuses
      # only the final confirmation, before any temp/download/load effects.
      _out, _err, declined = Open3.capture3(env, *argv, '--interactive', stdin_data: "n\n")
      count_after_decline = File.readlines(File.join(ROOT, 'kexec.jsonl')).length
      File.unlink(File.join(ROOT, 'http/node.test/6/initrd'))
      _out, _err, partial = Open3.capture3(env, *argv)
      puts JSON.generate('loaded' => loaded, 'declined' => declined.exitstatus, 'loads_after_decline' => count_after_decline, 'partial' => partial.exitstatus, 'remaining_temp' => Dir.children(File.join(ROOT, 'tmp')), 'loads_after_partial' => File.readlines(File.join(ROOT, 'kexec.jsonl')).length)
    RUBY_GUEST
    expect(actual.fetch('loaded').values_at('kernel', 'initrd')).to eq(['old-payload:6:bzImage', 'old-payload:6:initrd'])
    append = actual.fetch('loaded').fetch('argv').find { |arg| arg.start_with?('--append=') }
    expect(append).to eq(
      '--append=httproot=http://netboot.test/node.test/6/initrd ' \
      'init=/fixture/store/system-6/init console=ttyS0 literal option=value with spaces'
    )
    expect(actual.values_at('declined', 'loads_after_decline', 'partial', 'remaining_temp', 'loads_after_partial')).to eq([1, 1, 1, [], 1])
  end
end
