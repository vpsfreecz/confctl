module ConfCtl
  class Swpins::ChannelList < Array
    # @return [Swpins::ChannelList]
    def self.get
      @get ||= new
    end

    # @param pattern [String]
    def self.pattern(pattern)
      get.pattern(pattern)
    end

    def self.refresh
      get.refresh
    end

    # @param pattern [String]
    def initialize(pattern: '*')
      super()
      parse(pattern:)
    end

    # @param pattern [String]
    # @return [Array<Swpins::Channel>]
    def pattern(pattern)
      select { |c| Pattern.match?(pattern, c.name) }
    end

    def refresh
      clear
      parse
    end

    protected

    def parse(pattern: '*')
      nix = Nix.new
      nix.list_swpins_channels.each do |name, nix_specs|
        next unless Pattern.match?(pattern, name)

        c = Swpins::Channel.new(name, nix_specs)
        c.parse
        self << c
      end
    end
  end
end
