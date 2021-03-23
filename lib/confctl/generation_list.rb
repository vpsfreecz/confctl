module ConfCtl
  class GenerationList
    Generation = Struct.new(:id, :date, :current)

    # @param str [String] output of nix-env --list-generations
    # @return [GenerationList]
    def self.parse(str)
      list = new

      str.strip.split("\n").each do |line|
        id, date, time, current = line.strip.split

        list << Generation.new(
          id.to_i,
          # TODO: we might have the wrong timezone here
          Time.strptime("#{date} #{time}", '%Y-%m-%d %H:%M:%S'),
          current == '(current)',
        )
      end

      list
    end

    def initialize
      @generations = []
    end

    # @param generation [Generation]
    def <<(generation)
      generations << generation
    end

    # @return [Integer]
    def count
      generations.length
    end

    # @return [Generation]
    def current
      generations.detect(&:current)
    end

    protected
    attr_reader :generations
  end
end
