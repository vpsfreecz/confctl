require 'confctl/cli/command'
require 'confctl/cli/swpins/utils'

module ConfCtl::Cli
  class Swpins::Cluster < Command
    include Swpins::Utils

    def list
      rows = []

      each_cluster_name_spec(args[0] || '*', args[1] || '*') do |cn, spec|
        rows << {
          cluster_name: cn.name,
          sw: spec.name,
          channel: spec.channel,
          type: spec.type,
          pin: spec.status,
        }
      end

      OutputFormatter.print(
        rows,
        %i(cluster_name sw channel type pin),
        layout: :columns,
      )
    end

    def set
      require_args!('cluster-name', 'sw', 'version...', strict: false)
      cluster_names = []
      change_set = ConfCtl::Swpins::ChangeSet.new

      each_cluster_name_spec(args[0], args[1]) do |cluster_name, spec|
        if spec.from_channel?
          puts "Skipping #{spec.name} as it comes from channel #{spec.channel}"
        else
          change_set.add(cluster_name, spec)
          spec_set_msg(cluster_name, spec) { spec.prefetch_set(args[2..-1]) }
          cluster_names << cluster_name unless cluster_names.include?(cluster_name)
        end
      end

      cluster_names.each(&:save)

      if opts[:commit]
        change_set.commit(
          type: opts[:downgrade] ? :downgrade : :upgrade,
          changelog: opts[:changelog],
        )
      end
    end

    def update
      require_args!(optional: %w(cluster-name sw))
      cluster_names = []
      change_set = ConfCtl::Swpins::ChangeSet.new

      each_cluster_name_spec(args[0] || '*', args[1] || '*') do |cluster_name, spec|
        if spec.from_channel?
          puts "Skipping #{spec.name} as it comes from channel #{spec.channel}"
        elsif spec.can_update?
          change_set.add(cluster_name, spec)
          spec_update_msg(cluster_name, spec) { spec.prefetch_update }
          cluster_names << cluster_name unless cluster_names.include?(cluster_name)
        else
          puts "#{spec.name} not configured for update"
        end
      end

      cluster_names.each(&:save)

      if opts[:commit]
        change_set.commit(
          type: opts[:downgrade] ? :downgrade : :upgrade,
          changelog: opts[:changelog],
        )
      end
    end
  end
end
