require 'json'

module ConfCtl::Cli
  class Nix < Command
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

      deps.select do |host, d|
        (pattern.nil? || ConfCtl::Pattern.match?(pattern, host)) \
          && (opts[:spin].nil? || opts[:spin] == d.spin)
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
      fmt, cols, fmtopts = printf_fmt_cols
      puts sprintf(fmt, *cols)

      deps.each do |host, d|
        args = [fmt, host]
        args << (d.managed ? 'yes' : 'no') if fmtopts[:managed]
        args << d.spin

        puts sprintf(*args)
      end
    end

    def do_build(deps)
      nix = ConfCtl::Nix.new(show_trace: opts['show-trace'])
      host_swpins = {}

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

    def printf_fmt_cols
      fmts = %w(%-40s)
      cols = %w(HOST)
      managed = %w(a all).include?(opts[:managed])

      if managed
        fmts << '%-10s'
        cols << 'MANAGED'
      end

      fmts.concat(%w(%s))
      cols.concat(%w(SPIN))

      [fmts.join(' '), cols, {managed: managed}]
    end
  end
end
