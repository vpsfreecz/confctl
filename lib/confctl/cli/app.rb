require 'gli'

module ConfCtl::Cli
  class App
    include GLI::App

    def self.get
      cli = new
      cli.setup
      cli
    end

    def self.run
      cli = get
      exit(cli.run(ARGV))
    end

    def setup
      Thread.abort_on_exception = true

      program_desc 'Nix deployment configuration management tool'
      subcommand_option_handling :normal
      preserve_argv true
      arguments :strict
      hide_commands_without_desc true

      desc 'Toggle color mode'
      flag %i(c color), must_match: %w(always never auto), default_value: 'auto'

      desc 'Create a new configuration'
      command :init do |c|
        c.action &Command.run(c, Configuration, :init)
      end

      desc 'Add a new machine'
      arg_name '<name>'
      command :add do |c|
        c.action &Command.run(c, Configuration, :add)
      end

      desc 'Rename an existing machine'
      arg_name '<old-name> <new-name>'
      command :rename do |c|
        c.action &Command.run(c, Configuration, :rename)
      end

      desc 'Update cluster machine list with contents of cluster/'
      command :rediscover do |c|
        c.action &Command.run(c, Configuration, :rediscover)
      end

      desc 'Manage software pins'
      command :swpins do |pins|
        pins.desc 'Manage software pins channels'
        pins.command :channel do |ch|
          ch.desc 'List configured sw pins'
          ch.arg_name '[channel [sw]]'
          ch.command :ls do |c|
            c.action &Command.run(c, Swpins::Channel, :list)
          end

          swpins_commands(ch, Swpins::Channel, 'channel')
        end

        pins.desc 'Manage cluster software pins'
        pins.command :cluster do |cl|
          cl.desc 'List configured sw pins'
          cl.arg_name '[cluster-name [sw]]'
          cl.command :ls do |c|
            c.action &Command.run(c, Swpins::Cluster, :list)
          end

          swpins_commands(cl, Swpins::Cluster, 'name')
        end

        pins.desc 'Manage core software pins'
        pins.command :core do |core|
          core.desc 'List configured sw pins'
          core.arg_name '[sw]'
          core.command :ls do |c|
            c.action &Command.run(c, Swpins::Core, :list)
          end

          core.desc 'Set to specific version'
          core.arg_name "<sw> <ref>"
          core.command :set do |c|
            c.action &Command.run(c, Swpins::Core, :set)
          end

          core.desc 'Update to newest version'
          core.arg_name "[<sw> [<version...>]]]"
          core.command :update do |c|
            c.action &Command.run(c, Swpins::Core, :update)
          end
        end

        pins.desc 'Update all swpins'
        pins.command :update do |c|
          c.action &Command.run(c, Swpins::Base, :update)
        end

        pins.desc 'Generate confctl-managed JSON files for configured swpins'
        pins.command :reconfigure do |c|
          c.action &Command.run(c, Swpins::Base, :reconfigure)
        end
      end

      desc 'List configured machines'
      arg_name '[machine-pattern]'
      command :ls do |c|
        c.desc 'Enable traces in Nix'
        c.switch 'show-trace'

        c.desc 'Filter (un)managed machines'
        c.flag :managed, must_match: %w(y yes n no a all)

        c.desc 'List possible attributes to output'
        c.switch %i(L list), negatable: false

        c.desc 'Select attributes to output'
        c.flag %i(o output)

        c.desc 'Do not show the header'
        c.switch %i(H hide-header)

        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.action &Command.run(c, Cluster, :list)
      end

      desc 'Build target systems'
      arg_name '[machine-pattern]'
      command :build do |c|
        c.desc 'Enable traces in Nix'
        c.switch 'show-trace'

        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        nix_build_options(c)

        c.action &Command.run(c, Cluster, :build)
      end

      desc 'Deploy target systems'
      arg_name '[machine-pattern [switch-action]]'
      command :deploy do |c|
        c.desc 'Enable traces in Nix'
        c.switch 'show-trace'

        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.desc 'Deploy selected generation'
        c.flag %i(g generation)

        c.desc 'Ask for confirmation before activation'
        c.switch %w(i interactive)

        c.desc 'Try to dry-activate before the real switch'
        c.switch 'dry-activate-first'

        c.desc 'Copy and deploy machines one by one'
        c.switch 'one-by-one'

        c.desc 'Max number of concurrent nix-copy-closure processes'
        c.flag 'max-concurrent-copy', arg_name: 'n', type: Integer,
          default_value: 5

        c.desc 'Do not activate copied closures'
        c.switch 'copy-only', negatable: false

        c.desc 'Reboot target systems after deployment'
        c.switch :reboot

        c.desc 'Wait for the machine to boot'
        c.flag 'wait-online', default_value: '600'

        nix_build_options(c)

        c.desc 'Toggle health checks'
        c.switch 'health-checks', default_value: true

        c.desc 'Do not abourt on failed health checks'
        c.switch 'keep-going', default_value: false

        c.action &Command.run(c, Cluster, :deploy)
      end

      desc 'Run machine health-checks'
      arg_name '[machine-pattern]'
      command :'health-check' do |c|
        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.desc 'Maximum number of health-check jobs'
        c.flag %w(j max-jobs), arg_name: 'number', type: Integer, default_value: 5

        c.action &Command.run(c, Cluster, :health_check)
      end

      desc 'Check machine status'
      arg_name '[machine-pattern]'
      command :status do |c|
        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.desc 'Check status against selected generation'
        c.flag %i(g generation)

        c.action &Command.run(c, Cluster, :status)
      end

      desc 'Changelog between deployed and configured swpins'
      arg_name '[machine-pattern [sw-pattern]]'
      command :changelog do |c|
        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.desc 'Show changelog against swpins from selected generation'
        c.flag %i(g generation)

        c.desc 'Show a changelog for downgrade'
        c.switch %i(d downgrade)

        c.desc 'Show full-length changelog descriptions'
        c.switch %i(v verbose)

        c.desc 'Show patches'
        c.switch %i(p patch)

        nix_build_options(c)

        c.action &Command.run(c, Cluster, :changelog)
      end

      desc 'Diff between deployed and configured swpins'
      arg_name '[machine-pattern [sw-pattern]]'
      command :diff do |c|
        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.desc 'Show diff against swpins from selected generation'
        c.flag %i(g generation)

        c.desc 'Show a changelog for downgrade'
        c.switch %i(d downgrade)

        nix_build_options(c)

        c.action &Command.run(c, Cluster, :diff)
      end

      desc 'Test SSH connection'
      arg_name '[machine-pattern]'
      command :'test-connection' do |c|
        c.desc 'Filter (un)managed machines'
        c.flag :managed, must_match: %w(y yes n no a all)

        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.action &Command.run(c, Cluster, :test_connection)
      end

      desc 'Run command over SSH'
      arg_name '[machine-pattern [command [arguments...]]]'
      command :ssh do |c|
        c.desc 'Filter (un)managed machines'
        c.flag :managed, must_match: %w(y yes n no a all)

        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.desc 'Run command in parallel on all machines at once'
        c.switch %i(p parallel)

        c.desc 'Aggregate identical command output'
        c.switch %i(g aggregate)

        c.desc 'Data passed to standard input'
        c.flag %i(i input-string)

        c.desc 'File passed to standard input'
        c.flag %i(f input-file)

        c.action &Command.run(c, Cluster, :ssh)
      end

      desc 'Open ClusterSSH'
      arg_name '[machine-pattern]'
      command :cssh do |c|
        c.desc 'Filter (un)managed machines'
        c.flag :managed, must_match: %w(y yes n no a all)

        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.action &Command.run(c, Cluster, :cssh)
      end

      desc 'Manage built machine generations'
      command :generation do |gen|
        gen.desc 'List machine generations'
        gen.arg_name '[machine-pattern [generation-pattern]]'
        gen.command :ls do |c|
          c.desc 'Filter by attribute'
          c.flag %i(a attr), multiple: true

          c.desc 'Filter by tag'
          c.flag %i(t tag), multiple: true

          c.desc 'List local build generations'
          c.switch %i(l local)

          c.desc 'List remote machine generations'
          c.switch %i(r remote)

          c.action &Command.run(c, Generation, :list)
        end

        gen.desc 'Remove machine generations'
        gen.arg_name '[machine-pattern [generation-pattern|old]]'
        gen.command :rm do |c|
          c.desc 'Filter by attribute'
          c.flag %i(a attr), multiple: true

          c.desc 'Filter by tag'
          c.flag %i(t tag), multiple: true

          c.desc 'List local build generations'
          c.switch %i(l local)

          c.desc 'List remote machine generations'
          c.switch %i(r remote)

          c.desc 'Run nix-collect-garbage to delete unreachable store paths'
          c.switch %i(gc collect-garbage), default_value: true

          c.desc 'Max number of concurrent nix-collect-garbage processes'
          c.flag 'max-concurrent-gc', arg_name: 'n', type: Integer,
            default_value: 5

          c.desc 'Assume the answer to confirmations is yes'
          c.switch %w(y yes)

          c.action &Command.run(c, Generation, :remove)
        end

        gen.desc 'Auto-remove old machine generations'
        gen.arg_name '[machine-pattern]'
        gen.command :rotate do |c|
          c.desc 'Filter by attribute'
          c.flag %i(a attr), multiple: true

          c.desc 'Filter by tag'
          c.flag %i(t tag), multiple: true

          c.desc 'List local build generations'
          c.switch %i(l local)

          c.desc 'List remote machine generations'
          c.switch %i(r remote)

          c.desc 'Max number of concurrent nix-collect-garbage processes'
          c.flag 'max-concurrent-gc', arg_name: 'n', type: Integer,
            default_value: 5

          c.desc 'Assume the answer to confirmations is yes'
          c.switch %w(y yes)

          c.action &Command.run(c, Generation, :rotate)
        end
      end

      desc 'Generate data files'
      command 'gen-data' do |gen|
        gen.desc 'Fetch data from vpsAdmin'
        gen.command :vpsadmin do |vpsa|
          vpsa.desc 'Generate all data files'
          vpsa.command :all do |c|
            c.action &Command.run(c, GenData, :vpsadmin_all)
          end

          vpsa.desc 'Generate container data files'
          vpsa.command :containers do |c|
            c.action &Command.run(c, GenData, :vpsadmin_containers)
          end

          vpsa.desc 'Generate network data files'
          vpsa.command :network do |c|
            c.action &Command.run(c, GenData, :vpsadmin_network)
          end
        end
      end

      ConfCtl::UserScripts.each do |script|
        script.setup_cli(self)
      end

      on_error do |exception|
        log = ConfCtl::Logger.instance
        warn "\nLog file: #{log.path}" if log.open?
        true
      end
    end

    protected
    def swpins_commands(cmd, klass, arg_name)
      cmd.desc 'Set to specific version'
      cmd.arg_name "<#{arg_name}> <sw> <ref>"
      cmd.command :set do |c|
        c.action &Command.run(c, klass, :set)
      end

      cmd.desc 'Update to newest version'
      cmd.arg_name "[<#{arg_name}> [<sw> [<version...>]]]"
      cmd.command :update do |c|
        c.action &Command.run(c, klass, :update)
      end
    end

    def nix_build_options(cmd)
      cmd.desc 'Maximum number of build jobs (see nix-build)'
      cmd.flag %w(j max-jobs), arg_name: 'number'
    end
  end
end
