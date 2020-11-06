module ConfCtl
  class Swpins::ChannelList < Array
    # @param pattern [String]
    def initialize(pattern: '*')
      nix = Nix.new
      nix.list_swpins_channels.each do |name, nix_specs|
        next unless Pattern.match?(pattern, name)

        self << Swpins::Channel.new(name, nix_specs)
      end
    end
  end
end
