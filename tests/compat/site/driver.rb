# frozen_string_literal: true

# Execute unmodified Ruby source; fake only external dependencies and deploy
# stages outside this package. Every unexpected external command fails closed.
require 'fileutils'
require 'json'
require 'ostruct'

module SiteOracle
  ROOT = File.realpath(ENV.fetch('CONFCTL_COMPAT_ROOT'))
  SCENARIO = JSON.parse(File.read(File.join(ROOT, 'scenario.json')))
  EVENTS = [] # rubocop:disable Style/MutableConstant -- per-child append-only event trace
  MUTEX = Mutex.new

  def self.record(event, data = {})
    MUTEX.synchronize { EVENTS << { 'event' => event }.merge(data) }
  end

  def self.kernel_state
    path = File.join(ROOT, 'configs/node/kernels.json')
    File.file?(path) ? File.read(path) : nil
  end

  def self.prepare
    FileUtils.mkdir_p(File.join(ROOT, 'scripts'))
    FileUtils.mkdir_p(File.join(ROOT, 'configs/node'))
    FileUtils.mkdir_p(File.join(ROOT, 'cluster'))
    File.write(File.join(ROOT, 'flake.nix'), '{}')
    Dir[File.join(__dir__, 'baseline/*.txt')].each do |source|
      FileUtils.cp(source, File.join(ROOT, 'scripts', File.basename(source, '.txt')))
    end
    SCENARIO.fetch('files', {}).each do |path, text|
      raise 'Fixture path escape' if path.start_with?('/') || path.split('/').include?('..')

      destination = File.join(ROOT, path)
      FileUtils.mkdir_p(File.dirname(destination))
      File.write(destination, text)
    end
  end

  class Command
    Result = Struct.new(:out, :err, :exit_status)

    def run(*argv, **_options)
      hosts = SiteOracle::SCENARIO.fetch('machines')
      host =
        if argv == ['uname', '-r']
          hosts.find { |machine| machine.fetch('metaConfig').fetch('host')['target'] == 'localhost' }
        elsif argv[0] == 'ssh' && argv[-1] == 'uname -r'
          hosts.find { |machine| machine.fetch('metaConfig').fetch('host')['target'] == argv[-2] }
        end
      raise "Unexpected external command: #{argv.inspect}" unless host

      name = host.fetch('name')
      response = SiteOracle::SCENARIO.fetch('responses', {}).fetch(name, {})
      SiteOracle.record('exec', 'name' => name, 'argv' => argv)
      sleep(response.fetch('delay', 0))
      raise IOError, 'fixture transport failure' if response['infrastructure_error']

      result = Result.new(response.fetch('stdout', "6.12.3-extra.more\n"), response.fetch('stderr', ''), response.fetch('status', 0))
      SiteOracle.record('exec.complete', 'name' => name, 'status' => result.exit_status)
      raise TTY::Command::ExitError.new(argv.join(' '), result) if result.exit_status != 0

      result
    end
  end
end

SiteOracle.prepare
Dir.chdir(SiteOracle::ROOT)
$:.unshift(File.join(ENV.fetch('CONFCTL_RUBY_ORACLE_ROOT'), 'lib'))
require 'confctl'
require 'confctl/cli'

class ConfCtl::Nix
  def initialize(**_options); end

  def confctl_settings
    { 'list' => { 'columns' => %w[name spin host.target] }, 'nix' => { 'maxJobs' => 'auto' } }
  end

  def list_machines
    path = File.join(SiteOracle::ROOT, 'cluster/cluster.nix')
    SiteOracle.record('inventory', 'core_list' => File.file?(path) ? File.read(path) : nil)
    raise 'fixture inventory failure' if SiteOracle::SCENARIO['inventory_error']

    SiteOracle::SCENARIO.fetch('machines').to_h { |machine| [machine.fetch('name'), machine] }
  end
end

module ConfCtl::SystemCommand
  def self.new(**_options)
    SiteOracle::Command.new
  end
end

class ConfCtl::Cli::Cluster
  def do_build(machines)
    SiteOracle.record('build', 'kernels' => SiteOracle.kernel_state)
    machines.transform_values { true }
  end

  def find_generations(machines, selector)
    SiteOracle.record('generation.lookup', 'selector' => selector, 'kernels' => SiteOracle.kernel_state)
    machines.transform_values { true }
  end

  def skip_current_deploy_targets(machines, generations, action)
    SiteOracle.record('skip-current', 'action' => action)
    [machines, SiteOracle::SCENARIO['already_current'] ? {} : generations]
  end

  def deploy_in_bulk(machines, _generations, _nix, action)
    SiteOracle.record('deploy.bulk', 'names' => machines.map { |name, _machine| name }, 'action' => action, 'copy_only' => opts['copy-only'])
  end

  def deploy_one_by_one(machines, generations, nix, action)
    SiteOracle.record('deploy.one-by-one')
    deploy_in_bulk(machines, generations, nix, action)
  end
end

at_exit do
  files = %w[configs/node/kernels.json cluster/netbootable.nix cluster/cluster.nix].to_h do |path|
    [path, File.file?(path) ? File.read(path) : nil]
  end
  error = $!
  observation = { 'events' => SiteOracle::EVENTS, 'files' => files }
  observation['exception'] = error.class.name if error && !error.is_a?(SystemExit)
  File.write(File.join(SiteOracle::ROOT, 'observation.json'), JSON.generate(observation))
end

exit(ConfCtl::Cli::App.get.run(SiteOracle::SCENARIO.fetch('argv')))
