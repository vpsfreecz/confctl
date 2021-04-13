require_relative 'command'
require 'json'
require 'rainbow'
require 'tty-pager'
require 'tty-spinner'

module ConfCtl::Cli
  class Cluster < Command
    def list
      deps = ConfCtl::Deployments.new(show_trace: opts['show-trace'])
      selected = select_deployments(args[0])

      managed =
        case opts[:managed]
        when 'y', 'yes'
          selected.managed
        when 'n', 'no'
          selected.unmanaged
        when 'a', 'all'
          selected
        else
          selected.managed
        end

      list_deployments(managed)
    end

    def build
      deps = select_deployments(args[0]).managed

      ask_confirmation! do
        puts "The following deployments will be built:"
        list_deployments(deps)
      end

      do_build(deps)
    end

    def deploy
      deps = select_deployments(args[0]).managed
      action = args[1] || 'switch'

      unless %w(boot switch test dry-activate).include?(action)
        raise GLI::BadCommandLine, "invalid action '#{action}'"
      end

      if opts[:reboot]
        if action != 'boot'
          raise GLI::BadCommandLine, '--reboot can be used only with switch-action boot'
        end

        parse_wait_online
      end

      ask_confirmation! do
        puts "The following deployments will be deployed:"
        list_deployments(deps)
        puts
        puts "Generation: #{opts[:generation] || 'new build'}"
        puts "Target action: #{action}#{opts[:reboot] ? ' + reboot' : ''}"
      end

      host_generations =
        if opts[:generation]
          find_generations(deps, opts[:generation])
        else
          do_build(deps)
        end

      nix = ConfCtl::Nix.new(show_trace: opts['show-trace'])

      if opts['one-by-one']
        deploy_one_by_one(deps, host_generations, nix, action)
      else
        deploy_in_bulk(deps, host_generations, nix, action)
      end
    end

    def status
      deps = select_deployments(args[0]).managed

      ask_confirmation! do
        if opts[:generation]
          puts "The following deployments will be checked:"
        else
          puts "The following deployments will be built and then checked:"
        end

        list_deployments(deps)
        puts
        puts "Generation: #{opts[:generation] || 'new build'}"
      end

      statuses = Hash[deps.map do |host, dep|
        [host, ConfCtl::MachineStatus.new(dep)]
      end]

      # Evaluate toplevels
      if opts[:generation] == 'none'
        host_generations = nil
      elsif opts[:generation]
        host_generations = find_generations(deps, opts[:generation])

        # Ignore statuses when no generation was found
        statuses.delete_if do |host, st|
          !host_generations.has_key?(host)
        end
      else
        host_generations = do_build(deps)
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
        channels = ConfCtl::Swpins::ChannelList.new
        channels.each(&:parse)

        ConfCtl::Swpins::ClusterNameList.new(
          deployments: deps,
          channels: channels,
        ).each do |cn|
          cn.parse

          statuses[cn.name].target_swpin_specs = cn.specs
        end
      end

      # Check runtime status
      tw = ConfCtl::ParallelExecutor.new(deps.length)

      statuses.each do |host, st|
        tw.add do
          st.query(toplevel: opts[:generation] != 'none')
        end
      end

      tw.run

      # Collect all swpins
      swpins = []

      statuses.each do |host, st|
        st.target_swpin_specs.each_key do |name|
          swpins << name unless swpins.include?(name)
        end

        st.evaluate
      end

      # Render results
      cols = %w(host online uptime status generations) + swpins
      rows = []

      statuses.each do |host, st|
        build_generations = ConfCtl::Generation::BuildList.new(host)

        row = {
          'host' => host,
          'online' => st.online? && Rainbow('yes').green,
          'uptime' => st.uptime && format_duration(st.uptime),
          'status' => st.status ? Rainbow('ok').green : Rainbow('outdated').red,
          'generations' => "#{build_generations.count}:#{st.generations && st.generations.count}",
        }

        swpins.each do |name|
          swpin_state = st.swpins_state[name]

          row[name] =
            if swpin_state
              Rainbow(swpin_state.current_version).color(
                swpin_state.uptodate? ? :green : :red,
              )
            else
              nil
            end
        end

        rows << row
      end

      OutputFormatter.print(rows, cols, layout: :columns, color: use_color?)
    end

    def changelog
      compare_swpins do |io, host, status, sw_name, spec|
        begin
          s = spec.string_changelog_info(
            opts[:downgrade] ? :downgrade : :upgrade,
            status.swpins_info[sw_name],
            color: use_color?,
            verbose: opts[:verbose],
            patch: opts[:patch],
          )
        rescue ConfCtl::Error => e
          io.puts e.message
        else
          io.puts (s || 'no changes')
        end
      end
    end

    def diff
      compare_swpins do |io, host, status, sw_name, spec|
        begin
          s = spec.string_diff_info(
            opts[:downgrade] ? :downgrade : :upgrade,
            status.swpins_info[sw_name],
            color: use_color?,
          )
        rescue ConfCtl::Error => e
          io.puts e.message
        else
          io.puts (s || 'no changes')
        end
      end
    end

    def cssh
      deps = select_deployments(args[0]).managed

      ask_confirmation! do
        puts "Open cssh to the following deployments:"
        list_deployments(deps)
      end

      nix = ConfCtl::Nix.new

      cssh = [
        'cssh',
        '-l', 'root',
      ]

      deps.each do |host, dep|
        cssh << dep.target_host
      end

      nix.run_command_in_shell(
        packages: ['perlPackages.AppClusterSSH'],
        command: cssh.join(' '),
      )
    end

    protected
    attr_reader :wait_online

    def deploy_in_bulk(deps, host_generations, nix, action)
      skipped_copy = []
      skipped_activation = []

      host_generations.each do |host, gen|
        if copy_to_host(nix, host, deps[host], gen.toplevel) == :skip
          puts Rainbow("Skipping #{host}").yellow
          skipped_copy << host
        end
      end

      host_generations.each do |host, gen|
        if skipped_copy.include?(host)
          puts Rainbow("Copy to #{host} was skipped, skipping activation as well").yellow
          skipped_activation << host
          next
        end

        if deploy_to_host(nix, host, deps[host], gen.toplevel, action) == :skip
          puts Rainbow("Skipping #{host}").yellow
          skipped_activation << host
          next
        end

        puts if opts[:interactive]
      end

      if opts[:reboot]
        host_generations.each do |host, gen|
          if skipped_activation.include?(host)
            puts Rainbow("Activation on #{host} was skipped, skipping reboot as well").yellow
            next
          end

          if reboot_host(host, deps[host]) == :skip
            puts "Skipping #{host}"
            next
          end

          puts if opts[:interactive]
        end
      end
    end

    def deploy_one_by_one(deps, host_generations, nix, action)
      host_generations.each do |host, gen|
        dep = deps[host]

        if copy_to_host(nix, host, dep, gen.toplevel) == :skip
          puts Rainbow("Skipping #{host}").yellow
          next
        end

        if deploy_to_host(nix, host, dep, gen.toplevel, action) == :skip
          puts Rainbow("Skipping #{host}").yellow
          next
        end

        if opts[:reboot] && reboot_host(host, dep) == :skip
          puts Rainbow("Skipping #{host}").yellow
          next
        end

        puts if opts[:interactive]
      end
    end

    def copy_to_host(nix, host, dep, toplevel)
      puts Rainbow("Copying configuration to #{host} (#{dep.target_host})").yellow

      if opts[:interactive] && !ask_confirmation(always: true)
        return :skip
      end

      unless nix.copy(dep, toplevel)
        fail "Error while copying system to #{host}"
      end

      true
    end

    def deploy_to_host(nix, host, dep, toplevel, action)
      if opts['dry-activate-first']
        puts Rainbow("Trying to activate configuration on #{host} (#{dep.target_host})").yellow

        unless nix.activate(dep, toplevel, 'dry-activate')
          fail "Error while activating configuration on #{host}"
        end
      end

      puts Rainbow("Activating configuration on #{host} (#{dep.target_host}): #{action}").yellow

      if opts[:interactive] && !ask_confirmation(always: true)
        return :skip
      end

      unless nix.activate(dep, toplevel, action)
        fail "Error while activating configuration on #{host}"
      end

      unless nix.set_profile(dep, toplevel)
        fail "Error while setting profile on #{host}"
      end
    end

    def reboot_host(host, dep)
      if dep.localhost?
        puts Rainbow("Skipping reboot of #{host} as it is localhost").yellow
        return :skip
      end

      puts Rainbow("Rebooting #{host} (#{dep.target_host})").yellow

      if opts[:interactive] && !ask_confirmation(always: true)
        return :skip
      end

      m = ConfCtl::MachineControl.new(dep)

      if wait_online == :nowait
        m.reboot
      else
        since = Time.now
        spinner = nil

        secs = m.reboot_and_wait(
          timeout: wait_online == :wait ? nil : wait_online,
        ) do |state, timeleft|
          if state == :reboot
            spinner = TTY::Spinner.new(
              ":spinner Waiting for #{host} (:seconds s)",
              format: :classic,
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

        puts Rainbow("#{host} (#{dep.target_host}) is online (took #{secs.round(1)}s to reboot)").yellow
      end
    end

    def select_deployments(pattern)
      deps = ConfCtl::Deployments.new(show_trace: opts['show-trace'])

      attr_filters = AttrFilters.new(opts[:attr])
      tag_filters = TagFilters.new(opts[:tag])

      deps.select do |host, d|
        (pattern.nil? || ConfCtl::Pattern.match?(pattern, host)) \
          && attr_filters.pass?(d) \
          && tag_filters.pass?(d)
      end
    end

    def list_deployments(deps)
      cols =
        if opts[:output]
          opts[:output].split(',')
        else
          ConfCtl::Settings.instance.list_columns
        end

      rows = deps.map do |host, d|
        Hash[cols.map { |c| [c, d[c]] }]
      end

      OutputFormatter.print(rows, cols, layout: :columns)
    end

    def find_generations(deps, generation_name)
      host_generations = {}
      missing_hosts = []

      deps.each do |host, d|
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

      if host_generations.empty?
        fail 'No generation found'
      end

      if missing_hosts.any?
        ask_confirmation! do
          puts "Generation '#{generation_name}' was not found on the following hosts:"
          missing_hosts.each { |host| puts "  #{host}" }
          puts
          puts "These hosts will be ignored."
        end
      end

      host_generations
    end

    def do_build(deps)
      nix = ConfCtl::Nix.new(show_trace: opts['show-trace'])
      host_swpin_paths = {}

      autoupdate_swpins(deps)
      host_swpin_specs = check_swpins(deps)

      unless host_swpin_specs
        fail 'one or more swpins need to be updated'
      end

      deps.each do |host, d|
        puts Rainbow("Evaluating swpins for #{host}...").bright
        host_swpin_paths[host] = nix.eval_swpins(host).update(d.nix_paths)
      end

      grps = swpin_build_groups(host_swpin_paths)
      puts
      puts "Deployments will be built in #{grps.length} groups"
      puts
      host_generations = {}
      time = Time.now

      grps.each do |hosts, swpin_paths|
        puts Rainbow("Building deployments").bright
        hosts.each { |h| puts "  #{h}" }
        puts "with swpins"
        swpin_paths.each { |k, v| puts "  #{k}=#{v}" }

        host_generations.update(nix.build_toplevels(
          hosts: hosts,
          swpin_paths: swpin_paths,
          time: time,
          host_swpin_specs: host_swpin_specs,
        ))
      end

      generation_hosts = {}

      host_generations.each do |host, gen|
        generation_hosts[gen.name] ||= []
        generation_hosts[gen.name] << host
      end

      puts
      puts Rainbow("Built generations:").bright
      generation_hosts.each do |gen, hosts|
        puts Rainbow(gen).cyan
        hosts.each { |host| puts "  #{host}" }
      end

      host_generations
    end

    def autoupdate_swpins(deps)
      puts Rainbow("Running swpins auto updates...").bright
      channels = ConfCtl::Swpins::ChannelList.new
      channels.each(&:parse)
      channels_update = []

      core = ConfCtl::Swpins::Core.new(channels)
      core.parse

      core.channels.each do |c|
        channels_update << c unless channels_update.include?(c)
      end

      cluster_names = ConfCtl::Swpins::ClusterNameList.new(
        channels: channels,
        deployments: deps,
      )

      cluster_names.each do |cn|
        cn.parse

        cn.channels.each do |c|
          channels_update << c unless channels_update.include?(c)
        end
      end

      channels_update.each do |c|
        updated = false

        c.specs.each do |name, s|
          if s.auto_update?
            puts " updating #{c.name}.#{name}"
            s.prefetch_update
            updated = true
          end
        end

        c.save if updated
      end

      core_updated = false

      core.specs.each do |name, s|
        if !s.from_channel? && s.auto_update?
          puts " updating #{core.name}.#{name}"
          s.prefetch_update
          core_updated = true
        end
      end

      core.save if core_updated

      cluster_names.each do |cn|
        updated = false

        cn.specs.each do |name, s|
          if !s.from_channel? && s.auto_update?
            puts " updating #{c.name}.#{name}"
            s.prefetch_update
            updated = true
          end
        end

        cn.save if updated
      end
    end

    def check_swpins(deps)
      ret = {}
      valid = true

      channels = ConfCtl::Swpins::ChannelList.new
      channels.each(&:parse)

      puts Rainbow("Checking core swpins...").bright

      core = ConfCtl::Swpins::Core.new(channels)
      core.parse

      core.specs.each do |name, s|
        puts "  #{name} ... "+
             (s.valid? ? Rainbow('ok').green : Rainbow('needs update').cyan)
        valid = false unless s.valid?
      end

      ConfCtl::Swpins::ClusterNameList.new(
        channels: channels, deployments: deps,
      ).each do |cn|
        cn.parse

        puts Rainbow("Checking swpins for #{cn.name}...").bright

        cn.specs.each do |name, s|
          puts "  #{name} ... "+
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
      deps = select_deployments(args[0]).managed

      ask_confirmation! do
        puts "Compare swpins on the following deployments:"
        list_deployments(deps)
        puts
        puts "Generation: #{opts[:generation] || 'current configuration'}"
      end

      statuses = Hash[deps.map do |host, dep|
        [host, ConfCtl::MachineStatus.new(dep)]
      end]

      if opts[:generation]
        host_generations = find_generations(deps, opts[:generation])

        host_generations.each do |host, gen|
          statuses[host].target_swpin_specs = gen.swpin_specs
        end

        # Ignore statuses when no generation was found
        statuses.delete_if do |host, st|
          !host_generations.has_key?(host)
        end
      else
        channels = ConfCtl::Swpins::ChannelList.new
        channels.each(&:parse)

        ConfCtl::Swpins::ClusterNameList.new(
          deployments: deps,
          channels: channels,
        ).each do |cn|
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

    def format_duration(interval)
      {
        'd' => 24*60*60,
        'h' => 60*60,
        'm' => 60,
        's' => 1,
      }.each do |unit, n|
        if interval > n
          return "#{(interval / n.to_f).round(1)}#{unit}"
        end
      end

      raise ArgumentError, "invalid time duration '#{interval}'"
    end
  end
end
