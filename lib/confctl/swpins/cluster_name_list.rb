module ConfCtl
  class Swpins::ClusterNameList < Array
    # @param channels [Swpins::ChannelList]
    # @param pattern [String]
    # @param machines [MachineList]
    def initialize(channels: nil, pattern: '*', machines: nil)
      channels ||= ConfCtl::Swpins::ChannelList.get

      (machines || MachineList.new).each do |name, machine|
        if Pattern.match?(pattern, machine.name)
          self << Swpins::ClusterName.new(machine, channels)
        end
      end
    end
  end
end
