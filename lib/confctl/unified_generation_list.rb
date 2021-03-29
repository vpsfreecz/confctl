module ConfCtl
  class UnifiedGeneration
    # @return [String]
    attr_reader :host

    # @return [String]
    attr_reader :name

    # @return [Integer, nil]
    attr_reader :id

    # @return [String]
    attr_reader :toplevel

    # @return [Time]
    attr_reader :date

    # @return [Boolean]
    attr_reader :current

    # @return [BuildGeneration]
    attr_reader :build_generation

    # @return [HostGeneration]
    attr_reader :host_generation

    # @param host [String]
    # @param build_generation [BuildGeneration]
    # @param host_generation [HostGeneration]
    def initialize(host, build_generation: nil, host_generation: nil)
      @host = host
      @build_generation = build_generation
      @host_generation = host_generation
      @id = host_generation && host_generation.id

      if build_generation
        @name = build_generation.name
        @toplevel = build_generation.toplevel
        @date = build_generation.date
        @current ||= build_generation.current
      elsif host_generation
        @name = host_generation.date.strftime('%Y-%m-%d--%H-%M-%S')
        @toplevel = host_generation.toplevel
        @date = host_generation.date
        @current ||= host_generation.current
      else
        raise ArgumentError, 'set build or host'
      end
    end

    # @param gen [BuildGeneration]
    def set_build_generation(gen)
      @build_generation = gen
      @name = gen.name
      @date = gen.date
      @current ||= gen.current
    end

    # @param gen [HostGeneration]
    def set_host_generation(gen)
      @host_generation = gen
      @id = gen.id
      @current ||= gen.current
    end

    %i(swpin_names swpin_specs).each do |v|
      define_method(v) do
        build_generation ? build_generation.send(v) : []
      end
    end

    def current_str
      build = build_generation && build_generation.current
      host = host_generation && host_generation.current

      if build && host
        'build+host'
      elsif build
        'build'
      elsif host
        'host'
      else
        nil
      end
    end

    def presence_str
      if build_generation && host_generation
        'build+host'
      elsif build_generation
        'build'
      elsif host_generation
        'host'
      else
        fail 'programming error'
      end
    end

    def destroy
      build_generation.destroy if build_generation
      host_generation.destroy if host_generation
      true
    end
  end

  class UnifiedGenerationList
    def initialize
      @generations = []
    end

    # @param generation [BuildGeneration]
    def add_build_generation(generation)
      unified = generations.detect do |g|
        g.host == generation.host && g.toplevel == generation.toplevel
      end

      if unified
        unified.set_build_generation(generation)
      else
        generations << UnifiedGeneration.new(generation.host, build_generation: generation)
      end

      true
    end

    # @param generations [BuildGenerationList]
    def add_build_generations(generations)
      generations.each { |v| add_build_generation(v) }
      true
    end

    # @param generation [HostGeneration]
    def add_host_generation(generation)
      unified = generations.detect do |g|
        g.host == generation.host && g.toplevel == generation.toplevel
      end

      if unified
        unified.set_host_generation(generation)
      else
        generations << UnifiedGeneration.new(generation.host, host_generation: generation)
      end

      true
    end

    # @param generations [HostGenerationList]
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
