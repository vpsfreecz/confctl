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

      host_toplevels.each do |host, toplevel|
        dep = deps[host]
        puts "Copying configuration to #{host} (#{dep.target_host})"

        unless nix.copy(dep, toplevel)
          fail "Error while copying system to #{host}"
        end
      end

      host_toplevels.each do |host, toplevel|
        dep = deps[host]
        puts "Activating configuration on #{host} (#{dep.target_host})"

        unless nix.activate(dep, toplevel, action)
          fail "Error while activating configuration on #{host}"
        end
      end
    end

    protected
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

    def ask_confirmation
      return true if opts[:yes]

      yield
      STDOUT.write("\nContinue? [y/N]: ")
      STDOUT.flush
      STDIN.readline.strip.downcase == 'y'
    end

    def ask_confirmation!(&block)
      fail 'Aborted' unless ask_confirmation(&block)
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
        host_swpins[host] = nix.eval_swpins(host)
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
