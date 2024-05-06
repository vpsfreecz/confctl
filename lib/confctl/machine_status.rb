require 'json'
require 'tty-command'

module ConfCtl
  class MachineStatus
    class SwpinState
      # @return [Swpins::Specs::Base]
      attr_reader :target_spec

      # @return [Hash, nil]
      attr_reader :current_info

      # @param target_spec [Swpins::Specs::Base]
      # @param current_info [Hash, nil]
      def initialize(target_spec, current_info)
        @target_spec = target_spec
        @current_info = current_info
        @uptodate =
          if current_info
            target_spec.check_info(current_info)
          else
            false
          end
      end

      def uptodate?
        @uptodate
      end

      def outdated?
        !uptodate?
      end

      # @return [String, nil]
      def target_version
        target_spec.version
      end

      # @return [String, nil]
      def current_version
        target_spec.version_info(current_info) || 'unknown'
      end
    end

    # @return [Machine]
    attr_reader :machine

    # @return [Boolean]
    attr_reader :status

    # @return [Boolean]
    attr_reader :online
    alias online? online

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
    attr_accessor :target_swpin_specs

    # @return [Hash]
    attr_reader :swpins_info

    # @return [Hash]
    attr_reader :swpins_state

    # @param machine [Machine]
    def initialize(machine)
      @machine = machine
      @mc = MachineControl.new(machine.carried? ? machine.carrier_machine : machine)
    end

    # Connect to the machine and query its state
    def query(toplevel: true, generations: true)
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
          @generations = Generation::HostList.fetch(mc, profile: machine.profile)
        rescue TTY::Command::ExitError
          return
        end
      end

      begin
        @swpins_info = query_swpins
      rescue Error
        nil
      end
    end

    def evaluate
      @swpins_state = {}

      target_swpin_specs.each do |name, spec|
        swpins_state[name] = SwpinState.new(spec, swpins_info && swpins_info[name])
      end

      outdated_swpins = swpins_state.detect { |_k, v| v.outdated? }
      @online = uptime ? true : false
      @status = online? && !outdated_swpins
      @status = false if target_toplevel && target_toplevel != current_toplevel
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

      mc.read_symlink(path)
    end

    def query_swpins
      json =
        if machine.carried?
          query_carried_swpins
        else
          mc.read_file('/etc/confctl/swpins-info.json')
        end

      case json
      when String
        Swpins::DeployedInfo.parse!(json)
      when Hash
        json
      end
    end

    # @return [Hash, String]
    def query_carried_swpins
      begin
        json = mc.read_file(File.join(machine.profile, 'machine.json'))
        parsed = JSON.parse(json)
        return parsed['swpins-info'] if parsed['swpins-info']
      rescue TTY::Command::ExitError
        # pass
      end

      begin
        return mc.read_file(File.join(machine.profile, '/etc/confctl/swpins-info.json'))
      rescue TTY::Command::ExitError
        # pass
      end

      nil
    end
  end
end
