require 'json'
require 'tty-command'
require 'confctl/inputs_info'

module ConfCtl
  class MachineStatus
    # @return [Machine]
    attr_reader :machine

    # @return [Float]
    attr_reader :uptime

    # @return [String]
    attr_accessor :target_toplevel

    # @return [String]
    attr_reader :current_toplevel

    # @return [String]
    attr_reader :timezone_name

    # @return [String]
    attr_reader :timezone_offset

    # @return [Generation::HostList]
    attr_reader :generations

    # @return [Hash]
    attr_accessor :target_inputs_info

    # @return [Hash]
    attr_reader :inputs_info

    # @param machine [Machine]
    def initialize(machine)
      @machine = machine
      @mc = MachineControl.new(machine.carried? ? machine.carrier_machine : machine)
    end

    # Connect to the machine and query its state
    def query(toplevel: true, generations: true, inputs: false)
      begin
        @uptime = mc.uptime
      rescue TTY::Command::ExitError
        return
      end

      if toplevel
        begin
          @current_toplevel = query_toplevel
        rescue TTY::Command::ExitError
          return
        end
      end

      if generations
        begin
          @generations = Generation::HostList.fetch(machine, mc, profile: machine.profile)
        rescue TTY::Command::ExitError
          return
        end
      end

      return unless inputs

      begin
        @inputs_info = query_inputs_info
      rescue Error
        nil
      end
    end

    protected

    attr_reader :mc

    def query_toplevel
      path =
        if machine.carried?
          machine.profile
        else
          '/run/current-system'
        end

      mc.read_realpath(path)
    end

    def query_inputs_info
      json = read_inputs_info_json

      case json
      when String
        InputsInfo.parse(json)
      when Hash
        InputsInfo.normalize(json)
      end
    end

    # @return [Hash, String, nil]
    # @return [Hash, String, nil]
    def read_inputs_info_json
      if machine.carried?
        read_carried_info_json('inputs-info', '/etc/confctl/inputs-info.json')
      else
        read_file('/etc/confctl/inputs-info.json')
      end
    end

    def read_carried_info_json(key, path)
      info = read_carried_machine_json(key)
      return info if info

      read_file(File.join(machine.profile, path))
    end

    def read_carried_machine_json(key)
      json = mc.read_file(File.join(machine.profile, 'machine.json'))
      parsed = JSON.parse(json)
      parsed[key] if parsed[key]
    rescue TTY::Command::ExitError, JSON::ParserError
      nil
    end

    def read_file(path)
      mc.read_file(path)
    rescue TTY::Command::ExitError
      nil
    end
  end
end
