module ConfCtl
  class HostGeneration
    attr_reader :host

    attr_reader :id

    attr_reader :toplevel

    attr_reader :date

    attr_reader :current

    # @param host [String]
    # @param id [Integer]
    # @param toplevel [String]
    # @param date [Time]
    def initialize(host, id, toplevel, date, current: false)
      @host = host
      @id = id
      @toplevel = toplevel
      @date = date
      @current = current
    end
  end
end
