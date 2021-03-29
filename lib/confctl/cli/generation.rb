require 'time'

module ConfCtl::Cli
  class Generation < Command
    def list
      deps = select_deployments(args[0])
      gens = select_generations(deps, args[1])
      list_generations(gens)
    end

    def remove
      deps = select_deployments(args[0])
      gens = select_generations(deps, args[1])

      if gens.empty?
        puts 'No generations found'
        return
      end

      ask_confirmation! do
        puts 'The following generations will be removed:'
        list_generations(gens)
      end

      gens.each do |gen|
        puts "Removing generation #{gen.host}@#{gen.name}"
        gen.destroy
      end
    end

    def autoremove
      deps = select_deployments(args[0])

      global = ConfCtl::Settings.instance.build_generations
      to_delete = []

      deps.each do |host, dep|
        min = dep['buildGenerations']['min'] || global['min']
        max = dep['buildGenerations']['max'] || global['max']
        maxAge = dep['buildGenerations']['maxAge'] || global['maxAge']

        gens = ConfCtl::Generation::BuildList.new(host)
        next if gens.count <= min

        dep_deleted = 0

        gens.each do |gen|
          next if gen.current

          if gen.date + maxAge < Time.now
            to_delete << {host: gen.host, name: gen.name, generation: gen}
            dep_deleted += 1
          end

          break if gens.count - dep_deleted <= min
        end
      end

      if to_delete.empty?
        puts 'No generations to delete'
        return
      end

      ask_confirmation! do
        puts "The following generations will be removed:"
        to_delete.each do |gen|
          OutputFormatter.print(to_delete, %i(host name), layout: :columns)
        end
      end

      to_delete.each do |gen|
        puts "Removing generation #{gen[:host]}@#{gen[:name]}"
        gen[:generation].destroy
      end
    end

    protected
    def select_generations(deps, pattern)
      gens = ConfCtl::Generation::UnifiedList.new
      include_remote = opts[:remote]
      include_local = opts[:local] || (!opts[:remote] && !opts[:local])

      if include_remote
        tw = ConfCtl::ParallelExecutor.new(deps.length)
        statuses = {}

        deps.each do |host, dep|
          st = ConfCtl::MachineStatus.new(dep)
          statuses[host] = st

          tw.add do
            st.query(toplevel: false)
          end
        end

        tw.run

        statuses.each do |host, st|
          gens.add_host_generations(st.generations) if st.generations
        end
      end

      if include_local
        deps.each do |host, dep|
          gens.add_build_generations(ConfCtl::Generation::BuildList.new(host))
        end
      end

      if pattern
        gens.delete_if do |gen|
          if pattern == 'old'
            gen.current
          else
            !ConfCtl::Pattern.match?(pattern, gen.name)
          end
        end
      end

      gens
    end

    def list_generations(gens)
      swpin_names = []

      gens.each do |gen|
        gen.swpin_names.each do |name|
          swpin_names << name unless swpin_names.include?(name)
        end
      end

      rows = gens.map do |gen|
        row = {
          'host' => gen.host,
          'name' => gen.name,
          'id' => gen.id,
          'presence' => gen.presence_str,
          'current' => gen.current_str,
        }

        gen.swpin_specs.each do |name, spec|
          row[name] = spec.version
        end

        row
      end

      OutputFormatter.print(
        rows,
        %w(host name id presence current) + swpin_names,
        layout: :columns,
        sort: %w(name host),
      )
    end

    def select_deployments(pattern)
      deps = ConfCtl::Deployments.new(show_trace: opts['show-trace'])

      attr_filters = AttrFilters.new(opts[:attr])
      tag_filters = TagFilters.new(opts[:tag])

      deps.select do |host, d|
        (pattern.nil? || ConfCtl::Pattern.match?(pattern, host)) \
          && attr_filters.pass?(d) \
          && tag_filters.pass?(d)
      end
    end
  end
end
