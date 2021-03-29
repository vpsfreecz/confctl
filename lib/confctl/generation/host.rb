module ConfCtl
  class Generation::Host
    attr_reader :host

    attr_reader :profile

    attr_reader :id

    attr_reader :toplevel

    attr_reader :date

    attr_reader :current

    # @param host [String]
    # @param profile [String]
    # @param id [Integer]
    # @param toplevel [String]
    # @param date [Time]
    # @param mc [MachineControl]
    def initialize(host, profile, id, toplevel, date, current: false, mc: nil)
      @host = host
      @profile = profile
      @id = id
      @toplevel = toplevel
      @date = date
      @current = current
      @mc = mc
    end

    def approx_name
      @approx_name ||= date.strftime('%Y-%m-%d--%H-%M-%S')
    end

    def destroy
      fail 'machine control not available' if mc.nil?

      mc.execute!('nix-env', '-p', profile, '--delete-generations', id.to_s)
    end

    protected
    attr_reader :mc
  end
end
