module ConfCtl
  class Generation::Host
    attr_reader :host, :profile, :id, :toplevel, :date, :kernel_version, :current

    # @param machine [Machine]
    # @param profile [String]
    # @param id [Integer]
    # @param toplevel [String]
    # @param date [Time]
    # @param kernel_version [String, nil]
    # @param mc [MachineControl]
    def initialize(machine, profile, id, toplevel, date, kernel_version, current: false, mc: nil)
      @host = machine.name
      @machine = machine
      @profile = profile
      @id = id
      @toplevel = toplevel
      @date = date
      @kernel_version = kernel_version
      @current = current
      @mc = mc
    end

    def approx_name
      @approx_name ||= date.strftime('%Y-%m-%d--%H-%M-%S')
    end

    def destroy
      raise 'machine control not available' if mc.nil?

      env_cmd = @machine.carried? ? 'carrier-env' : 'nix-env'
      mc.execute(env_cmd, '-p', profile, '--delete-generations', id.to_s)
    end

    protected

    attr_reader :mc
  end
end
