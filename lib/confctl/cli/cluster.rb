require_relative 'command'
require 'json'

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
        puts "The following deployments will be built and deployed:"
        list_deployments(deps)
        puts
        puts "Target action: #{action}#{opts[:reboot] ? ' + reboot' : ''}"
      end

      host_toplevels = do_build(deps)
      nix = ConfCtl::Nix.new(show_trace: opts['show-trace'])

      if opts['one-by-one']
        deploy_one_by_one(deps, host_toplevels, nix, action)
      else
        deploy_in_bulk(deps, host_toplevels, nix, action)
      end
    end

    def status
      deps = select_deployments(args[0]).managed

      ask_confirmation! do
        if opts[:toplevel]
          puts "The following deployments will be built and then checked:"
        else
          puts "The following deployments will be checked:"
        end

        list_deployments(deps)
      end

      statuses = Hash[deps.map do |host, dep|
        [host, ConfCtl::MachineStatus.new(dep)]
      end]

      # Evaluate toplevels
      if opts[:toplevel]
        host_toplevels = do_build(deps)

        host_toplevels.each do |host, toplevel|
          statuses[host].target_toplevel = toplevel
        end

        puts
      end

      # Read configured swpins
      # TODO: this is done by do_build() as well
      channels = ConfCtl::Swpins::ChannelList.new
      channels.each(&:parse)

      ConfCtl::Swpins::ClusterNameList.new(
        deployments: deps,
        channels: channels,
      ).each do |cn|
        cn.parse

        statuses[cn.name].target_swpin_specs = cn.specs
      end

      # Check runtime status
      tw = ConfCtl::ParallelExecutor.new(deps.length)

      statuses.each do |host, st|
        tw.add do
          st.query(toplevel: opts[:toplevel])
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
        row = {
          'host' => host,
          'online' => st.online? && 'yes',
          'uptime' => st.uptime && format_duration(st.uptime),
          'status' => st.status ? 'ok' : 'outdated',
          'generations' => st.generations && st.generations.count,
        }

        swpins.each do |name|
          row[name] = st.swpins_state[name] ? 'ok' : 'outdated'
        end

        rows << row
      end

      OutputFormatter.print(rows, cols, layout: :columns)
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

    def deploy_in_bulk(deps, host_toplevels, nix, action)
      skipped_copy = []
      skipped_activation = []

      host_toplevels.each do |host, toplevel|
        if copy_to_host(nix, host, deps[host], toplevel) == :skip
          puts "Skipping #{host}"
          skipped_copy << host
        end
      end

      host_toplevels.each do |host, toplevel|
        if skipped_copy.include?(host)
          puts "Copy to #{host} was skipped, skipping activation as well"
          skipped_activation << host
          next
        end

        if deploy_to_host(nix, host, deps[host], toplevel, action) == :skip
          puts "Skipping #{host}"
          skipped_activation << host
          next
        end

        puts if opts[:interactive]
      end

      if opts[:reboot]
        host_toplevels.each do |host, toplevel|
          if skipped_activation.include?(host)
            puts "Activation on #{host} was skipped, skipping reboot as well"
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

    def deploy_one_by_one(deps, host_toplevels, nix, action)
      host_toplevels.each do |host, toplevel|
        dep = deps[host]

        if copy_to_host(nix, host, dep, toplevel) == :skip
          puts "Skipping #{host}"
          next
        end

        if deploy_to_host(nix, host, dep, toplevel, action) == :skip
          puts "Skipping #{host}"
          next
        end

        if opts[:reboot] && reboot_host(host, dep) == :skip
          puts "Skipping #{host}"
          next
        end

        puts if opts[:interactive]
      end
    end

    def copy_to_host(nix, host, dep, toplevel)
      puts "Copying configuration to #{host} (#{dep.target_host})"

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
        puts "Trying to activate configuration on #{host} (#{dep.target_host})"

        unless nix.activate(dep, toplevel, 'dry-activate')
          fail "Error while activating configuration on #{host}"
        end
      end

      puts "Activating configuration on #{host} (#{dep.target_host}): #{action}"

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
        puts "Skipping reboot of #{host} as it is localhost"
        return :skip
      end

      puts "Rebooting #{host} (#{dep.target_host})"

      if opts[:interactive] && !ask_confirmation(always: true)
        return :skip
      end

      m = ConfCtl::MachineControl.new(dep)

      if wait_online == :nowait
        m.reboot
      else
        secs = m.reboot_and_wait(timeout: wait_online == :wait ? nil : wait_online)
        puts "#{host} (#{dep.target_host}) is online (took #{secs.round(1)}s to reboot)"
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

    def ask_confirmation(always: false)
      return true if !always && opts[:yes]

      yield if block_given?
      STDOUT.write("\nContinue? [y/N]: ")
      STDOUT.flush
      STDIN.readline.strip.downcase == 'y'
    end

    def ask_confirmation!(**kwargs, &block)
      fail 'Aborted' unless ask_confirmation(**kwargs, &block)
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

    def do_build(deps)
      nix = ConfCtl::Nix.new(show_trace: opts['show-trace'])
      host_swpins = {}

      autoupdate_swpins(deps)

      unless check_swpins(deps)
        fail 'one or more swpins need to be updated'
      end

      deps.each do |host, d|
        puts "Evaluating swpins for #{host}..."
        host_swpins[host] = nix.eval_swpins(host).update(d.nix_paths)
      end

      grps = swpin_build_groups(host_swpins)
      puts "Deployments will be built in #{grps.length} groups"
      puts
      host_toplevels = {}

      grps.each do |hosts, swpins|
        puts "Building deployments"
        hosts.each { |h| puts "  #{h}" }
        puts "with swpins"
        swpins.each { |k, v| puts "  #{k}=#{v}" }

        host_toplevels.update(nix.build_toplevels(hosts, swpins))
      end

      host_toplevels
    end

    def autoupdate_swpins(deps)
      puts "Running swpins auto updates..."
      channels = ConfCtl::Swpins::ChannelList.new
      channels.each(&:parse)
      channels_update = []

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
      ret = true

      ConfCtl::Swpins::ClusterNameList.new(deployments: deps).each do |cn|
        cn.parse

        puts "Checking swpins for #{cn.name}..."

        cn.specs.each do |name, s|
          puts "  #{name} ... #{s.valid? ? 'ok' : 'needs update'}"
          ret = false unless s.valid?
        end
      end

      ret
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
      end

      statuses = Hash[deps.map do |host, dep|
        [host, ConfCtl::MachineStatus.new(dep)]
      end]

      channels = ConfCtl::Swpins::ChannelList.new
      channels.each(&:parse)

      ConfCtl::Swpins::ClusterNameList.new(
        deployments: deps,
        channels: channels,
      ).each do |cn|
        cn.parse

        statuses[cn.name].target_swpin_specs = cn.specs
      end

      Pager.open do |io|
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

            io.puts
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
