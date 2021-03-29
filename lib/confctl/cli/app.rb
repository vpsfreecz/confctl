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
        c.action &Command.run(Configuration, :init)
      end

      desc 'Add a new deployment'
      arg_name '<name>'
      command :add do |c|
        c.action &Command.run(Configuration, :add)
      end

      desc 'Rename an existing deployment'
      arg_name '<old-name> <new-name>'
      command :rename do |c|
        c.action &Command.run(Configuration, :rename)
      end

      desc 'Update deployment list with contents of cluster/'
      command :rediscover do |c|
        c.action &Command.run(Configuration, :rediscover)
      end

      desc 'Manage software pins'
      command :swpins do |pins|
        pins.desc 'Manage software pins channels'
        pins.command :channel do |ch|
          ch.desc 'List configured sw pins'
          ch.arg_name '[channel [sw]]'
          ch.command :ls do |c|
            c.action &Command.run(Swpins::Channel, :list)
          end

          swpins_commands(ch, Swpins::Channel, 'channel')
        end

        pins.desc 'Manage cluster software pins'
        pins.command :cluster do |cl|
          cl.desc 'List configured sw pins'
          cl.arg_name '[cluster-name [sw]]'
          cl.command :ls do |c|
            c.action &Command.run(Swpins::Cluster, :list)
          end

          swpins_commands(cl, Swpins::Cluster, 'name')
        end

        pins.desc 'Generate confctl-managed JSON files for configured swpins'
        pins.command :reconfigure do |c|
          c.action &Command.run(Swpins::Base, :reconfigure)
        end
      end

      desc 'List configured deployments'
      arg_name '[host-pattern]'
      command :ls do |c|
        c.desc 'Enable traces in Nix'
        c.switch 'show-trace'

        c.desc 'Filter (un)managed deployments'
        c.flag :managed, must_match: %w(y yes n no a all)

        c.desc 'Select attributes to output'
        c.flag %i(o output)

        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.action &Command.run(Cluster, :list)
      end

      desc 'Build target systems'
      arg_name '[host-pattern]'
      command :build do |c|
        c.desc 'Enable traces in Nix'
        c.switch 'show-trace'

        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.action &Command.run(Cluster, :build)
      end

      desc 'Deploy target systems'
      arg_name '[host-pattern [switch-action]]'
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

        c.desc 'Reboot target systems after deployment'
        c.switch :reboot

        c.desc 'Wait for the host to boot'
        c.flag 'wait-online', default_value: '600'

        c.action &Command.run(Cluster, :deploy)
      end

      desc 'Check host status'
      arg_name '[host-pattern]'
      command :status do |c|
        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.desc 'Check status against selected generation'
        c.flag %i(g generation)

        c.action &Command.run(Cluster, :status)
      end

      desc 'Changelog between deployed and configured swpins'
      arg_name '[host-pattern [sw-pattern]]'
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

        c.action &Command.run(Cluster, :changelog)
      end

      desc 'Diff between deployed and configured swpins'
      arg_name '[host-pattern [sw-pattern]]'
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

        c.action &Command.run(Cluster, :diff)
      end

      desc 'Open ClusterSSH'
      arg_name '[host-pattern]'
      command :cssh do |c|
        c.desc 'Filter by attribute'
        c.flag %i(a attr), multiple: true

        c.desc 'Filter by tag'
        c.flag %i(t tag), multiple: true

        c.desc 'Assume the answer to confirmations is yes'
        c.switch %w(y yes)

        c.action &Command.run(Cluster, :cssh)
      end

      desc 'Manage built host generations'
      command :generation do |gen|
        gen.desc 'List host generations'
        gen.arg_name '[host-pattern [generation-pattern]]'
        gen.command :ls do |c|
          c.desc 'Filter by attribute'
          c.flag %i(a attr), multiple: true

          c.desc 'Filter by tag'
          c.flag %i(t tag), multiple: true

          c.desc 'List local build generations'
          c.switch %i(l local)

          c.desc 'List remote host generations'
          c.switch %i(r remote)

          c.action &Command.run(Generation, :list)
        end

        gen.desc 'Remove host generations'
        gen.arg_name '[host-pattern [generation-pattern|old]]'
        gen.command :rm do |c|
          c.desc 'Filter by attribute'
          c.flag %i(a attr), multiple: true

          c.desc 'Filter by tag'
          c.flag %i(t tag), multiple: true

          c.desc 'List local build generations'
          c.switch %i(l local)

          c.desc 'List remote host generations'
          c.switch %i(r remote)

          c.desc 'Assume the answer to confirmations is yes'
          c.switch %w(y yes)

          c.action &Command.run(Generation, :remove)
        end

        gen.desc 'Auto-remove old host generations'
        gen.arg_name '[host-pattern]'
        gen.command :rotate do |c|
          c.desc 'Filter by attribute'
          c.flag %i(a attr), multiple: true

          c.desc 'Filter by tag'
          c.flag %i(t tag), multiple: true

          c.desc 'List local build generations'
          c.switch %i(l local)

          c.desc 'List remote host generations'
          c.switch %i(r remote)

          c.desc 'Assume the answer to confirmations is yes'
          c.switch %w(y yes)

          c.action &Command.run(Generation, :rotate)
        end
      end

      desc 'Generate data files'
      command 'gen-data' do |gen|
        gen.desc 'Fetch data from vpsAdmin'
        gen.command :vpsadmin do |vpsa|
          vpsa.desc 'Generate all data files'
          vpsa.command :all do |c|
            c.action &Command.run(GenData, :vpsadmin_all)
          end

          vpsa.desc 'Generate container data files'
          vpsa.command :containers do |c|
            c.action &Command.run(GenData, :vpsadmin_containers)
          end

          vpsa.desc 'Generate network data files'
          vpsa.command :network do |c|
            c.action &Command.run(GenData, :vpsadmin_network)
          end
        end
      end
    end

    protected
    def swpins_commands(cmd, klass, arg_name)
      cmd.desc 'Set to specific version'
      cmd.arg_name "<#{arg_name}> <sw> <ref>"
      cmd.command :set do |c|
        c.action &Command.run(klass, :set)
      end

      cmd.desc 'Update to newest version'
      cmd.arg_name "[<#{arg_name}> [<sw> [<version...>]]]"
      cmd.command :update do |c|
        c.action &Command.run(klass, :update)
      end
    end
  end
end
