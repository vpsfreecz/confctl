module ConfCtl
  class Swpins::ClusterNameList < Array
    # @param channels [Swpins::ChannelList]
    # @param pattern [String]
    # @param deployments [Deployments]
    def initialize(channels: nil, pattern: '*', deployments: nil)
      if channels.nil?
        channels = ConfCtl::Swpins::ChannelList.new
        channels.each(&:parse)
      end

      (deployments || Deployments.new).each do |name, dep|
        if Pattern.match?(pattern, dep.name)
          self << Swpins::ClusterName.new(dep, channels)
        end
      end
    end
  end
end
