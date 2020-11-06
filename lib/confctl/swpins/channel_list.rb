module ConfCtl
  class Swpins::ChannelList < Array
    # @param channel_dir [String]
    # @param cluster_file_dir [String]
    # @param pattern [String]
    def initialize(channel_dir, pattern: '*')
      nix = Nix.new
      nix.list_swpins_channels.each do |name, nix_specs|
        next unless Pattern.match?(pattern, name)

        self << Swpins::Channel.new(channel_dir, name, nix_specs)
      end
    end
  end
end
