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
        puts "Garbage collection: #{opts[:remote] && opts[:gc] ? 'yes' : 'no'}"
      end

      gens.each do |gen|
        puts "Removing #{gen.presence_str} generation #{gen.host}@#{gen.name}"
        gen.destroy
      end

      return unless opts[:remote] && opts[:gc]

      machines_gc = machines.select do |host, _machine|
        gens.detect { |gen| gen.host == host }
      end

      run_gc(machines_gc)
    end

    def rotate
      machines = select_machines(args[0])

      to_delete = []

      to_delete.concat(host_generations_rotate(machines)) if opts[:remote]

      to_delete.concat(build_generations_rotate(machines)) if opts[:local] || (!opts[:local] && !opts[:remote])

      if to_delete.empty?
        puts 'No generations to delete'
        return
      end

      ask_confirmation! do
        puts 'The following generations will be removed:'
        OutputFormatter.print(to_delete, %i[host name type id], layout: :columns)
        puts
        puts "Garbage collection: #{opts[:remote] ? 'when enabled in configuration' : 'no'}"
      end

      to_delete.each do |gen|
        puts "Removing #{gen[:type]} generation #{gen[:host]}@#{gen[:name]}"
        gen[:generation].destroy
      end

      return unless opts[:remote]

      global = ConfCtl::Settings.instance.host_generations

      machines_gc = machines.select do |_host, machine|
        gc = machine['buildGenerations']['collectGarbage']

        if gc.nil?
          global['collectGarbage']
        else
          gc
        end
      end

      run_gc(machines_gc) if machines_gc.any?
    end

    def collect_garbage
      machines = select_machines(args[0])

      raise 'No machines to collect garbage on' if machines.empty?

      ask_confirmation! do
        puts 'Collect garbage on the following machines:'
        list_machines(machines)
      end

      run_gc(machines)
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

        statuses.each do |_host, st|
          gens.add_host_generations(st.generations) if st.generations
        end
      end

      if include_local
        machines.each do |host, _machine|
          gens.add_build_generations(ConfCtl::Generation::BuildList.new(host))
        end
      end

      select_old = pattern == 'old'
      select_older_than =
        (Time.now - (::Regexp.last_match(1).to_i * 24 * 60 * 60) if !select_old && /\A(\d+)d\Z/ =~ pattern)

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
          maxAge: machine['buildGenerations']['maxAge'] || global['maxAge']
        ) do |gen|
          {
            name: gen.name,
            type: 'build'
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

      statuses.each do |_host, st|
        next unless st.generations

        machine = st.machine

        to_delete = generations_rotate(
          st.generations,
          min: machine['hostGenerations']['min'] || global['min'],
          max: machine['hostGenerations']['max'] || global['max'],
          maxAge: machine['hostGenerations']['maxAge'] || global['maxAge']
        ) do |gen|
          {
            type: 'host',
            name: gen.approx_name,
            id: gen.id
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
            generation: gen
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
          Rainbow('Collecting garbage on ').bright + Rainbow(machines.get_one.to_s).yellow
        end

      LogView.open(
        header: header + "\n",
        title: Rainbow('Live view').bright,
        size: :auto,
        reserved_lines: machines.length + 8
      ) do |lw|
        multibar = TTY::ProgressBar::Multi.new(
          'Collecting garbage [:bar] :current',
          width: 80
        )
        executor = ConfCtl::ParallelExecutor.new(opts['max-concurrent-gc'])

        machines.each do |host, machine|
          pb = multibar.register(
            "#{host} [:bar] :current"
          )

          executor.add do
            end_stats = nil

            ret = nix.collect_garbage(machine) do |progress|
              lw << "#{host}> #{progress}"

              if progress.path?
                lw.sync_console do
                  pb.advance
                end

              elsif /^\d+ store paths deleted/ =~ progress.line
                end_stats = progress.line
              end
            end

            lw.sync_console do
              pb.format = if ret
                            "#{host}: #{end_stats || 'done'}"
                          else
                            "#{host}: error occurred"
                          end

              pb.finish
            end

            ret ? nil : host
          end
        end

        retvals = executor.run
        failed = retvals.compact

        raise "Gargabe collection failed on: #{failed.join(', ')}" if failed.any?
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
          'current' => gen.current_str
        }

        gen.swpin_specs.each do |name, spec|
          row[name] = spec.version
        end

        row
      end

      OutputFormatter.print(
        rows,
        %w[host name id presence current] + swpin_names,
        layout: :columns,
        sort: %w[name host]
      )
    end
  end
end
