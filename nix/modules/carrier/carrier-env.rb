#!@ruby@/bin/ruby
require 'optparse'

class CarrierEnv
  ON_CHANGE_COMMANDS = '@onChangeCommands@'.freeze

  def self.run(args)
    env = new
    env.run(args)
  end

  def initialize
    @action = nil
    @profile = nil
    @generation = nil

    @optparser = OptionParser.new do |parser|
      parser.banner = "Usage: #{$0} <options>"

      parser.on('-p', '--profile PROFILE', 'Profile path') do |v|
        @profile = v
      end

      parser.on('--set GENERATION', 'Set profile to generation') do |v|
        @action = :set
        @generation = v
      end

      parser.on('--delete-generations GENERATIONS', 'Delete generations') do |v|
        @action = :delete
        @generation = v
      end
    end
  end

  def run(args)
    @optparser.parse!(args)

    if args.any?
      warn 'Too many arguments'
      puts parser
      exit(false)
    elsif @action.nil?
      warn 'No action specified'
      exit(false)
    elsif @profile.nil?
      warn 'Profile not set'
      exit(false)
    end

    send(:"run_#{@action}")
  end

  protected

  def run_set
    nix_env('--set', @generation)
    on_change_commands
  end

  def run_delete
    nix_env('--delete-generations', @generation)
    on_change_commands
  end

  def nix_env(*args)
    system_command('nix-env', '-p', @profile, *args)
  end

  def on_change_commands
    system_command(ON_CHANGE_COMMANDS)
  end

  def system_command(*args)
    return if Kernel.system(*args)

    raise "#{args.join(' ')} failed"
  end
end

CarrierEnv.run(ARGV)
