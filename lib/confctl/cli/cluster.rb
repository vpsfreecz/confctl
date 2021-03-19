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

      ask_confirmation! do
        puts "The following deployments will be built and deployed:"
        list_deployments(deps)
        puts
        puts "Target action: #{action}"
      end

      host_toplevels = do_build(deps)
      nix = ConfCtl::Nix.new(show_trace: opts['show-trace'])

      if opts['one-by-one']
        deploy_one_by_one(deps, host_toplevels, nix, action)
      else
        deploy_in_bulk(deps, host_toplevels, nix, action)
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
    def deploy_in_bulk(deps, host_toplevels, nix, action)
      skipped_copy = []

      host_toplevels.each do |host, toplevel|
        if copy_to_host(nix, host, deps[host], toplevel) == :skip
          puts "Skipping #{host}"
          skipped_copy << host
        end
      end

      host_toplevels.each do |host, toplevel|
        if skipped_copy.include?(host)
          puts "Copy to #{host} was skipped, skipping activation as well"
          next
        end

        if deploy_to_host(nix, host, deps[host], toplevel, action) == :skip
          puts "Skipping #{host}"
          next
        end

        puts if opts[:interactive]
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
  end
end
