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

      program_desc 'Manage vpsFree.cz cluster configuration and deployments'
      subcommand_option_handling :normal
      preserve_argv true
      arguments :strict
      hide_commands_without_desc true

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

        c.action &Command.run(Nix, :list)
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

        c.action &Command.run(Nix, :build)
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

        c.action &Command.run(Nix, :deploy)
      end

      desc 'Access rendered documentation'
      command :docs do |docs|
        docs.desc 'Start HTTP server'
        docs.command :start do |c|
          c.action &Command.run(Documentation, :start_server)
        end

        docs.desc 'Stop HTTP server'
        docs.command :stop do |c|
          c.action &Command.run(Documentation, :stop_server)
        end
      end

      desc 'Generate data files from vpsAdmin API'
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
