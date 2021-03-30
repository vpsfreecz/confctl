module ConfCtl
  class Generation::HostList
    # @param mc [MachineControl]
    # @return [Generation::HostList]
    def self.fetch(mc, profile: '/nix/var/nix/profiles/system')
      str = mc.bash_script_read!(<<-END
        realpath #{profile}

        for generation in `ls -d -1 #{profile}-*-link` ; do
          echo "$generation;$(readlink $generation);$(stat --format=%Y $generation)"
        done
        END
      ).output

      list = new(mc.deployment.name)
      lines = str.strip.split("\n")
      current_path = lines.shift
      id_rx = /^#{Regexp.escape(profile)}\-(\d+)\-link$/

      lines.each do |line|
        link, path, created_at = line.split(';')

        if id_rx =~ link
          id = $1.to_i
        else
          warn "Invalid profile generation link '#{link}'"
          next
        end

        list << Generation::Host.new(
          mc.deployment.name,
          profile,
          id,
          path,
          Time.at(created_at.to_i),
          current: path == current_path,
          mc: mc,
        )
      end

      list.sort
      list
    end

    # @return [String]
    attr_reader :host

    # @param host [String]
    def initialize(host)
      @host = host
      @generations = []
    end

    # @param generation [Generation::Host]
    def <<(generation)
      generations << generation
    end

    def sort
      generations.sort! { |a, b| a.id <=> b.id }
    end

    def each(&block)
      generations.each(&block)
    end

    # @return [Integer]
    def count
      generations.length
    end

    # @return [Generation::Host]
    def current
      generations.detect(&:current)
    end

    protected
    attr_reader :generations
  end
end