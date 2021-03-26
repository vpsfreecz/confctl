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

    protected
    def select_generations(deps, pattern)
      gens = []

      deps.each do |host, dep|
        ConfCtl::BuildGenerationList.new(host).each do |gen|
          if pattern.nil? \
             || ConfCtl::Pattern.match?(pattern, gen.name) \
             || (pattern == 'old' && !gen.current)
            gens << gen
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
          'current' => gen.current,
        }

        gen.swpin_specs.each do |name, spec|
          row[name] = spec.version
        end

        row
      end

      OutputFormatter.print(
        rows,
        %w(host name current) + swpin_names,
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
