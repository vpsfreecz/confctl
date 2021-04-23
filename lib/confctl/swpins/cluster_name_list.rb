module ConfCtl
  class Swpins::ClusterNameList < Array
    # @param channels [Swpins::ChannelList]
    # @param pattern [String]
    # @param machines [MachineList]
    def initialize(channels: nil, pattern: '*', machines: nil)
      channels ||= ConfCtl::Swpins::ChannelList.get

      (machines || MachineList.new).each do |name, dep|
        if Pattern.match?(pattern, dep.name)
          self << Swpins::ClusterName.new(dep, channels)
        end
      end
    end
  end
end
