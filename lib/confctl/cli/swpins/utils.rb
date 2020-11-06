module ConfCtl::Cli
  module Swpins::Utils
    def cluster_name_list(pattern)
      channels = channel_list('*')
      channels.each(&:parse)

      ConfCtl::Swpins::ClusterNameList.new(
        Swpins::Cluster::DIR,
        channels,
        pattern: pattern,
      )
    end

    def each_cluster_name(cn_pattern)
      cluster_name_list(cn_pattern).each do |cn|
        cn.parse
        yield(cn)
      end
    end

    def each_cluster_name_spec(cn_pattern, sw_pattern)
      each_cluster_name(cn_pattern) do |cn|
        cn.specs.each do |name, spec|
          yield(cn, spec) if ConfCtl::Pattern.match?(sw_pattern, name)
        end
      end
    end

    def channel_list(pattern)
      ConfCtl::Swpins::ChannelList.new(
        Swpins::Channel::DIR,
        pattern: pattern,
      )
    end

    def each_channel(chan_pattern)
      channel_list(chan_pattern).each do |chan|
        chan.parse
        yield(chan)
      end
    end

    def each_channel_spec(chan_pattern, sw_pattern)
      each_channel(chan_pattern) do |chan|
        chan.specs.each do |name, spec|
          yield(chan, spec) if ConfCtl::Pattern.match?(sw_pattern, name)
        end
      end
    end
  end
end
