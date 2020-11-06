module ConfCtl
  class Swpins::ClusterNameList < Array
    # @param channels [Swpins::ChannelList]
    # @param pattern [String]
    def initialize(channels, pattern: '*')
      Deployments.new.each do |name, dep|
        if Pattern.match?(pattern, dep.name)
          self << Swpins::ClusterName.new(dep, channels)
        end
      end
    end
  end
end
