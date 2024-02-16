module ConfCtl::Cli
  class AttrFilters
    # @param str_filters [Array<String>]
    def initialize(str_filters)
      @filters = parse_all(str_filters)
    end

    # @param machine [ConfCtl::Machine]
    def pass?(machine)
      filters.all? { |f| f.call(machine) }
    end

    protected

    attr_reader :filters

    def parse_all(str_filters)
      ret = []

      str_filters.each do |s|
        k, v = parse_one(s, '!=')
        if k
          ret << proc do |machine|
            machine[k].to_s != v
          end
          next
        end

        k, v = parse_one(s, '=')
        if k
          ret << proc do |machine|
            machine[k].to_s == v
          end
          next
        end

        raise GLI::BadCommandLine, "Invalid filter '#{v}'"
      end

      ret
    end

    def parse_one(v, sep)
      i = v.index(sep)
      return false unless i

      len = sep.length
      [v[0..i - 1], v[i + len..-1]]
    end
  end
end
