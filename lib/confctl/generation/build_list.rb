require 'confctl/utils/file'

module ConfCtl
  class Generation::BuildList
    include Utils::File

    # @return [String]
    attr_reader :host

    # @return [Generation::Build, nil]
    attr_reader :current, :rejected, :current_error

    # @return [String]
    def initialize(host)
      @host = host
      @generations = []
      @index = {}
      @rejected = {}

      return unless Dir.exist?(dir)

      Dir.entries(dir).each do |v|
        abs_path = File.join(dir, v)
        next if %w[. ..].include?(v) || !Dir.exist?(abs_path) || File.symlink?(abs_path)

        gen = Generation::Build.new(host)

        begin
          gen.load(v)
        rescue Error => e
          rejected[v] = { path: gen.dir, reason: e.message }
          report("Skipping generation #{gen.dir}: #{e.message}. Records and GC roots were left untouched; use confctl v3 for old software-pin generations.")
          next
        end

        generations << gen
        index[gen.name] = gen
      end

      generations.sort! do |a, b|
        a.date <=> b.date
      end

      if File.symlink?(current_symlink)
        name = File.basename(File.readlink(current_symlink))
        current_gen = index[name]
        if current_gen
          begin
            unless File.realpath(current_symlink) == File.realpath(current_gen.dir)
              @current_error = "#{current_symlink}: target does not resolve to local generation #{name}"
              current_gen = nil
            end
          rescue SystemCallError => e
            @current_error = "#{current_symlink}: cannot resolve current target: #{e.message}"
            current_gen = nil
          end
        else
          @current_error = rejected.dig(name, :reason) || "#{current_symlink}: missing generation #{name}"
        end
      elsif File.exist?(current_symlink)
        @current_error = "#{current_symlink}: expected a symlink"
      else
        current_gen = generations.last
        if current_gen && rejected.any?
          report("No current link for #{host}; selecting newest supported generation #{current_gen.name}. Excluded records and GC roots were left untouched.")
        end
      end

      change_current(current_gen) if current_gen
    end

    def select_current!
      require_resolved_current!
      current
    end

    def require_resolved_current!
      raise Error, "Cannot select current generation for #{host}: #{current_error}" if current_error
    end

    def check_selection!(name)
      if name && /\A-?\d+\z/.match?(name.to_s) && rejected.any?
        raise Error, "Numeric generation selection for #{host} is ambiguous because records were excluded; select an explicit supported generation name"
      end

      require_resolved_current! if name == 'current'
      rejection = rejected[name]
      raise Error, "Cannot select generation #{name} for #{host}: #{rejection[:reason]}" if rejection
    end

    # @param name [String]
    def [](name)
      check_selection!(name)
      index[name]
    end

    # @param offset [Integer] 0 = current/last, 1 = first (oldest), -1 = before last
    # @return [Generation::Build]
    def at_offset(offset)
      check_selection!(offset.to_s)
      if offset == 0
        generations.last
      else
        generations[offset - 1]
      end
    end

    def each(&)
      generations.each(&)
    end

    # @return [Array<Generation::Build>]
    def to_a
      generations.clone
    end

    # @return [Integer]
    def count
      generations.length
    end

    # @param gen [Generation::Build]
    def current=(gen)
      @current_error = nil
      change_current(gen)
      index[gen.name] = gen
      generations << gen unless generations.include?(gen)
      replace_symlink(current_symlink, gen.name)
    end

    # @param toplevel [String]
    # @param inputs [Hash]
    # @return [Generation::Build, nil]
    def find(toplevel, inputs)
      generations.detect do |gen|
        gen.toplevel == toplevel && gen.inputs == inputs
      end
    end

    protected

    attr_reader :generations, :index

    def report(message)
      warn message
      logger = Logger.instance
      logger << "#{message}\n" if logger.open?
    end

    def dir
      @dir ||= File.join(ConfDir.generation_dir, ConfCtl.safe_host_name(host))
    end

    def current_symlink
      @current_symlink ||= File.join(dir, 'current')
    end

    def change_current(gen)
      @current = gen
      generations.each { |g| g.current = false }
      gen.current = true
    end
  end
end
