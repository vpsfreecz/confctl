module ConfCtl
  class Swpins::ClusterNameList < Array
    # @param dir [String]
    # @param channels [Swpins::ChannelList]
    # @param pattern [String]
    def initialize(dir, channels, pattern: '*')
      Deployments.new.each do |name, dep|
        if Pattern.match?(pattern, dep.name)
          self << Swpins::ClusterName.new(dir, dep, channels)
        end
      end
    end
  end
end
