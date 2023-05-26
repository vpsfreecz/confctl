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
        puts
        puts "Garbage collection: #{opts[:gc] ? 'yes' : 'no'}"
      end

      gens.each do |gen|
        puts "Removing #{gen.presence_str} generation #{gen.host}@#{gen.name}"
        gen.destroy
      end

      if opts[:gc]
        machines_gc = machines.select do |host, machine|
          gens.detect { |gen| gen.host == host }
        end

        run_gc(machines_gc)
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

      global = ConfCtl::Settings.instance.host_generations

      machines_gc = machines.select do |host, machine|
        gc = machine['buildGenerations']['collectGarbage']

        if gc.nil?
          global['collectGarbage']
        else
          gc
        end
      end

      run_gc(machines_gc) if machines_gc.any?
    end

    protected
    def select_generations(machines, pattern)
      gens = ConfCtl::Generation::UnifiedList.new
      include_remote = opts[:remote]
      include_local = opts[:local] || (!opts[:remote] && !opts[:local])

      if include_remote
        tw = ConfCtl::ParallelExecutor.new(machines.length)
        statuses = {}

        machines.each do |host, machine|
          st = ConfCtl::MachineStatus.new(machine)
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
        machines.each do |host, machine|
          gens.add_build_generations(ConfCtl::Generation::BuildList.new(host))
        end
      end

      select_old = pattern == 'old'
      select_older_than =
        if !select_old && /\A(\d+)d\Z/ =~ pattern
          Time.now - ($1.to_i * 24*60*60)
        else
          nil
        end

      if pattern
        gens.delete_if do |gen|
          if select_old
            gen.current
          elsif select_older_than
            gen.date >= select_older_than
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

      machines.each do |host, machine|
        to_delete = generations_rotate(
          ConfCtl::Generation::BuildList.new(host),
          min: machine['buildGenerations']['min'] || global['min'],
          max: machine['buildGenerations']['max'] || global['max'],
          maxAge: machine['buildGenerations']['maxAge'] || global['maxAge'],
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

      machines.each do |host, machine|
        st = ConfCtl::MachineStatus.new(machine)
        statuses[host] = st

        tw.add do
          st.query(toplevel: false)
        end
      end

      tw.run

      statuses.each do |host, st|
        next unless st.generations

        machine = st.machine

        to_delete = generations_rotate(
          st.generations,
          min: machine['hostGenerations']['min'] || global['min'],
          max: machine['hostGenerations']['max'] || global['max'],
          maxAge: machine['hostGenerations']['maxAge'] || global['maxAge'],
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

      machine_deleted = 0

      gens.each do |gen|
        next if gen.current

        if (gens.count - machine_deleted) > max || (gen.date + maxAge) < Time.now
          ret << {
            host: gen.host,
            generation: gen,
          }.merge(yield(gen))
          machine_deleted += 1
        end

        break if gens.count - machine_deleted <= min
      end

      ret
    end

    def run_gc(machines)
      nix = ConfCtl::Nix.new

      header =
        if machines.length > 1
          Rainbow("Collecting garbage on #{machines.length} machines").bright
        else
          Rainbow("Collecting gargabe on ").bright + Rainbow(machines.get_one.to_s).yellow
        end

      LogView.open(
        header: header + "\n",
        title: Rainbow("Live view").bright,
        size: :auto,
        reserved_lines: machines.length + 8,
      ) do |lw|
        multibar = TTY::ProgressBar::Multi.new(
          "Collecting garbage [:bar] :current",
          width: 80,
        )
        executor = ConfCtl::ParallelExecutor.new(opts['max-concurrent-gc'])

        machines.each do |host, machine|
          pb = multibar.register(
            "#{host} [:bar] :current"
          )

          executor.add do
            ret = nix.collect_garbage(machine) do |progress|
              lw << "#{host}> #{progress}"

              if progress.path?
                lw.sync_console do
                  pb.advance
                end
              end
            end

            lw.sync_console do
              if ret
                pb.finish
              else
                pb.format = "#{host}: error occurred"
                pb.advance
              end
            end

            ret ? nil : host
          end
        end

        retvals = executor.run
        failed = retvals.compact

        if failed.any?
          fail "Gargabe collection failed on: #{failed.join(', ')}"
        end
      end
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

      machines.select do |host, m|
        (pattern.nil? || ConfCtl::Pattern.match?(pattern, host)) \
          && attr_filters.pass?(m) \
          && tag_filters.pass?(m)
      end
    end
  end
end
