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
      )
    end

    def set
      require_args!('cluster-name', 'sw', 'version...')
      files = []

      each_cluster_name_spec(args[0], args[1]) do |cluster_name, spec|
        if spec.from_channel?
          puts "Skipping #{spec.name} as it comes from channel #{spec.channel}"
        else
          puts "Configuring #{spec.name} in #{cluster_name.name}"
          spec.prefetch_set(args[2..])
          files << file unless files.include?(file)
        end
      end

      files.each(&:save)
    end

    def update
      files = []

      each_cluster_name_spec(args[0] || '*', args[1] || '*') do |cluster_name, spec|
        if spec.from_channel?
          puts "Skipping #{spec.name} as it comes from channel #{spec.channel}"
        elsif spec.can_update?
          puts "Updating #{spec.name} in #{cluster_name.name}"
          spec.prefetch_update
          files << file unless files.include?(file)
        else
          puts "#{spec.name} not configured for update"
        end
      end

      files.each(&:save)
    end

    protected
    def git_set(file_pattern, sw_pattern, ref)
      files = []

      each_file_spec(file_pattern, sw_pattern) do |file, spec|
        puts "Updating #{spec.name} to #{ref} in #{file.name}"
        spec.prefetch(ref: ref)
        files << file unless files.include?(file)
      end

      files.each(&:save)
    end
  end
end
