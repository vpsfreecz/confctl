require 'time'

module ConfCtl::Cli
  class Generation < Command
    def list
      machines = select_machines(args[0])
      gens = select_generations(machines, args[1])
      list_generations(gens)
    end

    def remove
      machines = select_machines(args[0])
      gens = select_generations(machines, args[1])

      if gens.empty?
        puts 'No generations found'
        return
      end

      ask_confirmation! do
        puts 'The following generations will be removed:'
        list_generations(gens)
      end

      gens.each do |gen|
        puts "Removing #{gen.presence_str} generation #{gen.host}@#{gen.name}"
        gen.destroy
      end
    end

    def rotate
      machines = select_machines(args[0])

      to_delete = []

      if opts[:remote]
        to_delete.concat(host_generations_rotate(machines))
      end

      if opts[:local] || (!opts[:local] && !opts[:remote])
        to_delete.concat(build_generations_rotate(machines))
      end

      if to_delete.empty?
        puts 'No generations to delete'
        return
      end

      ask_confirmation! do
        puts "The following generations will be removed:"
        OutputFormatter.print(to_delete, %i(host name type id), layout: :columns)
      end

      to_delete.each do |gen|
        puts "Removing #{gen[:type]} generation #{gen[:host]}@#{gen[:name]}"
        gen[:generation].destroy
      end
    end

    protected
    def select_generations(machines, pattern)
      gens = ConfCtl::Generation::UnifiedList.new
      include_remote = opts[:remote]
      include_local = opts[:local] || (!opts[:remote] && !opts[:local])

      if include_remote
        tw = ConfCtl::ParallelExecutor.new(machines.length)
        statuses = {}

        machines.each do |host, dep|
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
        machines.each do |host, dep|
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

    def build_generations_rotate(machines)
      global = ConfCtl::Settings.instance.build_generations
      ret = []

      machines.each do |host, dep|
        to_delete = generations_rotate(
          ConfCtl::Generation::BuildList.new(host),
          min: dep['buildGenerations']['min'] || global['min'],
          max: dep['buildGenerations']['max'] || global['max'],
          maxAge: dep['buildGenerations']['maxAge'] || global['maxAge'],
        ) do |gen|
          {
            name: gen.name,
            type: 'build',
          }
        end

        ret.concat(to_delete)
      end

      ret
    end

    def host_generations_rotate(machines)
      global = ConfCtl::Settings.instance.host_generations
      ret = []

      tw = ConfCtl::ParallelExecutor.new(machines.length)
      statuses = {}

      machines.each do |host, dep|
        st = ConfCtl::MachineStatus.new(dep)
        statuses[host] = st

        tw.add do
          st.query(toplevel: false)
        end
      end

      tw.run

      statuses.each do |host, st|
        next unless st.generations

        dep = st.deployment

        to_delete = generations_rotate(
          st.generations,
          min: dep['hostGenerations']['min'] || global['min'],
          max: dep['hostGenerations']['max'] || global['max'],
          maxAge: dep['hostGenerations']['maxAge'] || global['maxAge'],
        ) do |gen|
          {
            type: 'host',
            name: gen.approx_name,
            id: gen.id,
          }
        end

        ret.concat(to_delete)
      end

      ret
    end

    def generations_rotate(gens, min: nil, max: nil, maxAge: nil)
      ret = []

      return ret if gens.count <= min

      dep_deleted = 0

      gens.each do |gen|
        next if gen.current

        if (gens.count - dep_deleted) > max || (gen.date + maxAge) < Time.now
          ret << {
            host: gen.host,
            generation: gen,
          }.merge(yield(gen))
          dep_deleted += 1
        end

        break if gens.count - dep_deleted <= min
      end

      ret
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

    def select_machines(pattern)
      machines = ConfCtl::MachineList.new(show_trace: opts['show-trace'])

      attr_filters = AttrFilters.new(opts[:attr])
      tag_filters = TagFilters.new(opts[:tag])

      machines.select do |host, d|
        (pattern.nil? || ConfCtl::Pattern.match?(pattern, host)) \
          && attr_filters.pass?(d) \
          && tag_filters.pass?(d)
      end
    end
  end
end
