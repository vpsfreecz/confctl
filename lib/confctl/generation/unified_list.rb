module ConfCtl
  class Generation::UnifiedList
    def initialize
      @generations = []
    end

    # @param generation [Generation::Build]
    def add_build_generation(generation)
      unified = generations.detect do |g|
        g.host == generation.host && g.toplevel == generation.toplevel
      end

      if unified
        unified.set_build_generation(generation)
      else
        generations << Generation::Unified.new(generation.host, build_generation: generation)
      end

      true
    end

    # @param generations [Generation::BuildList]
    def add_build_generations(generations)
      generations.each { |v| add_build_generation(v) }
      true
    end

    # @param generation [Generation::Host]
    def add_host_generation(generation)
      unified = generations.detect do |g|
        g.host == generation.host && g.toplevel == generation.toplevel
      end

      if unified
        unified.set_host_generation(generation)
      else
        generations << Generation::Unified.new(generation.host, host_generation: generation)
      end

      true
    end

    # @param generations [Generation::HostList]
    def add_host_generations(generations)
      generations.each { |v| add_host_generation(v) }
      true
    end

    def each(&block)
      generations.each(&block)
    end

    def delete_if(&block)
      generations.delete_if(&block)
    end

    def empty?
      generations.empty?
    end

    include Enumerable

    protected
    attr_reader :generations
  end
end
