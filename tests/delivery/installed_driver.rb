# Evaluated by the existing RSpec VM runner. Every filesystem/process operation
# below is sent to its single guest; there is deliberately no host fallback.
require 'base64'
require 'json'
require 'shellwords'

# These finite helpers are evaluated only in the guest, never by the host
# runner. Files carry ordinary output; PTY output is drained by the sole waiter,
# so no background capture/wait thread can outlive an example's failure path.
DELIVERY_CLI_SUPPORT = <<~'RUBY_GUEST'.freeze
  require 'digest'
  require 'pty'
  def fixture_case(name)
    root = File.join(ROOT, 'cli', name)
    FileUtils.mkdir_p(File.dirname(root))
    Dir.mkdir(root, 0700)
    %w[home tmp plan trace].each { |part| Dir.mkdir(File.join(root, part), 0700) }
    File.write(File.join(root, 'flake.nix'), "{ outputs = _: {}; }\n")
    root
  end

  def fixture_env(root)
    { 'HOME' => File.join(root, 'home'), 'TMPDIR' => File.join(root, 'tmp'),
      'XDG_CACHE_HOME' => File.join(root, 'home/cache'), 'PATH' => "#{File.dirname(B.fetch('git'))}:#{B.fetch('coreutils')}:#{File.dirname(B.fetch('bash'))}:#{B.fetch('tools')}",
      'LANG' => 'C.UTF-8', 'TZ' => 'UTC', 'NO_COLOR' => '1', 'PAGER' => '',
      'USER' => 'root', 'LOGNAME' => 'root',
      'GIT_CONFIG_NOSYSTEM' => '1', 'GIT_CONFIG_GLOBAL' => '/dev/null',
      'GIT_TERMINAL_PROMPT' => '0', 'GIT_AUTHOR_NAME' => 'Delivery Fixture',
      'GIT_AUTHOR_EMAIL' => 'fixture@example.invalid', 'GIT_COMMITTER_NAME' => 'Delivery Fixture',
      'GIT_COMMITTER_EMAIL' => 'fixture@example.invalid', 'GIT_EDITOR' => 'false',
      'CONFCTL_SSH_CONFIG' => '', 'COMPAT_ROOT' => root,
      'COMPAT_PLAN' => File.join(root, 'plan'), 'COMPAT_TRACE' => File.join(root, 'trace') }
  end

  def fixture_stat(pid)
    text = File.read("/proc/#{pid}/stat")
    fields = text[(text.rindex(')') + 2)..].split
    { pid: pid, state: fields[0], session: Integer(fields[3]), start: Integer(fields[19]) }
  rescue Errno::ENOENT, Errno::ESRCH
    nil
  end

  def fixture_start(root, env, argv, tty: false, tty_stderr: false, cwd: root)
    raise ArgumentError, 'tty_stderr requires tty' if tty_stderr && !tty
    stdout, stderr = File.join(root, "stdout-#{Dir.glob(root + '/stdout-*').length}"), File.join(root, "stderr-#{Dir.glob(root + '/stderr-*').length}")
    File.write(stderr, '') if tty_stderr # This mode reports merged PTY output.
    reader, writer = tty ? PTY.open : IO.pipe
    pid = fork do
      Process.setsid
      if tty
        writer.ioctl(0x540e, 0) # TIOCSCTTY, this child's fresh session only.
        writer.ioctl(0x5410, [Process.pid].pack('i')) # TIOCSPGRP.
      end
      exec(env, *argv, chdir: cwd, in: tty ? writer : reader,
           out: tty ? writer : stdout, err: tty_stderr ? writer : stderr, unsetenv_others: true)
    end
    tty ? writer.close : reader.close
    child = { pid: pid, writer: tty ? reader : writer, master: tty ? reader : nil,
              stdout: stdout, stderr: stderr, output: +'', status: nil }
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 3
    loop do
      identity = fixture_stat(pid)
      if identity && identity[:session] == pid
        child[:identity] = identity
        break
      end
      raise 'fixture child did not establish its owned session' if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
      sleep(0.01)
    end
    child
  rescue StandardError
    # Before reaping, this exact direct child's PID cannot have been reused.
    Process.kill('KILL', pid) if pid
    Process.waitpid(pid) if pid
    reader.close unless reader.nil? || reader.closed?
    writer.close unless writer.nil? || writer.closed?
    raise
  end

  def fixture_output(child)
    if child[:master] && !child[:master].closed?
      loop do
        data = child[:master].read_nonblock(16384, exception: false)
        break if data == :wait_readable || data.nil?
        child[:output] << data
      end
    end
    child[:master] ? child[:output] : (File.exist?(child[:stdout]) ? File.read(child[:stdout]) : '')
  rescue Errno::EIO # PTY slave closed; output already drained remains available.
    child[:output]
  end

  def fixture_until(child, seconds)
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + seconds
    loop do
      fixture_output(child)
      return if yield
      raise 'fixture deadline (normal completion failed)' if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
      sleep(0.01)
    end
  end

  def fixture_wait(child, seconds: 8, verify_members: true)
    fixture_until(child, seconds) do
      if (result = Process.waitpid2(child.fetch(:pid), Process::WNOHANG))
        child[:status] = result[1]
      end
      child[:status]
    end
    fixture_output(child)
    if verify_members
      live = fixture_members(child)
      raise "CLI left owned session members: #{live.inspect}" unless live.empty?
    end
    { 'exit' => child[:status].exitstatus, 'signal' => child[:status].termsig,
      'out' => fixture_output(child), 'err' => File.read(child[:stderr]) }
  end

  def fixture_members(child)
    Dir.glob('/proc/[0-9]*/stat').filter_map { |path| fixture_stat(Integer(path.split('/')[2])) }
       .select { |row| row[:session] == child[:pid] && row[:state] != 'Z' }
  end

  def fixture_cleanup(child)
    return unless child
    # Finite fixtures never create another session. If its numeric leader is
    # reused, refuse the session scan; a repeated scan alone proves no ownership.
    leader = fixture_stat(child[:pid])
    original = child.fetch(:identity)
    if leader && (leader[:start] != original[:start] || leader[:session] != original[:session])
      raise 'cleanup refused reused fixture session leader'
    end
    # A gone leader leaves the kernel session identity retained by its children.
    fixture_members(child).each do |row|
      current = fixture_stat(row[:pid])
      next unless current && current[:start] == row[:start] && current[:session] == row[:session]
      begin
        Process.kill('KILL', row[:pid])
      rescue Errno::ESRCH
        nil
      end
    end
    fixture_wait(child, seconds: 3, verify_members: false) unless child[:status]
    fixture_until(child, 3) { fixture_members(child).empty? }
  ensure
    child[:writer].close if child && !child[:writer].closed?
  end

  def fixture_run(root, argv, input: '', env: {}, executable: B.fetch('operationCLI'), tty: false, tty_stderr: false, cwd: root)
    child = fixture_start(root, fixture_env(root).merge(env), [executable, *argv], tty: tty, tty_stderr: tty_stderr, cwd: cwd)
    child[:writer].write(input) unless input.empty?
    child[:writer].close unless tty
    fixture_wait(child)
  ensure
    fixture_cleanup(child)
  end

  def fixture_ok(result)
    abort "CLI failed: #{result.inspect}" unless result.fetch('exit') == 0 && result.fetch('signal').nil?
    result
  end

  # The real package wrappers select real Nix, independent of fixture PATH.
  # Only this guest-local, uncached derivation waits on the explicit release.
  def fixture_installed_tui_build(label, executable)
    require 'tmpdir'
    require 'time'
    root = fixture_case('tui-' + label)
    gate = Dir.mktmpdir('confctl-delivery-tui-' + label + '-', '/tmp')
    File.chmod(0777, gate) # The guest's Nix build user cannot enter ROOT's 0700 tree.
    release, pidfile = File.join(gate, 'release'), File.join(gate, 'builder-pid')
    token = 'delivery-tui-' + label + '-' + File.basename(gate)
    child = builder = nil
    File.write(root + '/flake.nix', <<~NIX)
      {
        outputs = _: let
          inventory = builtins.derivation {
            name = "delivery-tui-inventory";
            system = #{B.fetch('system').to_json};
            builder = #{B.fetch('bash').to_json};
            args = [ "-eu" "-c" ''printf '%s' "$text" > "$out"'' ];
            text = builtins.toJSON {
              node = { name = "node"; clusterName = "node"; key = "node";
                metaConfig = { managed = true; spin = "nixos"; }; };
            };
            preferLocalBuild = true;
            allowSubstitutes = false;
          };
          top = builtins.derivation {
            name = #{token.to_json};
            system = #{B.fetch('system').to_json};
            builder = #{B.fetch('bash').to_json};
            PATH = #{B.fetch('coreutils').to_json};
            preferLocalBuild = true;
            allowSubstitutes = false;
            args = [ "-eu" "-c" ''
              printf '%s\\n' "$$" > "#{pidfile}"
              printf '%s\\n' '#{token}' >&2
              deadline=$((SECONDS + 60))
              until test -f "#{release}"; do
                if test "$SECONDS" -ge "$deadline"; then exit 97; fi
                sleep 0.05
              done
              mkdir "$out"
              printf '%s' '#{token}' > "$out/tui-token"
            '' ];
          };
        in { confctl = {
          settings = { list.columns = [ "name" ];
            nix = { maxJobs = "1"; impureEval = false; legacyNixPath = false; }; };
          machinesJson = inventory;
          machineKeys.node = "node";
          buildPlan.node = { key = "node"; inputs = {}; };
          inputsInfo.node = {};
          build.node = { toplevel = top; autoRollback = top; };
        }; };
      }
    NIX
    env = fixture_env(root).merge('CONFCTL_TTY' => '1', 'TERM' => 'xterm-256color',
      'NIX_REMOTE' => 'local',
      'NIX_CONFIG' => "experimental-features = nix-command flakes\nsandbox = false\nsubstituters =\nbuilders =\n")
    color = label == 'normal'
    argv = [executable]
    argv << '--color=always' if color # Explicit always overrides this fixture's NO_COLOR=1.
    argv.concat(['build', '--yes', '--max-jobs', '1', 'node'])
    child = fixture_start(root, env, argv, tty: true, tty_stderr: true)
    child[:master].ioctl(0x5414, [32, 160, 0, 0].pack('S4')) # This owned PTY only.
    held = nil
    prelude_lines = nil
    frame_end = "\e[32;1H\e[2K"
    separator = " \u2014 ".b
    full_log = nil
    header_command = 'Command: confctl ' + (color ? '--color\=always ' : '') + 'build --yes --max-jobs 1 node'
    duration_ms = lambda do |value, live: false|
      parts = value.match(/\A(?:(\d+)h )?(?:(\d+)m )?(\d+)(?:\.(\d{3}))?s\z/)
      abort 'invalid timing duration' unless parts && (live ? parts[4].nil? : !parts[4].nil?)
      hours, minutes, seconds = parts.captures.first(3).map { |part| part.to_i }
      prefix = hours > 0 ? "#{hours}h #{minutes}m " : (minutes > 0 ? "#{minutes}m " : '')
      canonical = prefix + (live ? seconds.to_s : format('%d.%03d', seconds, parts[4].to_i)) + 's'
      abort 'noncanonical timing duration' unless value == canonical && minutes < 60 && seconds < 60
      ((hours * 3600 + minutes * 60 + seconds) * 1000) + parts[4].to_i
    end
    timing_header = lambda do |line|
      timing = line.match(/\AStarted: (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2}))  Elapsed: (.+)\z/)
      abort 'missing invocation timing header' unless timing
      Time.iso8601(timing[1])
      [timing[1], duration_ms.call(timing[2], live: true)]
    end
    held_started = held_elapsed = nil
    fixture_until(child, 45) do
      if (done = Process.waitpid2(child[:pid], Process::WNOHANG))
        child[:status] = done[1]
        abort "packaged CLI exited before release: exit=#{done[1].exitstatus} signal=#{done[1].termsig} #{fixture_output(child)}"
      end
      if !builder && File.file?(pidfile) && File.read(pidfile).match?(/\A[0-9]+\n\z/)
        builder = fixture_stat(Integer(File.read(pidfile)))
        if builder
          command = File.binread("/proc/#{builder[:pid]}/cmdline")
          abort 'unexpected builder identity' unless command.include?(token) && command.include?(gate)
        end
      end
      screen = fixture_output(child)
      # Only a complete latest paint can satisfy admission; earlier prelude or
      # partial rows cannot release the real builder. Keep the captured bytes.
      finish = screen.rindex(frame_end)
      start = screen.rindex("\e[H", finish) if finish
      next false unless builder && start
      frame = screen[start...(finish + frame_end.bytesize)]
      rows = frame.delete_prefix("\e[H").delete_suffix(frame_end).split("\e[2K\r", -1)
      next false unless rows.shift == '' && rows.length == 31
      rows.map! { |row| row.delete_suffix("\r\n").delete_suffix("\n") }
      plain = rows.map { |row| row.gsub(/\e\[[0-9;]*m/, '') } # Assertion view only, never transport/log bytes.
      bottom = plain.index { |row| row.match?(/\A<-+\z/) }
      next false unless bottom && plain[5...bottom].any? { |row| row.include?(token) } &&
        plain[(bottom + 1)..].any? { |row| row.match?(/\ABuilding +\[running[^\]]*\] +1\/1\b/) }
      held_started, held_elapsed = timing_header.call(plain[1])
      abort 'wrong compact live header' unless plain[0] == header_command && plain[2] == 'Build group: 1/1 (1 machines)' &&
        plain[3].start_with?('Full log: ' + root + '/.confctl/logs/') && plain[4].match?(/\ALive view -+>\z/)
      bars = plain[(bottom + 1)..].reject(&:empty?)
      abort 'log border/bar allocation changed' unless bottom - 5 >= 12 && bars.length == 2 &&
        bars[0].match?(/\ABuilding +\[running[^\]]*\] +1\/1 \(100%\) +\[[= ]{4,20}\]/) &&
        bars[1].match?(/\AFetching +\[pending\]\z/)
      # Fetching has no observations in this real local build: no invented
      # counter/bar. Main PTYs cover two actual positive-total tracks.
      log_path = plain[3].delete_prefix('Full log: ')
      full_log = plain[3] if File.file?(log_path)
      if full_log
        path = File.binread(log_path).scan(%r{/nix/store/[0-9abcdfghijklmnpqrsvwxyz]{32}-[^\s'"]+}).find do |candidate|
          candidate.include?(token) && candidate.end_with?('.drv')
        end
        if path && (divider = bars[0].index(separator))
          detail = bars[0][(divider + separator.bytesize)..]
          # Prefix/count/track are ASCII here; the separator occupies 3 cells.
          if path.bytesize <= 160 - divider - 3
            abort 'fitting canonical detail was shortened' unless detail == path
          else
            abort 'canonical detail did not shorten only its hash' unless detail.start_with?(path[0, 18] + "\u2026-".b)
          end
          abort 'child log hash was shortened' unless plain[5...bottom].any? { |row| row.include?(path[0, 44]) }
        end
      end
      abort 'old observed label survived' if plain.join("\n").match?(/observed (?:starts|step)/i)
      abort 'child log was styled' if rows[5...bottom].any? { |row| row.match?(/\e\[[0-9;]*m/) }
      prelude = screen.split("\e[H", 2).first
      prelude_lines = prelude.gsub(/\e\[[0-9;]*m/, '').lines.map(&:strip)
      abort 'prelude missing or repeated before view' unless prelude_lines.count('Building machines') == 1 && prelude_lines.count('node') == 1
      abort 'prelude was not scrolled before first paint' unless prelude.match?(/(?:\r?\n){24,}\z/)
      abort 'prelude repeated in repaint' if plain.any? { |row| ['Building machines', 'node', 'with inputs'].include?(row.strip) }
      if color
        abort 'missing confctl header styling' unless rows[0].start_with?("\e[1mCommand:\e[0m ") &&
          rows[1].include?("\e[1mStarted:\e[0m") && rows[1].include?("\e[1mElapsed:\e[0m") &&
          rows[2].start_with?("\e[1mBuild group:\e[0m ") && rows[3].start_with?("\e[1mFull log:\e[0m ") &&
          rows[4] == "\e[1m#{plain[4]}\e[0m"
        abort 'missing confctl prelude styling' unless prelude.include?("\e[1mBuilding machines\e[0m") && prelude.include?("\e[33mnode\e[0m")
      else
        abort 'NO_COLOR frame/prelude has SGR' if (frame + prelude).match?(/\e\[[0-9;]*m/)
      end
      cli_now, builder_now = fixture_stat(child[:pid]), fixture_stat(builder[:pid])
      abort 'release preceded live observation' if File.exist?(release)
      abort 'CLI not alive at frame' unless cli_now && cli_now[:start] == child[:identity][:start] && cli_now[:state] != 'Z'
      abort 'builder not alive at frame' unless builder_now && builder_now[:start] == builder[:start] && builder_now[:state] != 'Z'
      abort 'generation completed before release' if File.exist?(root + '/.confctl/generations/node/current')
      held = { 'release_absent' => true, 'cli_alive' => true, 'builder_alive' => true,
               'builder_pid' => builder[:pid], 'builder_start' => builder[:start], 'frame_base64' => Base64.strict_encode64(screen) }
      true
    end
    File.write(release, "release\n")
    result = fixture_ok(fixture_wait(child, seconds: 45))
    generations = root + '/.confctl/generations/node'
    current = File.realpath(generations + '/current')
    record = JSON.parse(File.read(current + '/generation.json'))
    abort 'wrong built generation' unless record.fetch('mode') == 'flakes' &&
      File.read(record.fetch('toplevel') + '/tui-token') == token &&
      result.fetch('out').include?('Built generations:')
    final_lines = result.fetch('out').gsub(/\e\[[0-9;]*m/, '').lines.map(&:strip)
    abort 'prelude repeated during final summary' unless final_lines.count('Building machines') == 1 && final_lines.count('node') == 1 &&
      final_lines.count('with inputs') == prelude_lines.count('with inputs')
    output = result.fetch('out')
    finish = output.rindex(frame_end)
    start = output.rindex("\e[H", finish) if finish
    abort 'missing final complete paint' unless start
    final_frame = output[start...(finish + frame_end.bytesize)]
    final_rows = final_frame.delete_prefix("\e[H").delete_suffix(frame_end).split("\e[2K\r", -1)
    abort 'partial final paint' unless final_rows.shift == '' && final_rows.length == 31
    final_rows.map! { |row| row.delete_suffix("\r\n").delete_suffix("\n") }
    final_plain = final_rows.map { |row| row.gsub(/\e\[[0-9;]*m/, '') }
    bottom = final_plain.index { |row| row.match?(/\A<-+\z/) }
    abort 'final panel lost compact context or log border' unless bottom && bottom - 5 >= 12 &&
      final_plain[0] == header_command && final_plain[2] == 'Build group: 1/1 (1 machines)' &&
      final_plain[3].start_with?('Full log: ' + root + '/.confctl/logs/')
    final_started, final_elapsed = timing_header.call(final_plain[1])
    abort 'invocation timing reset between paints' unless final_started == held_started && final_elapsed >= held_elapsed
    tasks = final_plain[(bottom + 1)..].reject(&:empty?)
    abort 'final panel lost child outcomes' unless tasks.length == 3 &&
      tasks[0].match?(/\ABuilding +\[succeeded\] +1\/1 \(100%\) +\[[= ]{4,20}\]/) &&
      tasks[1].match?(/\AFetching +\[skipped\]/) && tasks[1].end_with?('No steps observed') &&
      tasks[2].match?(/\Anix eval +\[succeeded\]/)
    # All selected rows use one detail column. Counterless Fetching leaves the
    # shared counter/track space blank instead of claiming work or an empty bar.
    divider = tasks[0].index(separator)
    fetch_divider = tasks[1].index(separator)
    abort 'final detail columns differ' unless divider && fetch_divider == divider
    abort 'unobserved Fetching gained a counter/bar' unless tasks[1][(tasks[1].index(']') + 1)...fetch_divider].strip.empty?
    abort 'final child log was styled' if final_rows[5...bottom].any? { |row| row.match?(/\e\[[0-9;]*m/) }
    raw_suffix = output[(finish + frame_end.bytesize)..]
    suffix = raw_suffix.gsub(/\e\[[0-9;]*m/, '')
    suffix_lines = suffix.lines.map(&:strip)
    abort 'header or selected task replayed after final paint' if suffix.include?("\e[H") ||
      suffix_lines.any? { |line| line.match?(/\A(?:Command:|Build group:|Started:.*  Elapsed:|(?:Building|Fetching|nix build|nix eval) +\[)/) }
    locators = suffix_lines.grep(/\AFull log: /)
    abort 'full log locator replayed' if locators.length > 1 || (locators.any? && final_plain.include?(locators.first))
    abort 'wrong recovered full log locator' if locators.any? && (full_log ? locators.first != full_log :
      !locators.first.start_with?('Full log: ' + root + '/.confctl/logs/'))
    abort 'normal build result missing after final paint' unless suffix_lines.include?('Built generations:')
    # These two PTYs merge original stderr with stdout. Keep its exact bytes,
    # and accept only this invocation's complete terminal footer, never a
    # blanket Timing-line filter. Separate-stream Main tests own stderr policy.
    footer_start = raw_suffix.index("Timing: completed (exit 0)")
    abort 'missing or repeated final timing report' unless footer_start && final_lines.grep(/\ATiming:/).length == 1
    footer = raw_suffix[footer_start..].lines.map { |line| line.delete_suffix("\r\n").delete_suffix("\n") }
    labels = ['Timing: completed (exit 0)', '  Started: ', '  Ended: ', '  Evaluation: ', '  Build: ', '  Prompt wait: ', '  Other: ', '  Total: ']
    abort 'malformed installed build timing footer' unless footer.length == labels.length && footer[0] == labels[0] &&
      labels.drop(1).each_with_index.all? { |label, i| footer[i + 1].start_with?(label) } && raw_suffix.end_with?("\n")
    abort 'timing report was not on a fresh line after normal results' unless footer_start > 0 && raw_suffix[footer_start - 1] == "\n" &&
      suffix_lines.index('Timing: completed (exit 0)') > suffix_lines.index('Built generations:')
    report_started = footer[1].delete_prefix(labels[1])
    report_ended = footer[2].delete_prefix(labels[2])
    abort 'footer invocation start changed' unless report_started == held_started &&
      report_ended.match?(/\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2})\z/) &&
      Time.iso8601(report_ended) >= Time.iso8601(report_started)
    durations = footer.drop(3).each_with_index.map { |line, i| duration_ms.call(line.delete_prefix(labels[i + 3])) }
    abort 'footer timing does not cover final panel and work' unless durations[2] == 0 && durations[4] >= final_elapsed &&
      durations[4] >= durations[0] + durations[1]
    { 'product' => label, 'executable' => executable, 'exit' => result.fetch('exit'),
      'signal' => result.fetch('signal'), 'before_release' => held, 'token' => token,
      'timing' => { 'started' => held_started, 'held_elapsed_ms' => held_elapsed, 'final_elapsed_ms' => final_elapsed,
                    'total_ms' => durations[4], 'footer_count' => 1 },
      'generation' => record, 'output_base64' => Base64.strict_encode64(result.fetch('out')) }
  ensure
    # Release before cancel/join, including assertion failure; never remove the
    # builder's files while its recorded PID/start identity can still be alive.
    begin
      File.write(release, "cleanup\n") if release && File.directory?(gate)
    ensure
      fixture_cleanup(child)
    end
    if !builder && pidfile && File.file?(pidfile) && File.read(pidfile).match?(/\A[0-9]+\n\z/)
      builder = fixture_stat(Integer(File.read(pidfile)))
      if builder
        command = File.binread("/proc/#{builder[:pid]}/cmdline")
        abort 'unexpected cleanup builder identity' unless command.include?(token) && command.include?(gate)
      end
    end
    if builder
      fixture_until(child, 10) do
        now = fixture_stat(builder[:pid])
        now.nil? || now[:start] != builder[:start]
      end
    end
    FileUtils.remove_entry(gate) if gate && File.directory?(gate)
  end

  def fixture_plan(root, rules)
    rules.group_by { |rule| rule.fetch('tool') }.each do |tool, rows|
      File.write(File.join(root, 'plan', tool + '.json'), JSON.generate(rows.each_with_index.map { |row, i| { index: i, rule: row } }))
    end
  end

  def fixture_events(root)
    rows = Dir.glob(File.join(root, 'trace', '*.json')).map { |path| JSON.parse(File.read(path)) }.sort_by { |row| row.fetch('start_ns') }
    abort 'unexpected fixture request' if rows.any? { |row| row.fetch('rule') < 0 }
    rows
  end

  def fixture_machines(root, names = ['node'], previous: false)
    inventory = names.to_h do |name|
      [name, { name: name, clusterName: name, key: name, metaConfig: { managed: true, spin: 'nixos', host: { target: name + '.invalid', fqdn: name + '.test' }, tags: ['fixture'] } }]
    end
    File.write(File.join(root, 'machines.json'), JSON.generate(inventory))
    FileUtils.mkdir_p(File.join(root, 'top'))
    File.symlink('/nix/store/fixture-linux-6.12.3/bzImage', File.join(root, 'top/kernel'))
    # Opt in only when activation/health is intended. Defaults still exercise
    # the current-target skip and copy-only behavior with the original replies.
    current_top = root + (previous ? '/previous' : '/top')
    rules = {
      '.#confctl.settings' => { list: { columns: ['name', 'managed'] }, nix: { maxJobs: '7', impureEval: false, legacyNixPath: false },
                               buildGenerations: { min: 0, max: 0, maxAge: 0, collectGarbage: false }, hostGenerations: { min: 0, max: 0, maxAge: 0, collectGarbage: true } },
      '.#confctl.machinesJson' => [{ outputs: { out: File.join(root, 'machines.json') } }],
      '.#confctl.machineKeys' => names.to_h { |name| [name, name] },
      '.#confctl.buildPlan' => names.to_h { |name| [name, { key: name, inputs: {} }] }
    }.map { |key, value| { 'tool' => 'nix', 'contains' => key, 'stdout' => JSON.generate(value) } }
    names.each do |name|
      rules << { 'tool' => 'nix', 'contains' => '.#confctl.inputsInfo.' + name, 'stdout' => '{"os":"fresh-revision"}' }
      rules << { 'tool' => 'ssh', 'host' => name + '.invalid', 'contains' => 'cat /proc/uptime', 'stdout' => "600 0\n" }
      rules << { 'tool' => 'ssh', 'host' => name + '.invalid', 'contains' => 'realpath /run/current-system', 'stdout' => current_top + "\n" }
      rules << { 'tool' => 'ssh', 'host' => name + '.invalid', 'contains' => 'cat /etc/confctl/inputs-info.json', 'stdout' => '{"os":"fresh-revision"}' }
      rules << { 'tool' => 'ssh', 'host' => name + '.invalid', 'contains' => 'bash --norc', 'read_stdin' => true,
                 'stdout' => "#{current_top}\n/nix/var/nix/profiles/system-1-link;#{root}/old;0\n/nix/var/nix/profiles/system-2-link;#{current_top};1\n" }
    end
    apply = "x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ #{names.map { |name| JSON.generate(name) }.join(' ')} ])"
    rules << { 'tool' => 'nix', 'argv' => ['eval', '--json', '--no-write-lock-file', '--no-update-lock-file', '--option', 'max-jobs', '7', '--apply', apply, '.#confctl.inputsInfo'],
               'stdout' => JSON.generate(names.to_h { |name| [name, { os: 'fresh-revision' }] }) }
    outputs = names.to_h { |name| [name, { toplevel: root + '/top', autoRollback: '/fixture/rollback' }] }
    names.each do |name|
      rules << { 'tool' => 'nix', 'contains' => ".#confctl.build.#{name}.toplevel", 'stdout' => 'null' }
    end
    rules << { 'tool' => 'nix', 'contains' => '.#confctl.build', 'stdout' => JSON.generate(outputs) }
    rules
  end

  def fixture_seed(root, host, name, date = '2026-10-15T10:00:00Z')
    dir = File.join(root, '.confctl/generations', host.tr('/', ':'), name)
    FileUtils.mkdir_p(dir)
    File.write(File.join(dir, 'generation.json'), JSON.generate(mode: 'flakes', toplevel: root + '/top', date: date, inputs: {}, inputs_info: { os: 'stored-revision' }))
    File.symlink(root + '/top', File.join(dir, 'toplevel'))
    dir
  end

  def fixture_peer(root, confirm: false)
    reg = { schema: 1, bound_sources: [], extensions: [{ id: 'fixture.delivery', protocol: { major: 1, minor: 0 }, argv: [B.fetch('peer')] }] }
    if confirm
      reg[:extensions][0][:commands] = [{ path: ['fixture-confirm'], handler: 'confirm' }]
    else
      reg[:extensions][0][:hooks] = [{ event: 'deploy.prepare', handler: 'prepare' }]
    end
    # Real finite source binding, rather than treating a fixture template as an
    # operational site registry. This is only our disposable peer's authority.
    reg[:bound_sources] = [{ path: 'flake.nix', sha256: Digest::SHA256.file(root + '/flake.nix').hexdigest }]
    File.write(root + '/peer-registry.json', JSON.generate(reg))
    { 'CONFCTL_EXTENSION_ROOT' => root, 'CONFCTL_EXTENSION_REGISTRY' => root + '/peer-registry.json' }
  end

  def fixture_git(root, *argv)
    child = fixture_start(root, fixture_env(root), [B.fetch('git'), *argv])
    child[:writer].close
    fixture_ok(fixture_wait(child)).fetch('out')
  ensure
    fixture_cleanup(child)
  end

  def fixture_deploy_rules(root)
    [ { 'tool' => 'nix-copy-closure', 'contains' => root + '/top', 'stderr' => "copying 1 paths\n" },
      { 'tool' => 'ssh', 'contains' => '/bin/switch-to-configuration', 'stdout' => 'fixture activation only' },
      { 'tool' => 'ssh', 'contains' => 'nix-env -p ', 'stdout' => '' } ]
  end

  def fixture_health(root, fail: false)
    path = File.join(root, 'health-probe')
    File.write(path, "#!#{B.fetch('ruby')}\nFile.open(#{(root + '/health-events').inspect}, 'a') { |f| f.puts('health') }\nwarn 'failure detail'\nexit #{fail ? 7 : 0}\n")
    File.chmod(0700, path)
    inventory = JSON.parse(File.read(root + '/machines.json'))
    inventory.fetch('node').fetch('metaConfig')['healthChecks'] = {
      'builderCommands' => [{ 'command' => [path, 'literal token'], 'description' => '', 'exitStatus' => 0,
                             'standardOutput' => { 'match' => nil, 'include' => [], 'exclude' => [] },
                             'standardError' => { 'match' => nil, 'include' => [], 'exclude' => [] }, 'timeout' => 0, 'cooldown' => 0 }]
    }
    File.write(root + '/machines.json', JSON.generate(inventory))
  end

  def fixture_cancel(child, signal, terminal: false)
    identity = fixture_stat(child.fetch(:pid))
    abort 'lost initial owned signal target' unless identity && identity[:start] == child.fetch(:identity)[:start] && identity[:session] == child[:pid]
    if terminal
      child[:writer].write("\x03") # Default PTY ISIG, child's foreground group.
    else
      Process.kill(signal, child.fetch(:pid))
    end
    fixture_wait(child) # Deadline failure precedes independent emergency cleanup.
  end

  def fixture_site(root, pair, rebuilt: false)
    source, registry, package = if rebuilt
      pair.values_at('rebuiltSource', 'rebuiltRegistry', 'rebuiltPackage')
    else
      pair.values_at('source', 'registry', 'package')
    end
    abort 'missing explicit operational site pair' if [source, registry, package].any?(&:nil?)
    reg = JSON.parse(File.read(registry))
    abort 'fixture template is not operational authority' unless reg.fetch('bound_sources').length == 10
    abort 'site executable/registry mismatch' unless reg.fetch('extensions').all? { |entry| entry.fetch('argv') == [package + '/bin/vpsfree-confctl-ext'] }
    reg.fetch('bound_sources').each do |bound|
      path = bound.fetch('path')
      abort 'unconfined site source path' if path.start_with?('/') || path.split('/').include?('..')
      original = File.join(source, path)
      abort 'supplied registry/source mismatch' unless Digest::SHA256.file(original).hexdigest == bound.fetch('sha256')
      target = File.join(root, path)
      FileUtils.mkdir_p(File.dirname(target))
      FileUtils.cp(original, target)
      abort 'copied source mismatch' unless Digest::SHA256.file(target).hexdigest == bound.fetch('sha256')
    end
    FileUtils.mkdir_p([root + '/cluster', root + '/configs/node'])
    File.write(root + '/cluster/cluster.nix', "[]\n")
    { 'CONFCTL_EXTENSION_REGISTRY' => registry, 'CONFCTL_EXTENSION_ROOT' => File.realpath(root) }
  end
RUBY_GUEST

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
  encoded = Base64.strict_encode64(prefix + DELIVERY_CLI_SUPPORT + source)
  command = [DELIVERY_BINDINGS.fetch('ruby'), '-rbase64', '-e', 'eval(Base64.strict_decode64(ARGV.fetch(0)), TOPLEVEL_BINDING, "installed-delivery-guest")', encoded]
  _, output = @delivery_machine.succeeds(Shellwords.join(command), timeout: timeout)
  JSON.parse(output)
end

describe 'installed CLI with ordinary and operation-bound packages', order: :defined do
  selected = DELIVERY_BINDINGS['cli']

  it 'keeps all public command help, syntax exits and installed docs/notices', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST', timeout: 60)
      root = fixture_case('normal')
      commands = ['init', 'add', 'rename', 'rediscover', 'inputs ls', 'inputs update', 'inputs set',
                  'inputs channel ls', 'inputs channel update', 'inputs channel set', 'inputs machine update', 'inputs machine set',
                  'ls', 'build', 'deploy', 'health-check', 'status', 'changelog', 'diff', 'test-connection', 'ssh', 'cssh',
                  'generation ls', 'generation rm', 'generation rotate', 'collect-garbage',
                  'gen-data vpsadmin all', 'gen-data vpsadmin containers', 'gen-data vpsadmin network']
      commands.each do |command|
        result = fixture_ok(fixture_run(root, command.split + ['--help', '--invalid-after-help'], executable: B.fetch('cli')))
        abort 'missing help' unless result.fetch('out').include?('SYNOPSIS') && result.fetch('err').empty?
      end
      version = fixture_run(root, ['--version'], executable: B.fetch('cli'))
      abort 'version exit/stream contract' unless version.values_at('exit', 'out', 'err') == [1, '', "confctl: version unknown\nerror: confctl: version unknown\n"]
      [['--bad-root'], ['inputs', 'set', '--bad-option'], ['ssh', '--bad-option'], ['generation', 'ls', '--bad-option']].each do |args|
        bad = fixture_run(root, args, executable: B.fetch('cli'))
        abort 'syntax exit category' unless bad.fetch('exit') == 64 && bad.fetch('err').include?('error:')
      end
      share = File.expand_path('../share', File.dirname(B.fetch('cli')))
      %w[confctl confctl-options.nix kexec-netboot].each do |name|
        abort "missing manual #{name}" unless Dir.glob(share + '/man/man8/' + name + '.8{,.gz}').any? { |path| File.size?(path) }
      end
      notice = share + '/doc/confctl/notices/internal/ui/pager_source.NOTICE'
      abort 'lost surviving tty-screen MIT attribution' unless File.read(notice).include?('tty-screen') && File.read(notice).include?('MIT')
      abort 'help/syntax created config state' if File.exist?(root + '/.confctl')
      puts JSON.generate('help_count' => commands.length, 'version_exit' => version.fetch('exit'))
    RUBY_GUEST
    expect(actual.values_at('help_count', 'version_exit')).to eq([29, 1])
  end

  it 'shows the ordinary normal package live build frame before its real Nix builder is released', skip: !selected do
    actual = delivery_guest(<<~RUBY_GUEST, timeout: 120)
      puts JSON.generate(fixture_installed_tui_build('normal', B.fetch('cli')))
    RUBY_GUEST
    expect(actual.values_at('product', 'exit', 'signal')).to eq(['normal', 0, nil])
    expect(actual.fetch('before_release').values_at('release_absent', 'cli_alive', 'builder_alive')).to eq([true, true, true])
    screen = Base64.strict_decode64(actual.fetch('before_release').fetch('frame_base64'))
    finish = screen.rindex("\e[32;1H\e[2K")
    frame = screen[screen.rindex("\e[H", finish)...(finish + "\e[32;1H\e[2K".bytesize)]
    plain = frame.gsub(/\e\[[0-9;]*m/, '')
    expect(plain).to include(actual.fetch('token'), 'Command: confctl', "Started: #{actual.fetch('timing').fetch('started')}", '  Elapsed: ', 'Build group: 1/1 (1 machines)', 'Live view')
    expect(actual.fetch('timing').fetch('footer_count')).to eq(1)
    expect(plain).to match(%r{Building +\[running[^\]\r\n]*\] +1/1\b})
    expect(plain).to match(/Fetching +\[pending\]/)
    expect(frame).to include("\e[1mCommand:\e[0m", "\e[1mLive view ")
  end

  it 'shows the ordinary prototype package live build frame before its real Nix builder is released', skip: !selected do
    actual = delivery_guest(<<~RUBY_GUEST, timeout: 120)
      puts JSON.generate(fixture_installed_tui_build('prototype', B.fetch('prototypeCLI')))
    RUBY_GUEST
    expect(actual.values_at('product', 'exit', 'signal')).to eq(['prototype', 0, nil])
    expect(actual.fetch('before_release').values_at('release_absent', 'cli_alive', 'builder_alive')).to eq([true, true, true])
    screen = Base64.strict_decode64(actual.fetch('before_release').fetch('frame_base64'))
    finish = screen.rindex("\e[32;1H\e[2K")
    frame = screen[screen.rindex("\e[H", finish)...(finish + "\e[32;1H\e[2K".bytesize)]
    expect(frame).to include(actual.fetch('token'), 'Command: confctl', "Started: #{actual.fetch('timing').fetch('started')}", '  Elapsed: ', 'Build group: 1/1 (1 machines)', 'Live view')
    expect(actual.fetch('timing').fetch('footer_count')).to eq(1)
    expect(frame).to match(%r{Building +\[running[^\]\r\n]*\] +1/1\b})
    expect(frame).to match(/Fetching +\[pending\]/)
    expect(frame).not_to match(/\e\[[0-9;]*m/)
  end

  it 'creates and renames a disposable configuration without losing duplicate-add state', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST')
      support = fixture_case('configuration')
      root = support + '/configuration'
      Dir.mkdir(root, 0700)
      abort 'init target is not empty' unless Dir.empty?(root)
      # Captures and environment support stay outside Init's empty child CWD.
      fixture_ok(fixture_run(support, ['init'], cwd: root))
      %w[home tmp plan trace].each { |part| Dir.mkdir(File.join(root, part), 0700) }
      [['add', 'edge/site'], ['rename', 'edge/site', 'edge/renamed'], ['rediscover']].each { |args| fixture_ok(fixture_run(root, args)) }
      %w[flake.nix cluster/cluster.nix cluster/edge/renamed/module.nix cluster/edge/renamed/config.nix].each { |name| abort "missing #{name}" unless File.file?(root + '/' + name) }
      abort 'old machine directory remains' if File.exist?(root + '/cluster/edge/site')
      before = File.read(root + '/cluster/cluster.nix')
      duplicate = fixture_run(root, ['add', 'edge/renamed'])
      abort 'duplicate changed cluster' unless duplicate.fetch('exit') == 1 && File.read(root + '/cluster/cluster.nix') == before
      puts JSON.generate('renamed' => true, 'duplicate_exit' => duplicate.fetch('exit'))
    RUBY_GUEST
    expect(actual).to eq('renamed' => true, 'duplicate_exit' => 1)
  end

  it 'uses real Git for six optional commits, editor and hook refusal without consuming unrelated changes', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST', timeout: 90)
      argv = [ ['inputs', 'update', '--commit', '--no-changelog', 'a'],
               ['inputs', 'channel', 'update', '--commit', '--no-changelog', 'stable', 'os'],
               ['inputs', 'machine', 'update', '--commit', '--no-changelog', 'node', 'os'],
               ['inputs', 'set', '--commit', '--no-changelog', 'a', 'next'],
               ['inputs', 'channel', 'set', '--commit', '--no-changelog', 'stable', 'os', 'next'],
               ['inputs', 'machine', 'set', '--commit', '--no-changelog', 'node', 'os', 'next'] ]
      lock = { 'nodes' => { 'root' => { 'inputs' => { 'a' => 'a' } }, 'a' => {
        'locked' => { 'type' => 'git', 'url' => 'https://repo.invalid', 'rev' => '123456789' },
        'original' => { 'type' => 'git', 'url' => 'https://repo.invalid', 'ref' => 'main' } } } }
      (argv + [argv.first, argv.first]).each_with_index do |args, i|
        root = fixture_case("commit-#{i}")
        File.write(root + '/flake.lock', JSON.generate(lock))
        next_lock = JSON.parse(JSON.generate(lock))
        next_lock['nodes']['a']['locked']['rev'] = 'abcdef987654321'
        File.write(root + '/next.lock', JSON.generate(next_lock))
        File.write(root + '/unrelated', 'initial')
        fixture_git(root, 'init', '--quiet')
        fixture_git(root, 'add', 'flake.nix', 'flake.lock', 'unrelated')
        fixture_git(root, 'commit', '--quiet', '-m', 'Fixture base')
        before = fixture_git(root, 'rev-parse', 'HEAD')
        File.write(root + '/unrelated', 'staged unrelated')
        fixture_git(root, 'add', 'unrelated')
        File.write(root + '/flake.nix', '{} # unstaged')
        hook = root + '/.git/hooks/pre-commit'
        File.write(hook, "#!#{B.fetch('ruby')}\nFile.open(#{(root + '/hook').inspect}, 'a') { |f| f.puts('hook') }\nwarn 'hook detail'\nexit #{i == 6 ? 9 : 0}\n")
        File.chmod(0700, hook)
        editor = root + '/editor'
        File.write(editor, "#!#{B.fetch('ruby')}\nFile.write(#{(root + '/edited').inspect}, 'yes')\nFile.open(ARGV.fetch(0), 'a') { |f| f.puts('Edited by fixture') }\n")
        File.chmod(0700, editor)
        rules = [ { 'tool' => 'nix', 'contains' => '.#confctl.settings', 'stdout' => '{"nix":{"maxJobs":"auto","impureEval":false},"list":{"columns":["name"]}}' },
                  { 'tool' => 'nix', 'contains' => '.#confctl.channels', 'stdout' => '{"stable":{"os":"a"}}' },
                  { 'tool' => 'nix', 'contains' => '.#confctl.machineKeys', 'stdout' => '{"node":"node-key"}' },
                  { 'tool' => 'nix', 'contains' => '.#confctl.inputsInfo.node-key', 'stdout' => '{"os":{"input":"a"}}' },
                  { 'tool' => 'nix', 'contains' => 'flake', 'stdout' => '' } ]
        fixture_plan(root, rules)
        result = fixture_run(root, args, env: { 'GIT_EDITOR' => editor }, tty: i == 7)
        expected = i == 6 ? 1 : 0
        abort "commit #{i}: #{result.inspect}" unless result.fetch('exit') == expected
        after = fixture_git(root, 'rev-parse', 'HEAD')
        abort 'commit/refusal head semantics' unless (before == after) == (i == 6)
        unless i == 6
          abort 'committed unrelated paths' unless fixture_git(root, 'diff-tree', '--no-commit-id', '--name-only', '-r', 'HEAD').strip == 'flake.lock'
        end
        abort 'consumed unrelated index/worktree' unless fixture_git(root, 'diff', '--cached', '--name-only').strip == 'unrelated' && File.read(root + '/flake.nix') == '{} # unstaged'
        abort 'lost mutation/hook' unless JSON.parse(File.read(root + '/flake.lock')) == next_lock && File.read(root + '/hook') == "hook\n"
        abort 'editor TTY policy' unless File.exist?(root + '/edited') == (i == 7)
        abort 'failure log lost' if i == 6 && Dir.glob(root + '/.confctl/logs/*.log').empty?
        fixture_events(root)
      end
      puts JSON.generate('commits' => 6, 'refusal' => true, 'tty_editor' => true)
    RUBY_GUEST
    expect(actual).to eq('commits' => 6, 'refusal' => true, 'tty_editor' => true)
  end

  it 'preserves refusal, handled host failures and literal SSH command/input data', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST')
      root = fixture_case('connection')
      rules = fixture_machines(root, ['one', 'two'])
      rules.select { |r| r['tool'] == 'ssh' && r['contains'] == 'cat /proc/uptime' }.each { |r| r['exit'] = 255 }
      fixture_plan(root, rules)
      refused = fixture_run(root, ['ssh', '*', 'printf', 'literal% value'], input: "n\n")
      abort 'refusal ran SSH' unless refused.fetch('exit') == 1 && fixture_events(root).none? { |e| e['tool'] == 'ssh' }
      completed = fixture_run(root, ['test-connection', '-y', '*'])
      abort 'host failure changed invocation exit' unless completed.fetch('exit') == 0 && completed.fetch('out').include?('2 failed')
      rules.unshift('tool' => 'ssh', 'contains' => 'printf', 'read_stdin' => true, 'stdin' => 'stdin payload', 'stdout' => "literal reply\n")
      fixture_plan(root, rules)
      literal = fixture_run(root, ['ssh', '-y', '-i', 'stdin payload', 'one', 'printf', 'literal% value', '--help'])
      fixture_ok(literal)
      event = fixture_events(root).find { |e| e['stdin'] == 'stdin payload' }
      abort 'lost literal argv/stdin' unless event && event.fetch('argv').last.include?('--help') && literal.fetch('out').include?('literal reply')
      puts JSON.generate('refused' => 1, 'handled_exit' => completed.fetch('exit'), 'stdin' => event.fetch('stdin'))
    RUBY_GUEST
    expect(actual).to eq('refused' => 1, 'handled_exit' => 0, 'stdin' => 'stdin payload')
  end

  it 'keeps gen-data syntax and empty-username authentication failure ahead of inventory', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST')
      rows = %w[all containers network].map do |leaf|
        root = fixture_case('gen-data-' + leaf)
        # No username is supplied. Unlike username-then-EOF, this cannot reach
        # the production provider. Provider TLS/payload tests remain separate.
        result = fixture_run(root, ['gen-data', 'vpsadmin', leaf])
        abort 'gen-data authentication failure category/order' unless result.fetch('exit') == 1 && result.fetch('out').start_with?('User name: ') && result.fetch('err').include?('authentication failed') && fixture_events(root).empty?
        logs = Dir.glob(root + '/.confctl/logs/*.log')
        abort 'gen-data failure log missing' unless logs.length == 1 && logs.first.end_with?("-confctl-gen-data-vpsadmin-#{leaf}.log")
        abort 'prompt reached command log' if File.read(logs.first).include?('User name:')
        syntax = fixture_run(root, ['gen-data', 'vpsadmin', leaf, '--endpoint=fixture.invalid'])
        abort 'new provider endpoint option appeared' unless syntax.fetch('exit') == 64
        [leaf, result.fetch('exit')]
      end
      puts JSON.generate('rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows')).to eq([['all', 1], ['containers', 1], ['network', 1]])
  end

  it 'compares disposable real Git histories with installed confirmation and SystemPager', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST')
      root = fixture_case('comparison')
      rules = fixture_machines(root)
      fixture_git(root, 'init', '--quiet')
      File.write(root + '/payload', 'old payload')
      fixture_git(root, 'add', 'payload')
      fixture_git(root, 'commit', '--quiet', '-m', 'Fixture old')
      old = fixture_git(root, 'rev-parse', 'HEAD').strip
      File.write(root + '/payload', 'new payload')
      fixture_git(root, 'commit', '--quiet', '-am', 'Fixture target change')
      fresh = fixture_git(root, 'rev-parse', 'HEAD').strip
      rules.find { |r| r['contains'] == '.#confctl.inputsInfo.node' }['stdout'] = JSON.generate(os: { rev: fresh, url: root })
      rules.find { |r| r['contains'] == 'cat /etc/confctl/inputs-info.json' }['stdout'] = JSON.generate(os: { rev: old, url: root })
      selected = fixture_seed(root, 'node', 'selected')
      record = JSON.parse(File.read(selected + '/generation.json'))
      record['inputs_info'] = { 'os' => { 'rev' => fresh, 'url' => root } }
      File.write(selected + '/generation.json', JSON.generate(record))
      fixture_plan(root, rules)
      %w[diff changelog].each do |name|
        result = fixture_ok(fixture_run(root, [name, '-y', '-gselected'], tty: true))
        marker = name == 'diff' ? "node @ os (#{root}): #{old[0, 8]} -> #{fresh[0, 8]}" : 'Fixture target change'
        abort "missing #{name} effect: #{result.inspect}" unless result.fetch('out').include?(marker)
      end
      # One queued write supplies confirmation plus Q. SystemPager exits at
      # output EOF; this does not claim Basic paging or consumption of Q.
      pager = File.join(B.fetch('coreutils'), 'cat')
      result = fixture_ok(fixture_run(root, ['changelog', '-gselected'], input: "y\nQ", tty: true, tty_stderr: true,
                                     env: { 'PAGER' => pager, 'GIT_PAGER' => pager }))
      abort 'SystemPager lost comparison output' unless result.fetch('out').include?('Fixture target change') && result.fetch('err').empty?
      fixture_events(root)
      puts JSON.generate('commands' => %w[diff changelog], 'pager' => 'SystemPager', 'streams' => 'merged PTY stdout/stderr', 'comparison_output' => result.fetch('out'))
    RUBY_GUEST
    expect(actual.values_at('commands', 'pager', 'streams')).to eq([%w[diff changelog], 'SystemPager', 'merged PTY stdout/stderr'])
    expect(actual.fetch('comparison_output')).to include('Fixture target change')
  end

  it 'writes/reuses generation state, distinguishes status targets and keeps earlier state on later failure', skip: !selected do
    actual = delivery_guest(<<~RUBY_GUEST)
      root = fixture_case('build-status')
      rules = fixture_machines(root)
      fixture_plan(root, rules)
      fixture_ok(fixture_run(root, ['build', '-y']))
      generation_root = root + '/.confctl/generations/node'
      dirs = Dir.children(generation_root).filter_map do |name|
        path = File.join(generation_root, name)
        # lstat excludes current's symlink alias from the real directory count.
        File.join(path, 'generation.json') if File.lstat(path).directory?
      end
      abort 'actual writer created no generation' unless dirs.length == 1
      before = File.read(dirs.first)
      current = File.readlink(root + '/.confctl/generations/node/current')
      fixture_ok(fixture_run(root, ['build', '-y']))
      abort 'reuse rewrote metadata/current' unless File.read(dirs.first) == before && File.readlink(root + '/.confctl/generations/node/current') == current
      ['none', current, 'current', '0'].each { |target| fixture_ok(fixture_run(root, ['status', '-y', '-g', target])) }
      fixture_ok(fixture_run(root, ['status', '-y']))
      rules.find { |r| r['contains'] == '.#confctl.build.node.toplevel' }['exit'] = 7
      rules.find { |r| r['contains'] == '.#confctl.build.node.toplevel' }['stderr'] = 'synthetic later build failure'
      fixture_plan(root, rules)
      failed = fixture_run(root, ['build', '-y'])
      abort 'failed build lost earlier state/log' unless failed.fetch('exit') == 1 && File.read(dirs.first) == before && File.readlink(root + '/.confctl/generations/node/current') == current && !Dir.glob(root + '/.confctl/logs/*.log').empty?
      fixture_events(root)
      puts JSON.generate('record' => JSON.parse(before), 'current' => current, 'later_failure' => failed.fetch('exit'))
    RUBY_GUEST
    expect(actual.fetch('record').fetch('mode')).to eq('flakes')
    expect(actual.fetch('later_failure')).to eq(1)
  end

  it 'removes/rotates only selected remote generations, orders fake GC and retains a partial failure', skip: !selected do
    actual = delivery_guest(<<~RUBY_GUEST)
      rows = []
      ['rm', 'rotate', 'partial', 'gc'].each do |mode|
        root = fixture_case('retention-' + mode)
        rules = fixture_machines(root)
        old = fixture_seed(root, 'node', 'old', '2020-01-01T00:00:00Z')
        fixture_seed(root, 'node', 'new')
        File.symlink('new', File.join(File.dirname(old), 'current'))
        before = Dir.glob(root + '/.confctl/generations/**/*').sort.map { |path| [path, File.symlink?(path) ? File.readlink(path) : (File.file?(path) ? File.read(path) : 'directory')] }
        rules << { 'tool' => 'ssh', 'contains' => '--delete-generations 1' }
        rules << { 'tool' => 'ssh', 'contains' => '--delete-generations 2', 'exit' => mode == 'partial' ? 23 : 0, 'stderr' => mode == 'partial' ? 'synthetic delete failure' : '' }
        rules << { 'tool' => 'ssh', 'contains' => 'nix-collect-garbage', 'stdout' => '1 store paths deleted' }
        fixture_plan(root, rules)
        args = mode == 'gc' ? ['collect-garbage', '-y'] : ['generation', mode == 'partial' ? 'rm' : mode, '-ry']
        result = fixture_run(root, args)
        abort 'retention exit' unless result.fetch('exit') == (mode == 'partial' ? 1 : 0)
        events = fixture_events(root).select { |e| e['tool'] == 'ssh' }
        deletes = events.select { |e| e['argv'].last.include?('--delete-generations') }
        gc = events.select { |e| e['argv'].last == 'nix-collect-garbage' }
        abort 'GC ran after partial failure' if mode == 'partial' && !gc.empty?
        abort 'current generation rotated' if mode == 'rotate' && deletes.any? { |e| e['argv'].last.end_with?(' 2') }
        abort 'missing fake GC' if mode != 'partial' && gc.length != 1
        abort 'GC preceded deletion completion' if !deletes.empty? && !gc.empty? && gc.first.fetch('start_ns') < deletes.last.fetch('end_ns')
        after = before.map { |path, _value| [path, File.symlink?(path) ? File.readlink(path) : (File.file?(path) ? File.read(path) : 'directory')] }
        abort 'remote mutation changed local state' unless before == after
        rows << [mode, result.fetch('exit')]
      end
      puts JSON.generate('rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows')).to eq([['rm', 0], ['rotate', 0], ['partial', 1], ['gc', 0]])
  end

  it 'passes deploy action/options to prepare, refreshes before copy and retains failed activation/health effects', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST')
      rows = []
      ['switch', 'test', 'default-switch', 'activation-failure', 'health-failure'].each do |mode|
        root = fixture_case('deploy-' + mode)
        activating = !['switch', 'test'].include?(mode)
        rules = fixture_machines(root, previous: activating) + fixture_deploy_rules(root)
        selected = fixture_seed(root, 'node', 'selected')
        File.symlink('selected', File.join(File.dirname(selected), 'current'))
        fixture_health(root, fail: true) if mode == 'health-failure'
        if ['switch', 'test'].include?(mode)
          fresh = JSON.parse(File.read(root + '/machines.json'))
          fresh['node']['metaConfig']['host']['target'] = 'fresh.invalid'
          File.write(root + '/machines.after.json', JSON.generate(fresh))
        end
        rules.find { |r| r['contains'] == '/bin/switch-to-configuration' }['exit'] = 23 if mode == 'activation-failure'
        fixture_plan(root, rules)
        args = ['deploy', '-y', '-gselected', '--disable-auto-rollback']
        args << '--no-health-checks' unless mode == 'health-failure'
        args << '--copy-only' if ['switch', 'test'].include?(mode)
        args += ['node', 'test'] if mode == 'test'
        result = fixture_run(root, args, env: fixture_peer(root))
        expected = mode.end_with?('failure') ? 1 : 0
        abort "deploy #{mode}: #{result.inspect}" unless result.fetch('exit') == expected
        context = JSON.parse(File.read(root + '/peer-context.json'))
        abort 'prepare invocation metadata' unless context.values_at('event', 'action', 'origin_command', 'selected_names', 'raw_argv') == ['deploy.prepare', mode == 'test' ? 'test' : 'switch', ['deploy'], ['node'], args]
        events = fixture_events(root)
        copy = events.find { |e| e['tool'] == 'nix-copy-closure' }
        abort 'copy missing' unless copy
        if activating
          activation = events.select { |e| e['tool'] == 'ssh' && e['argv'].last.include?('/bin/switch-to-configuration') }
          abort 'intended activation phase not reached' unless activation.length == 1 && activation.first.fetch('exit') == (mode == 'activation-failure' ? 23 : 0)
          abort 'default switch did not update profile' if mode == 'default-switch' && events.none? { |e| e['tool'] == 'ssh' && e['argv'].last.start_with?('nix-env -p ') }
        end
        abort 'prepare did not refresh inventory before copy' if ['switch', 'test'].include?(mode) && !copy.fetch('argv').include?('root@fresh.invalid')
        abort 'copy-only activated' if context.fetch('options')['copy-only'] && events.any? { |e| e['tool'] == 'ssh' }
        abort 'failed operation lost log' if expected == 1 && Dir.glob(root + '/.confctl/logs/*.log').empty?
        abort 'health failure not reached' if mode == 'health-failure' && File.read(root + '/health-events') != "health\n"
        rows << [mode, result.fetch('exit')]
      end
      puts JSON.generate('rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows')).to eq([['switch', 0], ['test', 0], ['default-switch', 0], ['activation-failure', 1], ['health-failure', 1]])
  end
end

describe 'public confirmation cancellation with owned open input', order: :defined do
  selected = DELIVERY_BINDINGS['cli']

  it 'exits on actual INT/TERM without newline or EOF on pipes and a foreground PTY', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST', timeout: 90)
      rows = [ ['pipe-empty-int', false, '', 'INT', false, 1], ['pipe-empty-term', false, '', 'TERM', false, 1],
               ['pipe-partial-int', false, 'y', 'INT', false, 1], ['pipe-partial-term', false, 'y', 'TERM', false, 1],
               ['pty-term', true, '', 'TERM', false, 1], ['pty-ctrl-c', true, '', 'INT', true, 1] ].map do |name, tty, partial, signal, terminal, code|
        root = fixture_case(name)
        fixture_plan(root, fixture_machines(root))
        child = fixture_start(root, fixture_env(root), [B.fetch('operationCLI'), 'test-connection'], tty: tty)
        begin
          fixture_until(child, 8) { fixture_output(child).include?('Continue?') }
          child[:writer].write(partial) unless partial.empty?
          result = fixture_cancel(child, signal, terminal: terminal)
          abort "public cancellation status: #{result.inspect}" unless result.fetch('exit') == code && result.fetch('signal').nil?
          abort 'cancelled confirmation reached SSH' if fixture_events(root).any? { |e| e['tool'] == 'ssh' }
          abort 'cancelled operation lost log' if Dir.glob(root + '/.confctl/logs/*.log').empty?
          [name, result]
        ensure
          fixture_cleanup(child)
        end
      end
      # Preserve actual statuses/output in the runner stream, not a normalized
      # terminal golden or a claim about an original Ruby signal's numeric code.
      puts JSON.generate('rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows').length).to eq(6)
  end

  it 'records the matching finite original Ruby signal cases separately', skip: !selected do
    skip 'original lane unselected: supply oracleSrc' unless DELIVERY_BINDINGS['originalCLI']
    actual = delivery_guest(<<~'RUBY_GUEST', timeout: 90)
      rows = [ [false, '', 'INT', false], [false, '', 'TERM', false], [false, 'y', 'INT', false],
               [false, 'y', 'TERM', false], [true, '', 'TERM', false], [true, '', 'INT', true] ].each_with_index.map do |(tty, partial, signal, terminal), i|
        root = fixture_case("ruby-signal-#{i}")
        fixture_plan(root, fixture_machines(root))
        child = fixture_start(root, fixture_env(root), [B.fetch('originalCLI'), 'test-connection'], tty: tty)
        begin
          fixture_until(child, 8) { fixture_output(child).include?('Continue?') }
          child[:writer].write(partial) unless partial.empty?
          result = fixture_cancel(child, signal, terminal: terminal)
          abort 'original signal silently accepted permission' if result.fetch('exit') == 0 || fixture_events(root).any? { |e| e['tool'] == 'ssh' }
          [i, result]
        ensure
          fixture_cleanup(child)
        end
      end
      puts JSON.generate('lane' => 'fixture-bound original Ruby, numeric parity not assumed', 'rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows').length).to eq(6)
  end

  it 'cancels dry-activation retry and failed-health permission while retaining prior fake effects', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST', timeout: 60)
      rows = %w[dry-activation health].map do |phase|
        root = fixture_case('retry-' + phase)
        rules = fixture_machines(root, previous: true) + fixture_deploy_rules(root)
        selected = fixture_seed(root, 'node', 'selected')
        File.symlink('selected', File.join(File.dirname(selected), 'current'))
        fixture_health(root, fail: true) if phase == 'health'
        fixture_plan(root, rules)
        args = ['deploy', '-y', '-gselected', '--disable-auto-rollback', '--interactive']
        args.concat(['--dry-activate-first', '--no-health-checks']) if phase == 'dry-activation'
        child = fixture_start(root, fixture_env(root), [B.fetch('operationCLI'), *args])
        begin
          target = phase == 'dry-activation' ? 'Choose [ysar]:' : 'Choose [crfa]:'
          permissions = phase == 'dry-activation' ? ['Choose [yn]:'] : ['Choose [yn]:', 'Choose [ysa]:', 'Choose [yn]:']
          answered = 0
          fixture_until(child, 10) do
            prompts = fixture_output(child).scan(/Choose \[[a-z]+\]:/)
            next false unless prompts.length > answered
            if prompts[answered] == target
              abort 'retry permission order' unless answered == permissions.length && prompts.length == answered + 1
              true
            else
              abort 'unexpected retry prompt' unless prompts[answered] == permissions[answered]
              child[:writer].write("y\n")
              answered += 1
              false
            end
          end
          before = fixture_events(root)
          copies = before.select { |e| e['tool'] == 'nix-copy-closure' }
          abort 'retry not reached after one successful copy' unless copies.length == 1 && copies.first.fetch('exit') == 0
          activation = before.select { |e| e['tool'] == 'ssh' && e['argv'].last.include?('/bin/switch-to-configuration') }
          action = phase == 'dry-activation' ? 'dry-activate' : 'switch'
          abort 'retry not reached after intended successful activation' unless activation.length == 1 && activation.first.fetch('exit') == 0 && activation.first.fetch('argv').last.end_with?('/bin/switch-to-configuration ' + action)
          # timeout=0/cooldown=0 admits one automatic probe attempt. The open
          # prompt now tests interactive retry permission, not timed probe retry.
          abort 'health retry not reached after single probe' if phase == 'health' && File.read(root + '/health-events') != "health\n"
          abort 'dry retry reached health' if phase == 'dry-activation' && File.exist?(root + '/health-events')
          result = fixture_cancel(child, 'TERM')
          abort 'retry cancellation exit' unless result.fetch('exit') == 1 && result.fetch('signal').nil?
          abort 'cancellation ran another effect' unless fixture_events(root) == before
          abort 'earlier health effect lost/retried' if phase == 'health' && File.read(root + '/health-events') != "health\n"
          abort 'cancelled dry retry reached health' if phase == 'dry-activation' && File.exist?(root + '/health-events')
          abort 'retry failure log lost' if Dir.glob(root + '/.confctl/logs/*.log').empty?
          [phase, result]
        ensure
          fixture_cleanup(child)
        end
      end
      puts JSON.generate('rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows').map(&:first)).to eq(%w[dry-activation health])
  end

  it 'joins reverse confirmations and owned extension children, serializing concurrent prompts', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST')
      root = fixture_case('reverse-affirmative')
      accepted = fixture_ok(fixture_run(root, ['fixture-confirm'], input: "y\n", env: fixture_peer(root, confirm: true)))
      abort 'affirmative control did not decode accepted object' unless accepted.fetch('out').scan(/Fixture permission/).length == 1 && File.read(root + '/peer-events') == "prepare\naccepted\n"
      rows = [1, 2].map do |count|
        root = fixture_case("reverse-#{count}")
        env = fixture_env(root).merge(fixture_peer(root, confirm: true)).merge('DELIVERY_CONFIRM_COUNT' => count.to_s)
        child = fixture_start(root, env, [B.fetch('operationCLI'), 'fixture-confirm'])
        begin
          fixture_until(child, 8) { fixture_output(child).include?('Fixture permission') }
          # Both requests have been submitted by the finite peer; the gate may
          # expose only one prompt until that read completes/cancels.
          abort 'parallel reverse readers/prompts' unless fixture_output(child).scan(/Fixture permission/).length == 1
          result = fixture_cancel(child, 'TERM')
          abort 'reverse cancellation exit' unless result.fetch('exit') == 1 && result.fetch('signal').nil?
          abort 'second prompt after cancellation' unless result.fetch('out').scan(/Fixture permission/).length == 1
          [count, result]
        ensure
          fixture_cleanup(child)
        end
      end
      root = fixture_case('hook-confirm-refusal')
      fixture_plan(root, fixture_machines(root))
      fixture_seed(root, 'node', 'selected')
      env = fixture_peer(root)
      registry = JSON.parse(File.read(env.fetch('CONFCTL_EXTENSION_REGISTRY')))
      registry.fetch('extensions').first.fetch('hooks').first['handler'] = 'confirm'
      File.write(env.fetch('CONFCTL_EXTENSION_REGISTRY'), JSON.generate(registry))
      result = fixture_run(root, ['deploy', '-y', '-gselected', '--copy-only'], env: env)
      abort 'hook acquired input permission' unless result.fetch('exit') == 1 && !result.fetch('out').include?('Fixture permission')
      abort 'hook refusal reached copy' if fixture_events(root).any? { |e| e['tool'] == 'nix-copy-closure' }
      puts JSON.generate('rows' => rows, 'affirmative_control' => true, 'hook_refused' => true)
    RUBY_GUEST
    expect(actual.fetch('affirmative_control')).to be(true)
    expect(actual.fetch('hook_refused')).to be(true)
  end

  it 'keeps a deliberate fixture deadline failed even when independent cleanup succeeds', skip: !selected do
    actual = delivery_guest(<<~RUBY_GUEST)
      root = fixture_case('cleanup-fault')
      child = fixture_start(root, fixture_env(root), [B.fetch('ruby'), '-e', 'sleep 30'])
      refused = false
      begin
        fixture_wait(child, seconds: 0.05)
      rescue RuntimeError => e
        raise unless e.message == 'fixture deadline (normal completion failed)'
        refused = true
      ensure
        fixture_cleanup(child)
      end
      abort 'cleanup changed a failed acceptance into success' unless refused && child[:status].signaled? && fixture_members(child).empty?
      puts JSON.generate('normal_deadline_failed' => refused, 'cleanup_signal' => child[:status].termsig)
    RUBY_GUEST
    expect(actual).to eq('normal_deadline_failed' => true, 'cleanup_signal' => 9)
  end
end

describe 'installed state interoperability with explicit original Ruby adapters', order: :defined do
  it 'reads old Ruby records in Go and actual Go writer records in the old Ruby reader' do
    skip 'select appSrc/appDocs and immutable oracleSrc for this separate oracle lane' unless DELIVERY_BINDINGS['cli'] && DELIVERY_BINDINGS['oracleSrc']
    actual = delivery_guest(<<~RUBY_GUEST)
      # Reuse the two existing finite adapter bodies verbatim; no studied Go
      # import/test execution or new Ruby dependency in the normal package.
      writer_source = File.read(B.fetch('fixtureSrc') + '/internal/generation/ruby_adapter_test.go')
      writer = writer_source.match(/const originalWriterAdapter = `(.*?)`/m).captures.first
      reader_source = File.read(B.fetch('fixtureSrc') + '/internal/generation/writer_ruby_adapter_test.go')
      reader = reader_source.match(/const writerOriginalReader = `(.*?)`/m).captures.first
      root = fixture_case('ruby-to-go')
      fixture_plan(root, fixture_machines(root, ['nested/host']))
      oracle_env = { 'GEM_HOME' => B.fetch('oracleGemPath'), 'GEM_PATH' => B.fetch('oracleGemPath') }
      original = fixture_ok(fixture_run(root, ['-e', writer, B.fetch('oracleSrc') + '/lib', root], executable: B.fetch('oracleRuby'), env: oracle_env))
      record = JSON.parse(original.fetch('out'))
      listing = fixture_ok(fixture_run(root, ['generation', 'ls', '--local', 'nested/host']))
      abort 'Go did not read original generations/current' unless record.fetch('names').all? { |name| listing.fetch('out').include?(name) } && listing.fetch('out').include?(record.fetch('current'))
      root = fixture_case('go-to-ruby')
      fixture_plan(root, fixture_machines(root, ['nested/host']))
      fixture_ok(fixture_run(root, ['build', '-y']))
      FileUtils.mkdir_p(root + '/private-gcroots')
      original = fixture_ok(fixture_run(root, ['-e', reader, B.fetch('oracleSrc') + '/lib', root], executable: B.fetch('oracleRuby'), env: oracle_env))
      readback = JSON.parse(original.fetch('out'))
      abort 'Ruby failed actual Go record/current' unless readback.fetch('records').length == 1 && readback.fetch('current') == readback.fetch('records').first.fetch('name')
      abort 'Ruby lost metadata role' unless readback.fetch('records').first.fetch('info_roles') == ['os']
      puts JSON.generate('old_to_new' => record, 'new_to_old' => readback)
    RUBY_GUEST
    expect(actual.fetch('new_to_old').fetch('records').length).to eq(1)
  end
end

describe 'supplied operational site pairs in disposable source copies', order: :defined do
  selected = DELIVERY_BINDINGS['cli'] && !DELIVERY_BINDINGS.fetch('sitePairs').empty?

  it 'validates exact source binding, refuses stale bytes and restores only the supplied pair', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST')
      rows = B.fetch('sitePairs').each_with_index.map do |pair, i|
        root = fixture_case("site-binding-#{i}")
        fixture_plan(root, fixture_machines(root))
        env = fixture_site(root, pair)
        fixture_ok(fixture_run(root, ['runtime-kernels', 'update', '--help'], env: env))
        bound = JSON.parse(File.read(pair.fetch('registry'))).fetch('bound_sources').first.fetch('path')
        original = File.binread(root + '/' + bound)
        File.binwrite(root + '/' + bound, original + "\n# stale fixture\n")
        stale = fixture_run(root, ['runtime-kernels', 'update', '-y'], env: env)
        abort 'stale source reached effects' unless stale.fetch('exit') == 1 && fixture_events(root).empty?
        File.binwrite(root + '/' + bound, original)
        fixture_ok(fixture_run(root, ['runtime-kernels', 'update', '--help'], env: env))
        [pair.fetch('name'), 'matching / stale refused / exact old pair restored']
      end
      puts JSON.generate('rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows').length).to eq(DELIVERY_BINDINGS.fetch('sitePairs').length)
  end

  it 'uses a separately supplied rebuilt package/registry/source after a binding change', skip: !selected do
    skip 'rebuilt pair unselected/unbuilt: no registry hash regeneration by the fixture' unless DELIVERY_BINDINGS.fetch('sitePairs').any? { |pair| pair['rebuiltSource'] && pair['rebuiltRegistry'] && pair['rebuiltPackage'] }
    actual = delivery_guest(<<~'RUBY_GUEST')
      rows = B.fetch('sitePairs').select { |pair| pair['rebuiltSource'] && pair['rebuiltRegistry'] && pair['rebuiltPackage'] }.each_with_index.map do |pair, i|
        root = fixture_case("site-rebuilt-#{i}")
        env = fixture_site(root, pair, rebuilt: true)
        fixture_ok(fixture_run(root, ['runtime-kernels', 'update', '--help'], env: env))
        pair.fetch('name')
      end
      puts JSON.generate('rebuilt_pairs' => rows)
    RUBY_GUEST
    expect(actual.fetch('rebuilt_pairs')).not_to be_empty
  end

  it 'preserves rediscover order/noncarried filtering and runtime decline/partial state', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST', timeout: 60)
      rows = B.fetch('sitePairs').each_with_index.map do |pair, i|
        root = fixture_case("site-flows-#{i}")
        rules = fixture_machines(root, %w[first carried disabled second])
        env = fixture_site(root, pair)
        machines = JSON.parse(File.read(root + '/machines.json'))
        machines.each do |name, machine|
          machine['metaConfig']['netboot'] = { 'enable' => name != 'disabled' }
          FileUtils.mkdir_p(root + '/cluster/' + name)
          File.write(root + '/cluster/' + name + '/config.nix', '{}')
        end
        machines['carried']['carrier'] = 'carrier'
        File.write(root + '/machines.json', JSON.generate(machines))
        fixture_plan(root, rules)
        fixture_ok(fixture_run(root, ['rediscover'], env: env))
        netboot = File.read(root + '/cluster/netbootable.nix')
        abort 'site rediscover ordered/filter contract' unless netboot.scan(/^  "(.*)"$/).flatten == %w[first second]
        # Use a fresh disposable source copy for runtime state and its own plan.
        root = fixture_case("site-kernels-#{i}")
        rules = fixture_machines(root, %w[node failed])
        env = fixture_site(root, pair)
        machines = JSON.parse(File.read(root + '/machines.json'))
        machines.each_value { |machine| machine['metaConfig']['spin'] = 'vpsadminos' }
        File.write(root + '/machines.json', JSON.generate(machines))
        state = root + '/configs/node/kernels.json'
        File.write(state, '{"keep":{"custom":true},"failed":"old"}')
        rules << { 'tool' => 'ssh', 'host' => 'node.invalid', 'contains' => 'uname', 'stdout' => "6.12.35.extra\n" }
        rules << { 'tool' => 'ssh', 'host' => 'failed.invalid', 'contains' => 'uname', 'exit' => 23, 'stderr' => 'synthetic kernel query failure' }
        fixture_plan(root, rules)
        before = File.read(state)
        refused = fixture_run(root, ['runtime-kernels', 'update'], input: "n\n", env: env)
        abort 'decline changed state/queried SSH' unless refused.fetch('exit') == 1 && File.read(state) == before && fixture_events(root).none? { |e| e['tool'] == 'ssh' }
        partial = fixture_ok(fixture_run(root, ['runtime-kernels', 'update', '-y'], env: env))
        saved = JSON.parse(File.read(state))
        abort 'runtime partial/unselected state' unless saved == { 'keep' => { 'custom' => true }, 'node' => '6.12.35' } && partial.fetch('out').include?('Error on failed')
        [pair.fetch('name'), saved]
      end
      puts JSON.generate('rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows').length).to eq(DELIVERY_BINDINGS.fetch('sitePairs').length)
  end

  it 'runs actual default/nondefault deploy.prepare before metadata resolution and fake copy', skip: !selected do
    actual = delivery_guest(<<~'RUBY_GUEST', timeout: 60)
      rows = B.fetch('sitePairs').each_with_index.flat_map do |pair, i|
        ['', 'test'].map do |action|
          root = fixture_case("site-deploy-#{i}-#{action.empty? ? 'default' : action}")
          rules = fixture_machines(root) + fixture_deploy_rules(root)
          env = fixture_site(root, pair)
          machines = JSON.parse(File.read(root + '/machines.json'))
          machines['node']['metaConfig']['spin'] = 'vpsadminos'
          File.write(root + '/machines.json', JSON.generate(machines))
          fixture_seed(root, 'node', 'selected')
          rules << { 'tool' => 'ssh', 'contains' => 'uname', 'stdout' => "6.12.35.extra\n" }
          fixture_plan(root, rules)
          args = ['deploy', '-y', '-gselected', '--copy-only', 'node']
          args << action unless action.empty?
          fixture_ok(fixture_run(root, args, env: env))
          saved = JSON.parse(File.read(root + '/configs/node/kernels.json'))
          events = fixture_events(root)
          kernel = events.find { |e| e['tool'] == 'ssh' && e['argv'].last.include?('uname') }
          copy = events.find { |e| e['tool'] == 'nix-copy-closure' }
          abort 'site prepare did not precede later copy' unless saved == { 'node' => '6.12.35' } && kernel && copy && kernel.fetch('end_ns') < copy.fetch('start_ns')
          [pair.fetch('name'), action.empty? ? 'switch' : action]
        end
      end
      puts JSON.generate('rows' => rows)
    RUBY_GUEST
    expect(actual.fetch('rows').length).to eq(2 * DELIVERY_BINDINGS.fetch('sitePairs').length)
  end
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
    # Verbatim finite cc40679d adapter supplied by the helper contract owner.
    old_reader_adapter = <<~'OLD_NETBOOT_READER_ADAPTER'
      # PREPARATION ONLY: unexecuted finite guest adapter, not the original entrypoint.
      # Later argv: exact oracle source file, actual generated index path, current 1|2.
      require 'json'
      require 'uri'
      require 'net/http'
      require 'digest'

      abort 'expected source, generated index, current 1|2' unless ARGV.length == 3
      source_path, index_path, current_arg = ARGV
      expected_current = { '1' => 1, '2' => 2 }.fetch(current_arg)
      abort 'wrong fixture index' unless index_path == '/var/lib/confctl-delivery/http/machines.json'
      source = File.binread(source_path)
      source_sha = Digest::SHA256.hexdigest(source)
      abort 'not selected cc40679d original H4 source' unless source_sha == 'b6f9ec347d5a8b23a97a4d9224688810b9346ba6241bfd894b28eb8894c1c517'
      index_bytes = File.binread(index_path).freeze
      lines = source.lines

      # Original complete methods only; neither initialize, run nor the file tail.
      # 169..219: derive URL, fetch/parse index, pick machine.
      # 249..291: pick generation; 312..347: parse generation, pick variant.
      reader_class = Class.new
      reader_class.const_set(:MACHINE_FQDN, 'node.test'.freeze)
      [[169, 219], [249, 291], [312, 347]].each do |first, last|
        reader_class.class_eval(lines[(first - 1)..(last - 1)].join, source_path, first)
      end
      # Retain the exact original run's parameter-combination body without run itself.
      reader_class.class_eval(
        "def selected_params(generation, variant)\n" + lines[70..77].join +
        "[combined_params, final_params]\nend\n", source_path, 70
      )
      reader = reader_class.allocate
      reader.instance_variable_set(:@interactive, false)
      reader.instance_variable_set(:@append_params, '')

      forbidden_calls = []
      refuse = lambda do |edge|
        forbidden_calls << edge.to_s
        raise "forbidden old-reader effect: #{edge}"
      end
      # The fixture cmdline value is supplied, never read from /proc.
      proc_stub_calls = 0
      reader.define_singleton_method(:parse_httproot_from_cmdline) do
        proc_stub_calls += 1
        'http://127.0.0.1:18080/node.test/1/initrd'
      end
      %i[run parse_arguments interactive_pick_machine interactive_pick_variant
         list_generations download_file load_kexec exec_kexec unload_kexec
         cleanup_downloads].each do |edge|
        reader.define_singleton_method(edge) { |*| refuse.call(edge) }
      end
      # These guards are confined to this separate oracle process.
      kernel_guard = Module.new
      [:system, :exec, :spawn, :fork, :'`'].each do |edge|
        kernel_guard.define_method(edge) { |*| refuse.call("Kernel.#{edge}") }
      end
      Kernel.prepend(kernel_guard)
      Kernel.singleton_class.prepend(kernel_guard)
      %i[exec spawn fork kill].each do |edge|
        Process.define_singleton_method(edge) { |*| refuse.call("Process.#{edge}") }
      end
      IO.define_singleton_method(:popen) { |*| refuse.call('IO.popen') }

      # Execute the original fetch/JSON.parse body. Only its HTTP edge is replaced.
      # The response contains exactly File.binread(index_path), without reserialization.
      response = Net::HTTPOK.new('1.1', '200', 'OK')
      response.define_singleton_method(:body) { index_bytes }
      fetch_urls = []
      Net::HTTP.define_singleton_method(:get_response) do |uri|
        refuse.call('unexpected/repeated HTTP request') unless fetch_urls.empty? && uri.to_s == 'http://127.0.0.1:18080/machines.json'
        fetch_urls << uri.to_s
        response
      end
      %i[start new].each do |edge|
        Net::HTTP.define_singleton_method(edge) { |*| refuse.call("Net::HTTP.#{edge}") }
      end
      machines_url = reader.derive_machines_json_url(reader.parse_httproot_from_cmdline)
      index = reader.fetch_machines_json(machines_url)
      reader.instance_variable_set(:@machines_json, index)
      machine = reader.pick_machine('node.test')
      abort 'old reader chose wrong machine' unless machine && machine.fetch('fqdn') == 'node.test' && machine.fetch('spin') == 'nixos'
      gens = machine.fetch('generations')
      abort 'expected the two generated generations' unless gens.length == 2 && [1, 2].all? { |id| gens.any? { |g| g.fetch('generation') == id } }

      expected = {
        1 => {
          'url' => 'http://127.0.0.1:18080/node.test/1',
          'boot_files' => { 'bzImage' => 'http://127.0.0.1:18080/node.test/1/bzImage', 'initrd' => 'http://127.0.0.1:18080/node.test/1/initrd' },
          'kernel_params' => ['httproot=http://127.0.0.1:18080/node.test/1/initrd', 'init=/var/lib/confctl-delivery/assets/system-1/init', 'console=ttyS0']
        },
        2 => {
          'url' => 'http://127.0.0.1:18080/node.test/2',
          'boot_files' => { 'bzImage' => 'http://127.0.0.1:18080/node.test/2/bzImage', 'initrd' => 'http://127.0.0.1:18080/node.test/2/initrd' },
          'kernel_params' => ['httproot=http://127.0.0.1:18080/node.test/2/initrd', 'init=/var/lib/confctl-delivery/assets/system-2/init', 'console=ttyS0']
        }
      }
      selections = [[nil, expected_current], ['1', 1]].map do |requested, id|
        generation = reader.pick_generation(machine, requested)
        abort 'old reader chose wrong generation' unless generation && generation.fetch('generation') == id
        abort 'wrong current marker' unless generation.fetch('current') == (id == expected_current)
        variant = reader.pick_variant(generation, nil)
        abort 'unexpected NixOS variant' unless variant.nil? && generation.fetch('variants') == []
        facts = expected.fetch(id)
        facts.each { |key, value| abort "old-reader #{key} differs" unless generation.fetch(key) == value }
        combined, joined = reader.selected_params(generation, variant)
        abort 'old combined array differs' unless combined == facts.fetch('kernel_params') + ['']
        abort 'old combined string differs' unless joined == (facts.fetch('kernel_params') + ['']).join(' ')
        abort 'reader changed generation params' unless generation.fetch('kernel_params') == facts.fetch('kernel_params')
        {
          'requested_generation' => requested, 'machine' => machine.fetch('fqdn'),
          'generation' => generation.fetch('generation'), 'current' => generation.fetch('current'),
          'variant' => variant, 'generation_url' => generation.fetch('url'),
          'boot_files' => generation.fetch('boot_files'), 'kernel_params' => generation.fetch('kernel_params'),
          'combined_params' => combined, 'joined_params' => joined
        }
      end
      abort 'external edge used' unless forbidden_calls.empty? && proc_stub_calls == 1 && fetch_urls == ['http://127.0.0.1:18080/machines.json']
      puts JSON.generate(
        'oracle_source_sha256' => source_sha, 'index_sha256' => Digest::SHA256.hexdigest(index_bytes),
        'index_bytes' => index_bytes.bytesize, 'machines_url' => machines_url,
        'fetch_stub_urls' => fetch_urls, 'proc_stub_calls' => proc_stub_calls,
        'forbidden_calls' => forbidden_calls, 'selections' => selections
      )
    OLD_NETBOOT_READER_ADAPTER
    actual = delivery_guest("old_reader_adapter = #{old_reader_adapter.dump}\n" + <<~'RUBY_GUEST')
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
      # Only an entirely absent oracle is a helper-only selection. A partial
      # binding fails, and a full selection executes this subcell on both runs.
      oracle_bindings = %w[oracleSrc oracleRuby oracleGemPath].map { |key| B.fetch(key) }
      abort 'partial original oracle binding' unless oracle_bindings.all?(&:nil?) || oracle_bindings.all? { |value| value.is_a?(String) && !value.empty? }
      oracle_root = fixture_case('old-netboot-reader') unless oracle_bindings.all?(&:nil?)
      old_read = lambda do |expected_current|
        if oracle_bindings.all?(&:nil?)
          next({ 'status' => 'unselected', 'reason' => 'original oracle bindings absent; mixed-direction acceptance remains open' })
        end
        path = File.join(http, 'machines.json')
        before_bytes = File.binread(path)
        before_digest = Digest::SHA256.hexdigest(before_bytes)
        oracle_env = { 'GEM_HOME' => B.fetch('oracleGemPath'), 'GEM_PATH' => B.fetch('oracleGemPath') }
        native = fixture_run(oracle_root,
                             ['-e', old_reader_adapter, B.fetch('oracleSrc') + '/nix/modules/confctl/kexec-netboot/kexec-netboot.rb', path, expected_current.to_s],
                             executable: B.fetch('oracleRuby'), env: oracle_env)
        # Raw oracle streams/status stay separate from the installed builder's.
        abort "old H4 reader failed: #{native.inspect}" unless native.fetch('exit') == 0 && native.fetch('signal').nil? && native.fetch('err').empty?
        after_bytes = File.binread(path)
        after_digest = Digest::SHA256.hexdigest(after_bytes)
        abort 'old reader changed generated index bytes' unless before_bytes == after_bytes && before_digest == after_digest
        facts = JSON.parse(native.fetch('out'))
        abort 'old reader consumed different index bytes' unless facts.values_at('index_sha256', 'index_bytes') == [before_digest, before_bytes.bytesize]
        { 'status' => 'executed', 'producer' => B.fetch('builder'), 'native' => native,
          'before_sha256' => before_digest, 'after_sha256' => after_digest, 'facts' => facts }
      end
      old_first = old_read.call(1)
      expected_assets = { 'pxelinux.0' => 'asset:pxelinux.0', 'ldlinux.c32' => 'asset:ldlinux.c32', 'libcom32.c32' => 'asset:libcom32.c32', 'libutil.c32' => 'asset:libutil.c32', 'memdisk' => 'asset:memdisk', 'menu.c32' => 'asset:menu.c32', 'reboot.c32' => 'asset:reboot.c32' }
      copied_assets = lambda do |current_tftp|
        bytes = expected_assets.keys.to_h do |name|
          path = File.join(current_tftp, name)
          abort "nonregular/symlink boot asset: #{name}" unless File.lstat(path).file? && !File.symlink?(path)
          [name, File.binread(path)]
        end
        abort 'boot asset bytes differ' unless bytes == expected_assets
        bytes
      end
      first_assets = copied_assets.call(tftp)
      old_roots = [File.realpath(http), File.realpath(tftp)]
      index = JSON.parse(File.read(File.join(http, 'machines.json')))
      machine = index.fetch('machines').fetch(0)
      abort 'new index contract' unless machine.fetch('fqdn') == 'node.test' && machine.fetch('generations').all? { |g| g.fetch('generation').is_a?(Integer) && g.fetch('boot_files').values.all? { |url| url.start_with?('http://127.0.0.1:18080/') } }
      first_current = machine.fetch('generations').find { |g| g.fetch('current') }.fetch('generation')
      File.unlink(current)
      File.symlink(File.join(stores, '2'), current)
      out, err, status = Open3.capture3(B.fetch('builder'))
      abort "second builder failed: #{out} #{err}" unless status.success?
      old_final = old_read.call(2)
      final_assets = copied_assets.call(tftp)
      final = JSON.parse(File.read(File.join(http, 'machines.json'))).fetch('machines').fetch(0)
      params = final.fetch('generations').find { |g| g.fetch('current') }.fetch('kernel_params')
      menu = File.read(File.join(tftp, 'pxeserver/machines/node.test/auto.cfg'))
      puts JSON.generate('old_reader_first' => old_first, 'old_reader_final' => old_final, 'first_assets' => first_assets, 'final_assets' => final_assets, 'first_current' => first_current, 'final_current' => final.fetch('generations').find { |g| g.fetch('current') }.fetch('generation'), 'params' => params, 'menu_has_payload' => menu.include?('boot/node.test/2/bzImage') && menu.include?('boot/node.test/2/initrd') && menu.include?('console=ttyS0'), 'current_links' => [File.readlink(File.join(http, 'node.test/current')), File.readlink(File.join(tftp, 'boot/node.test/current'))], 'payload' => File.read(File.join(http, 'node.test/2/bzImage')), 'inputs_info' => final.fetch('generations').find { |g| g.fetch('current') }.fetch('inputs_info'), 'old_roots_removed' => old_roots.none? { |path| File.exist?(path) }, 'outside_untouched' => File.read(File.join(marker, 'sentinel')))
    RUBY_GUEST
    expect(actual.values_at('first_current', 'final_current', 'menu_has_payload', 'old_roots_removed', 'outside_untouched')).to eq([1, 2, true, true, 'keep'])
    expected_assets = { 'pxelinux.0' => 'asset:pxelinux.0', 'ldlinux.c32' => 'asset:ldlinux.c32', 'libcom32.c32' => 'asset:libcom32.c32', 'libutil.c32' => 'asset:libutil.c32', 'memdisk' => 'asset:memdisk', 'menu.c32' => 'asset:menu.c32', 'reboot.c32' => 'asset:reboot.c32' }
    expect(actual.values_at('first_assets', 'final_assets')).to eq([expected_assets, expected_assets])
    expect(actual.fetch('payload')).to eq('payload:2:bzImage')
    expected_params = [
      'httproot=http://127.0.0.1:18080/node.test/2/initrd',
      "init=#{DELIVERY_BINDINGS.fetch('root')}/assets/system-2/init",
      'console=ttyS0'
    ]
    expect(actual.fetch('params')).to eq(expected_params)
    expect(actual.fetch('current_links')).to eq(%w[2 2])
    expect(actual.fetch('inputs_info')).to eq('os' => { 'rev' => 'input2' })
    old_first, old_final = actual.values_at('old_reader_first', 'old_reader_final')
    expect(old_first.fetch('status')).to eq(old_final.fetch('status'))
    if old_first.fetch('status') == 'executed'
      expect(old_first.fetch('facts').fetch('selections').map { |row| row.fetch('generation') }).to eq([1, 1])
      expect(old_final.fetch('facts').fetch('selections').map { |row| row.fetch('generation') }).to eq([2, 1])
      [old_first, old_final].each do |result|
        expect(result.fetch('before_sha256')).to eq(result.fetch('after_sha256'))
        expect(result.fetch('facts').fetch('forbidden_calls')).to eq([])
        expect(result.fetch('native').values_at('exit', 'signal', 'err')).to eq([0, nil, ''])
      end
    else
      expect(old_first).to eq('status' => 'unselected', 'reason' => 'original oracle bindings absent; mixed-direction acceptance remains open')
      expect(old_final).to eq(old_first)
      warn 'Builder -> old reader subcell unselected; original builder checks still ran.'
    end
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
