require 'confctl/utils/file'

module ConfCtl
  class Generation::BuildList
    include Utils::File

    # @return [String]
    attr_reader :host

    # @return [Generation::Build, nil]
    attr_reader :current

    # @return [String]
    def initialize(host)
      @host = host
      @generations = []
      @index = {}

      return unless Dir.exist?(dir)

      Dir.entries(dir).each do |v|
        abs_path = File.join(dir, v)
        next if %w(. ..).include?(v) || !Dir.exist?(abs_path) || File.symlink?(abs_path)

        gen = Generation::Build.new(host)

        begin
          gen.load(v)
        rescue Error => e
          warn "Ignoring invalid generation #{gen.dir}"
          next
        end

        generations << gen
        index[gen.name] = gen
      end

      current_gen =
        if File.exist?(current_symlink)
          name = File.basename(File.readlink(current_symlink))
          index[name] || generations.last
        else
          generations.last
        end

      change_current(current_gen) if current_gen
    end

    # @param name [String]
    def [](name)
      index[name]
    end

    def each(&block)
      generations.each(&block)
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
      change_current(gen)
      generations << gen unless generations.include?(gen)
      replace_symlink(current_symlink, gen.name)
    end

    # @param toplevel [String]
    # @param swpin_paths [Hash]
    # @return [Generation::Build, nil]
    def find(toplevel, swpin_paths)
      generations.detect do |gen|
        gen.toplevel == toplevel && gen.swpin_paths == swpin_paths
      end
    end

    protected
    attr_reader :generations, :index

    def dir
      @dir ||= File.join(ConfCtl.generation_dir, ConfCtl.safe_host_name(host))
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
