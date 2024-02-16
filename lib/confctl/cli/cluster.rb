require_relative 'command'
require_relative '../hook'
require 'json'
require 'rainbow'
require 'tty-pager'
require 'tty-progressbar'
require 'tty-spinner'

module ConfCtl::Cli
  class Cluster < Command
    ConfCtl::Hook.register :cluster_deploy

    def list
      if opts[:list]
        prefix = 'cluster.<name>.'
        nix = ConfCtl::Nix.new

        puts 'name'

        nix.module_options.each do |opt|
          next unless opt['name'].start_with?(prefix)

          puts opt['name'][prefix.length..]
        end

        return
      end

      list_machines(select_machines_with_managed(args[0]))
    end

    def build
      machines = select_machines(args[0]).managed

      raise 'No machines to build' if machines.empty?

      ask_confirmation! do
        puts 'The following machines will be built:'
        list_machines(machines)
      end

      do_build(machines)
    end

    def deploy
      machines = select_machines(args[0]).managed
      action = args[1] || 'switch'

      raise GLI::BadCommandLine, "invalid action '#{action}'" unless %w[boot switch test dry-activate].include?(action)

      if opts[:reboot]
        raise GLI::BadCommandLine, '--reboot can be used only with switch-action boot' if action != 'boot'

        parse_wait_online
      end

      raise 'No machines to deploy' if machines.empty?

      ask_confirmation! do
        puts 'The following machines will be deployed:'
        list_machines(machines)
        puts
        puts "Generation: #{opts[:generation] || 'new build'}"
        puts "Target action: #{action}#{opts[:reboot] ? ' + reboot' : ''}"
      end

      ConfCtl::Hook.call(:cluster_deploy, kwargs: {
        machines:,
        generation: opts[:generation],
        action: opts[:action],
        opts:
      })

      host_generations =
        if opts[:generation]
          find_generations(machines, opts[:generation])
        else
          do_build(machines)
        end

      nix = ConfCtl::Nix.new(show_trace: opts['show-trace'])

      if opts['one-by-one']
        deploy_one_by_one(machines, host_generations, nix, action)
      else
        deploy_in_bulk(machines, host_generations, nix, action)
      end
    end

    def health_check
      machines = select_machines(args[0]).managed.select do |_host, machine|
        machine.health_checks.any?
      end
      raise 'No machines to check or no health checks configured' if machines.empty?

      checks = machines.health_checks

      ask_confirmation! do
        puts 'Health checks will be run on the following machines:'

        list_machines(machines, prepend_cols: %w[checks])
        puts
        puts "#{checks.length} checks in total"
        puts
      end

      return unless run_health_checks(machines, checks).any?

      raise 'Health checks failed'
    end

    def status
      machines = select_machines(args[0]).managed
      raise 'No machines to check' if machines.empty?

      ask_confirmation! do
        if opts[:generation]
          puts 'The following machines will be checked:'
        else
          puts 'The following machines will be built and then checked:'
        end

        list_machines(machines)
        puts
        puts "Generation: #{opts[:generation] || 'new build'}"
      end

      statuses = machines.transform_values do |machine|
        ConfCtl::MachineStatus.new(machine)
      end

      # Evaluate toplevels
      if opts[:generation] == 'none'
        host_generations = nil
      elsif opts[:generation]
        host_generations = find_generations(machines, opts[:generation])

        # Ignore statuses when no generation was found
        statuses.delete_if do |host, _st|
          !host_generations.has_key?(host)
        end
      else
        host_generations = do_build(machines)
        puts
      end

      # Assign configured toplevel and swpins
      if host_generations
        host_generations.each do |host, gen|
          statuses[host].target_toplevel = gen.toplevel
          statuses[host].target_swpin_specs = gen.swpin_specs
        end
      else
        # We're not comparing a system generation, only configured swpins
        ConfCtl::Swpins::ClusterNameList.new(machines:).each do |cn|
          cn.parse

          statuses[cn.name].target_swpin_specs = cn.specs
        end
      end

      # Check runtime status
      tw = ConfCtl::ParallelExecutor.new(machines.length)

      statuses.each_value do |st|
        tw.add do
          st.query(toplevel: opts[:generation] != 'none')
        end
      end

      tw.run

      # Collect all swpins
      swpins = []

      statuses.each_value do |st|
        st.target_swpin_specs.each_key do |name|
          swpins << name unless swpins.include?(name)
        end

        st.evaluate
      end

      # Render results
      cols = %w[host online uptime status generations] + swpins
      rows = []

      statuses.each do |host, st|
        build_generations = ConfCtl::Generation::BuildList.new(host)

        row = {
          'host' => host,
          'online' => st.online? && Rainbow('yes').green,
          'uptime' => st.uptime && format_duration(st.uptime),
          'status' => st.status ? Rainbow('ok').green : Rainbow('outdated').red,
          'generations' => "#{build_generations.count}:#{st.generations && st.generations.count}"
        }

        swpins.each do |name|
          swpin_state = st.swpins_state[name]

          row[name] =
            if swpin_state
              Rainbow(swpin_state.current_version).color(
                swpin_state.uptodate? ? :green : :red
              )
            end
        end

        rows << row
      end

      OutputFormatter.print(rows, cols, layout: :columns, color: use_color?)
    end

    def changelog
      compare_swpins do |io, _host, status, sw_name, spec|
        s = spec.string_changelog_info(
          opts[:downgrade] ? :downgrade : :upgrade,
          status.swpins_info[sw_name],
          color: use_color?,
          verbose: opts[:verbose],
          patch: opts[:patch]
        )
      rescue ConfCtl::Error => e
        io.puts e.message
      else
        io.puts(s || 'no changes')
      end
    end

    def diff
      compare_swpins do |io, _host, status, sw_name, spec|
        s = spec.string_diff_info(
          opts[:downgrade] ? :downgrade : :upgrade,
          status.swpins_info[sw_name],
          color: use_color?
        )
      rescue ConfCtl::Error => e
        io.puts e.message
      else
        io.puts(s || 'no changes')
      end
    end

    def test_connection
      machines = select_machines_with_managed(args[0])
      raise 'No machines to test' if machines.empty?

      ask_confirmation! do
        puts 'Test SSH connection to the following machines:'
        list_machines(machines)
      end

      succeeded = []
      failed = []

      machines.each do |host, machine|
        mc = ConfCtl::MachineControl.new(machine)

        begin
          mc.test_connection
          succeeded << host
        rescue TTY::Command::ExitError => e
          puts "Unable to connect to #{host}: #{e.message}"
          puts
          failed << host
        end
      end

      puts
      puts "Result: #{succeeded.length} successful, #{failed.length} failed"
      puts
      puts 'Failed machines:'
      failed.each { |host| puts "  #{host}" }
    end

    def ssh
      machines = select_machines_with_managed(args[0])
      raise 'No machines to ssh to' if machines.empty?

      if opts['input-string'] && opts['input-file']
        raise GLI::BadCommandLine, 'use one of --input-string or --input-file'
      end

      if args.length == 1
        raise GLI::BadCommandLine, 'missing command' unless machines.length == 1

        run_ssh_interactive(machines)
        return

      end

      run_ssh_command(machines, args[1..])
    end

    def cssh
      machines = select_machines_with_managed(args[0])
      raise 'No machines to open cssh to' if machines.empty?

      ask_confirmation! do
        puts 'Open cssh to the following machines:'
        list_machines(machines)
      end

      nix = ConfCtl::Nix.new

      cssh = [
        'cssh',
        '-l', 'root'
      ]

      machines.each_value do |machine|
        cssh << machine.target_host
      end

      nix.run_command_in_shell(
        packages: ['perlPackages.AppClusterSSH'],
        command: cssh.join(' ')
      )
    end

    protected

    attr_reader :wait_online

    def deploy_in_bulk(machines, host_generations, nix, action)
      skipped_copy = []
      skipped_activation = []

      if opts[:interactive]
        host_generations.each do |host, gen|
          if copy_to_host(nix, host, machines[host], gen.toplevel) == :skip
            puts Rainbow("Skipping #{host}").yellow
            skipped_copy << host
          end
        end
      else
        concurrent_copy(machines, host_generations, nix)
      end

      return if opts['copy-only']

      host_generations.each do |host, gen|
        if skipped_copy.include?(host)
          puts Rainbow("Copy to #{host} was skipped, skipping activation as well").yellow
          skipped_activation << host
          next
        end

        if deploy_to_host(nix, host, machines[host], gen.toplevel, action) == :skip
          puts Rainbow("Skipping #{host}").yellow
          skipped_activation << host
          next
        end

        puts if opts[:interactive]
      end

      if opts[:reboot]
        host_generations.each_key do |host|
          if skipped_activation.include?(host)
            puts Rainbow("Activation on #{host} was skipped, skipping reboot as well").yellow
            next
          end

          if reboot_host(host, machines[host]) == :skip
            puts "Skipping #{host}"
            next
          end

          puts if opts[:interactive]
        end
      end

      return unless opts['health-checks'] && need_health_checks?(action)

      check_machines = machines.select do |host, _machine|
        if skipped_activation.include?(host)
          puts Rainbow("Activation on #{host} was skipped, skipping health checks as well").yellow
          false
        else
          true
        end
      end

      run_health_check_loop(check_machines)

      puts if opts[:interactive]
    end

    def deploy_one_by_one(machines, host_generations, nix, action)
      host_generations.each do |host, gen|
        machine = machines[host]

        if copy_to_host(nix, host, machine, gen.toplevel) == :skip
          puts Rainbow("Skipping #{host}").yellow
          next
        end

        next if opts['copy-only']

        if deploy_to_host(nix, host, machine, gen.toplevel, action) == :skip
          puts Rainbow("Skipping #{host}").yellow
          next
        end

        if opts[:reboot] && reboot_host(host, machine) == :skip
          puts Rainbow("Skipping #{host}").yellow
          next
        end

        if opts['health-checks'] && need_health_checks?(action)
          run_health_check_loop(ConfCtl::MachineList.from_machine(machine))
        end

        puts if opts[:interactive]
      end
    end

    def copy_to_host(nix, host, machine, toplevel)
      puts Rainbow("Copying configuration to #{host} (#{machine.target_host})").yellow

      return :skip if opts[:interactive] && !ask_confirmation(always: true)

      LogView.open(
        header: "#{Rainbow('Copying to').bright} #{host}\n",
        title: Rainbow('Live view').bright
      ) do |lw|
        pb = TTY::ProgressBar.new(
          'Copying [:bar] :current/:total (:percent)',
          width: 80
        )

        ret = nix.copy(machine, toplevel) do |i, n, path|
          lw << "[#{i}/#{n}] #{path}"

          lw.sync_console do
            pb.update(total: n) if pb.total != n
            pb.advance
          end
        end

        raise "Error while copying system to #{host}" unless ret
      end

      true
    end

    def concurrent_copy(machines, host_generations, nix)
      LogView.open(
        header: "#{Rainbow("Copying to #{host_generations.length} machines").bright}\n",
        title: Rainbow('Live view').bright
      ) do |lw|
        multibar = TTY::ProgressBar::Multi.new(
          'Copying [:bar] :current/:total (:percent)',
          width: 80
        )
        executor = ConfCtl::ParallelExecutor.new(opts['max-concurrent-copy'])

        host_generations.each do |host, gen|
          pb = multibar.register(
            "#{host} [:bar] :current/:total (:percent)"
          )

          executor.add do
            ret = nix.copy(machines[host], gen.toplevel) do |i, n, path|
              lw << "#{host}> [#{i}/#{n}] #{path}"

              lw.sync_console do
                if pb.total != n
                  pb.update(total: n)
                  multibar.top_bar.resume if multibar.top_bar.done?
                  multibar.top_bar.update(total: multibar.total)
                end

                pb.advance
              end
            end

            if !ret
              lw.sync_console do
                pb.format = "#{host}: error occurred"
                pb.advance
              end
            elsif pb.total.nil?
              lw.sync_console do
                pb.format = "#{host}: nothing to do"
                pb.advance
              end
            end

            ret ? nil : host
          end
        end

        retvals = executor.run
        failed = retvals.compact

        raise "Copy failed to: #{failed.join(', ')}" if failed.any?
      end
    end

    def deploy_to_host(nix, host, machine, toplevel, action)
      LogView.open_with_logger(
        header: "#{Rainbow('Deploying to').bright} #{Rainbow(host).yellow}\n",
        title: Rainbow('Live view').bright,
        size: :auto,
        reserved_lines: 10
      ) do |lw|
        if opts['dry-activate-first']
          lw.sync_console do
            puts Rainbow(
              "Trying to activate configuration on #{host} " \
              "(#{machine.target_host})"
            ).yellow
          end

          raise "Error while activating configuration on #{host}" unless nix.activate(machine, toplevel, 'dry-activate')
        end

        lw.sync_console do
          puts Rainbow(
            "Activating configuration on #{host} (#{machine.target_host}): " \
            "#{action}"
          ).yellow
        end

        return :skip if opts[:interactive] && !ask_confirmation(always: true)

        raise "Error while activating configuration on #{host}" unless nix.activate(machine, toplevel, action)

        if %w[boot switch].include?(action) && !nix.set_profile(machine, toplevel)
          raise "Error while setting profile on #{host}"
        end
      end
    end

    def reboot_host(host, machine)
      if machine.localhost?
        puts Rainbow("Skipping reboot of #{host} as it is localhost").yellow
        return :skip
      end

      puts Rainbow("Rebooting #{host} (#{machine.target_host})").yellow

      return :skip if opts[:interactive] && !ask_confirmation(always: true)

      m = ConfCtl::MachineControl.new(machine)

      if wait_online == :nowait
        m.reboot
      else
        since = Time.now
        spinner = nil

        secs = m.reboot_and_wait(
          timeout: wait_online == :wait ? nil : wait_online
        ) do |state, timeleft|
          if state == :reboot
            spinner = TTY::Spinner.new(
              ":spinner Waiting for #{host} (:seconds s)",
              format: :classic
            )
            spinner.auto_spin
          elsif state == :is_up
            spinner.success('up')
            next
          end

          if wait_online == :wait
            spinner.update(seconds: (Time.now - since).round)
          else
            spinner.update(seconds: timeleft.round)
          end
        end

        puts Rainbow("#{host} (#{machine.target_host}) is online (took #{secs.round(1)}s to reboot)").yellow
      end
    end

    def run_health_check_loop(machines)
      all_checks = machines.health_checks
      return if all_checks.empty?

      run_checks = all_checks

      if opts[:interactive]
        if machines.length > 1
          puts Rainbow("Running #{run_checks.length} health checks on #{machines.length} machines").yellow
        else
          puts Rainbow("Running #{run_checks.length} health checks on #{machines.get_one}").yellow
        end

        return unless ask_confirmation(always: true)
      end

      loop do
        failed = run_health_checks(machines, run_checks)
        return if failed.empty?

        if opts[:interactive]
          puts 'Health checks have failed'

          answer = ask_action(
            options: {
              'c' => 'Continue',
              'r' => 'Retry all',
              'f' => 'Retry failed',
              'a' => 'Abort'
            },
            default: 'a'
          )

          case answer
          when 'c'
            return
          when 'r'
            run_checks = all_checks
            next
          when 'f'
            run_checks = failed
            next
          when 'a'
            raise 'Aborting'
          end

        elsif opts['keep-going']
          puts 'Health checks have failed, going on'
          return

        else
          raise 'Health checks have failed'
        end
      end
    end

    # @return [Array<HealthChecks::Base>] failed checks
    def run_health_checks(machines, checks = nil)
      checks ||= machines.health_checks

      tw = ConfCtl::ParallelExecutor.new(opts['max-jobs'] || 5)

      header =
        if machines.length > 1
          Rainbow("Running health checks on #{machines.length} machines").bright
        else
          Rainbow('Running health checks on ').bright + Rainbow(machines.get_one.to_s).yellow
        end

      header << "\n" << Rainbow('Full log: ').bright << ConfCtl::Logger.relative_path << "\n"

      LogView.open(
        header:,
        title: Rainbow('Live view').bright,
        size: :auto,
        reserved_lines: 10
      ) do |lw|
        pb = TTY::ProgressBar.new(
          'Checks [:bar] :current/:total (:percent)',
          width: 80,
          total: checks.length
        )

        checks.each_with_index do |check, i|
          tw.add do
            prefix = "[#{i + 1}/#{checks.length}] #{check.machine}> "
            lw << "#{prefix}#{check.description}"

            check.run do |attempt, _errors|
              lw << "#{prefix}failed ##{attempt}: #{check.message}"
              ConfCtl::Logger.keep
            end

            lw << if check.successful?
                    "#{prefix}succeeded"
                  else
                    "#{prefix}error: #{check.message}"
                  end

            lw.sync_console do
              pb.advance
            end
          end
        end

        tw.run
        lw.flush
      end

      successful = []
      failed = []

      checks.each do |check|
        if check.successful?
          successful << check
        else
          failed << check
        end
      end

      puts "#{successful.length} checks passed, #{failed.length} failed"
      puts

      failed_by_machine = {}

      failed.each do |check|
        failed_by_machine[check.machine] ||= []
        failed_by_machine[check.machine] << check
      end

      failed_by_machine.each do |machine, checks|
        puts "#{machine}: #{checks.length} failures"

        checks.each do |check|
          puts "  #{check.message}"
        end

        puts
      end

      failed
    end

    def need_health_checks?(action)
      %w[switch test].include?(action) || (action == 'boot' && opts[:reboot])
    end

    def run_ssh_interactive(machines)
      raise ArgumentError if machines.length != 1

      ask_confirmation! do
        puts 'Open interactive shell on the following machine:'
        list_machines(machines)
      end

      machines.each_value do |machine|
        mc = ConfCtl::MachineControl.new(machine)
        mc.interactive_shell
        return
      end
    end

    def run_ssh_command(machines, cmd)
      ask_confirmation! do
        puts 'Run command over SSH on the following machines:'
        list_machines(machines)
        puts
        puts "Command: #{cmd.map(&:inspect).join(' ')}"
      end

      if opts[:parallel]
        run_ssh_command_in_parallel(machines, cmd)
      else
        run_ssh_command_one_by_one(machines, cmd)
      end
    end

    def run_ssh_command_one_by_one(machines, cmd)
      aggregate = opts[:aggregate]
      results = {}

      machines.each do |host, machine|
        mc = ConfCtl::MachineControl.new(machine)

        begin
          puts "#{host}:" unless aggregate

          result = run_ssh_command_on_machine(mc, cmd)

          if aggregate
            results[host] = result
          else
            puts result.out
          end
        rescue TTY::Command::ExitError => e
          if aggregate
            results[host] = e
          else
            puts e.message.to_s
          end
        end

        puts unless aggregate
      end

      return unless aggregate

      process_aggregated_results(results)
    end

    def run_ssh_command_in_parallel(machines, cmd)
      aggregate = opts[:aggregate]
      results = {}
      tw = ConfCtl::ParallelExecutor.new(machines.length)

      LogView.open_with_logger(
        header: Rainbow('Executing').bright + " #{cmd.join(' ')}\n",
        title: Rainbow('Live view').bright,
        size: :auto,
        reserved_lines: 10
      ) do |lw|
        pb = TTY::ProgressBar.new(
          'Command [:bar] :current/:total (:percent)',
          width: 80,
          total: machines.length
        )

        machines.each do |host, machine|
          tw.add do
            mc = ConfCtl::MachineControl.new(machine)

            begin
              result = run_ssh_command_on_machine(mc, cmd)
              results[host] = result
            rescue TTY::Command::ExitError => e
              results[host] = e
            end

            lw.sync_console { pb.advance }
          end
        end

        tw.run
        lw.flush
      end

      if aggregate
        process_aggregated_results(results)
        return
      end

      results.each do |host, result|
        puts "#{host}:"
        puts result.out
        puts
      end
    end

    def run_ssh_command_on_machine(mc, cmd)
      cmd_opts = { err: :out }

      if opts['input-string']
        cmd_opts[:input] = opts['input-string']
      elsif opts['input-file']
        cmd_opts[:in] = opts['input-file']
      end

      mc.execute(*cmd, **cmd_opts)
    end

    def process_aggregated_results(results)
      groups = {}

      results.each do |host, result|
        key = [result.exit_status, result.out]
        groups[key] ||= []
        groups[key] << host
      end

      groups.each do |key, hosts|
        exit_status, out = key
        puts "#{hosts.sort.join(', ')}:"
        puts "Exit status: #{exit_status}"
        puts out
        puts
      end
    end

    def find_generations(machines, generation_name)
      host_generations = {}
      missing_hosts = []

      machines.each_key do |host|
        list = ConfCtl::Generation::BuildList.new(host)

        gen =
          if generation_name == 'current'
            list.current
          else
            list[generation_name]
          end

        if gen
          host_generations[host] = gen
        else
          missing_hosts << host
        end
      end

      raise 'No generation found' if host_generations.empty?

      if missing_hosts.any?
        ask_confirmation! do
          puts "Generation '#{generation_name}' was not found on the following hosts:"
          missing_hosts.each { |host| puts "  #{host}" }
          puts
          puts 'These hosts will be ignored.'
        end
      end

      host_generations
    end

    def do_build(machines)
      nix = ConfCtl::Nix.new(
        show_trace: opts['show-trace'],
        max_jobs: opts['max-jobs']
      )
      host_swpin_paths = {}

      autoupdate_swpins(machines)
      host_swpin_specs = check_swpins(machines)

      raise 'one or more swpins need to be updated' unless host_swpin_specs

      machines.each do |host, d|
        puts Rainbow("Evaluating swpins for #{host}...").bright
        host_swpin_paths[host] = nix.eval_host_swpins(host).update(d.nix_paths)
      end

      grps = swpin_build_groups(host_swpin_paths)
      puts
      puts "Machines will be built in #{grps.length} groups"
      puts
      host_generations = {}
      time = Time.now

      puts "#{Rainbow('Build log:').yellow} #{Rainbow(ConfCtl::Logger.path).cyan}"
      puts

      grps.each_with_index do |grp, i|
        hosts, swpin_paths = grp

        built_generations = do_build_group(
          i,
          grps.length,
          hosts,
          swpin_paths,
          host_swpin_specs,
          nix,
          time
        )

        host_generations.update(built_generations)
      end

      generation_hosts = {}

      host_generations.each do |host, gen|
        generation_hosts[gen.name] ||= []
        generation_hosts[gen.name] << host
      end

      puts
      puts Rainbow('Built generations:').bright
      generation_hosts.each do |gen, hosts|
        puts Rainbow(gen).cyan
        hosts.each { |host| puts "  #{host}" }
      end

      host_generations
    end

    def do_build_group(i, n, hosts, swpin_paths, host_swpin_specs, nix, time)
      puts Rainbow('Building machines').bright
      hosts.each { |h| puts "  #{h}" }
      puts 'with swpins'
      swpin_paths.each { |k, v| puts "  #{k}=#{v}" }

      header = '' \
        << Rainbow('Command:').bright \
        << " #{format_command(10)}" \
        << "\n" \
        << Rainbow('Build group:').bright \
        << " #{i + 1}/#{n} (#{hosts.length} machines)" \
        << "\n" \
        << Rainbow('Full log:   ').bright \
        << " #{ConfCtl::Logger.relative_path}" \
        << "\n\n"

      LogView.open_with_logger(
        header:,
        title: Rainbow('Live view').bright,
        size: :auto,
        reserved_lines: 10
      ) do |lw|
        multibar = TTY::ProgressBar::Multi.new(
          'nix-build [:bar] :current/:total (:percent)',
          width: 80
        )

        build_pb = multibar.register(
          'Building [:bar] :current/:total (:percent)'
        )

        fetch_pb = multibar.register(
          'Fetching [:bar] :current/:total (:percent)'
        )

        built_generations = nix.build_toplevels(
          hosts:,
          swpin_paths:,
          time:,
          host_swpin_specs:
        ) do |type, _i, n, _path|
          if type == :build
            lw.sync_console do
              build_pb.update(total: n) if n > 0 && build_pb.total.nil?
              build_pb.advance
            end
          elsif type == :fetch
            lw.sync_console do
              fetch_pb.update(total: n) if n > 0 && fetch_pb.total.nil?
              fetch_pb.advance
            end
          end

          if build_pb.total && fetch_pb.total && multibar.top_bar.total.nil?
            lw.sync_console do
              multibar.top_bar.update(total: multibar.total)
            end
          end
        end

        built_generations
      end
    end

    def autoupdate_swpins(machines)
      puts Rainbow('Running swpins auto updates...').bright
      channels_update = []
      any_updated = false

      core = ConfCtl::Swpins::Core.get

      core.channels.each do |c|
        channels_update << c unless channels_update.include?(c)
      end

      cluster_names = ConfCtl::Swpins::ClusterNameList.new(machines:)

      cluster_names.each do |cn|
        cn.parse

        cn.channels.each do |c|
          channels_update << c unless channels_update.include?(c)
        end
      end

      channels_update.each do |c|
        updated = false

        c.specs.each do |name, s|
          next unless s.auto_update?

          puts " updating #{c.name}.#{name}"
          s.prefetch_update
          updated = true
        end

        if updated
          c.save
          any_updated = true
        end
      end

      core_updated = false

      core.specs.each do |name, s|
        next unless !s.from_channel? && s.auto_update?

        puts " updating #{core.name}.#{name}"
        s.prefetch_update
        core_updated = true
      end

      if core_updated
        core.save
        core.pre_evaluate
      end

      cluster_names.each do |cn|
        updated = false

        cn.specs.each do |name, s|
          next unless !s.from_channel? && s.auto_update?

          puts " updating #{cn.name}.#{name}"
          s.prefetch_update
          updated = true
        end

        if updated
          cn.save
          any_updated = true
        end
      end

      return unless any_updated || core_updated

      ConfCtl::Swpins::ChannelList.refresh
    end

    def check_swpins(machines)
      ret = {}
      valid = true

      puts Rainbow('Checking core swpins...').bright

      ConfCtl::Swpins::Core.get.specs.each do |name, s|
        puts "  #{name} ... " +
             (s.valid? ? Rainbow('ok').green : Rainbow('needs update').cyan)
        valid = false unless s.valid?
      end

      ConfCtl::Swpins::ClusterNameList.new(machines:).each do |cn|
        cn.parse

        puts Rainbow("Checking swpins for #{cn.name}...").bright

        cn.specs.each do |name, s|
          puts "  #{name} ... " +
               (s.valid? ? Rainbow('ok').green : Rainbow('needs update').cyan)
          valid = false unless s.valid?
        end

        ret[cn.name] = cn.specs
      end

      valid ? ret : false
    end

    def swpin_build_groups(host_swpins)
      ret = []
      all_swpins = host_swpins.values.uniq

      all_swpins.each do |swpins|
        hosts = []

        host_swpins.each do |host, host_swpins|
          hosts << host if swpins == host_swpins
        end

        ret << [hosts, swpins]
      end

      ret
    end

    def compare_swpins
      machines = select_machines(args[0]).managed

      ask_confirmation! do
        puts 'Compare swpins on the following machines:'
        list_machines(machines)
        puts
        puts "Generation: #{opts[:generation] || 'current configuration'}"
      end

      statuses = machines.transform_values do |machine|
        ConfCtl::MachineStatus.new(machine)
      end

      if opts[:generation]
        host_generations = find_generations(machines, opts[:generation])

        host_generations.each do |host, gen|
          statuses[host].target_swpin_specs = gen.swpin_specs
        end

        # Ignore statuses when no generation was found
        statuses.delete_if do |host, _st|
          !host_generations.has_key?(host)
        end
      else
        ConfCtl::Swpins::ClusterNameList.new(machines:).each do |cn|
          cn.parse

          statuses[cn.name].target_swpin_specs = cn.specs
        end
      end

      TTY::Pager.page(enabled: use_pager?) do |io|
        statuses.each do |host, st|
          st.query(toplevel: false, generations: false)
          st.evaluate

          unless st.online?
            io.puts "#{host} is offline"
            next
          end

          st.target_swpin_specs.each do |name, spec|
            next if args[1] && !ConfCtl::Pattern.match?(args[1], name)

            if st.swpins_info[name]
              io.puts "#{host} @ #{name}:"

              yield(io, host, st, name, spec)
            else
              io.puts "#{host} @ #{name} in unknown state"
            end

            io.puts ''
          end
        end
      end
    end

    def parse_wait_online
      @wait_online =
        case opts['wait-online']
        when 'wait'
          :wait
        when 'nowait'
          :nowait
        when /^\d+$/
          opts['wait-online'].to_i
        else
          raise GLI::BadCommandLine, 'invalid value of --wait-online'
        end
    end

    def format_command(reserved_cols = 0)
      cmd = "#{$0.split('/').last} #{ARGV.join(' ')}"
      _, cols = IO.console.winsize
      max_length = cols - reserved_cols

      if cmd.length > max_length
        "#{cmd[0..(max_length - 4)]}..."
      else
        cmd
      end
    end

    def format_duration(interval)
      {
        'd' => 24 * 60 * 60,
        'h' => 60 * 60,
        'm' => 60,
        's' => 1
      }.each do |unit, n|
        return "#{(interval / n.to_f).round(1)}#{unit}" if interval > n
      end

      raise ArgumentError, "invalid time duration '#{interval}'"
    end
  end
end
