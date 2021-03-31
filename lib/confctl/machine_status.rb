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
        current_info && target_spec.version_info(current_info)
      end
    end

    # @return [Deployment]
    attr_reader :deployment

    # @return [Boolean]
    attr_reader :status

    # @return [Boolean]
    attr_reader :online
    alias_method :online?, :online

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

    # @param deployment [Deployment]
    def initialize(deployment)
      @deployment = deployment
      @mc = MachineControl.new(deployment)
    end

    # Connect to the machine and query its state
    def query(toplevel: true, generations: true)
      begin
        @uptime = mc.get_uptime
      rescue CommandFailed
        return
      end

      if toplevel
        begin
          @current_toplevel = mc.read_symlink!('/run/current-system')
        rescue CommandFailed
          return
        end
      end

      if generations
        begin
          @generations = Generation::HostList.fetch(mc)
        rescue CommandFailed
          return
        end
      end

      begin
        @swpins_info = Swpins::DeployedInfo.parse!(mc.read_file!('/etc/confctl/swpins-info.json'))
      rescue Error
        return
      end
    end

    def evaluate
      @swpins_state = {}

      target_swpin_specs.each do |name, spec|
        swpins_state[name] = SwpinState.new(spec, swpins_info && swpins_info[name])
      end

      outdated_swpins = swpins_state.detect { |k, v| v.outdated? }
      @online = uptime ? true : false
      @status = online? && !outdated_swpins
      @status = false if target_toplevel && target_toplevel != current_toplevel
    end

    protected
    attr_reader :mc
  end
end
