module ConfCtl
  class Generation::HostList
    # @parma machine [Machine]
    # @param mc [MachineControl]
    # @param profile [String]
    # @return [Generation::HostList]
    def self.fetch(machine, mc, profile:)
      out, = mc.bash_script(<<~END)
        realpath #{profile}

        for generation in `ls -d -1 #{profile}-*-link` ; do
          echo -n "$generation;"
          echo -n "$(readlink $generation);"
          echo -n "$(stat --format=%Y $generation);"

          for kernel_file in kernel bzImage ; do
            [ -h "$generation/$kernel_file" ] && echo -n $(readlink "$generation/$kernel_file")
          done

          echo
        done
      END

      list = new(machine.name)
      lines = out.strip.split("\n")
      current_path = lines.shift
      id_rx = /^#{Regexp.escape(profile)}-(\d+)-link$/

      lines.each do |line|
        link, path, created_at, kernel = line.split(';')

        if id_rx =~ link
          id = ::Regexp.last_match(1).to_i
        else
          warn "Invalid profile generation link '#{link}'"
          next
        end

        kernel_version =
          if kernel && %r{\A/nix/store/[^-]+-linux-([^/]+)} =~ kernel
            ::Regexp.last_match(1)
          end

        list << Generation::Host.new(
          machine,
          profile,
          id,
          path,
          Time.at(created_at.to_i),
          kernel_version,
          current: path == current_path,
          mc:
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

    def each(&)
      generations.each(&)
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
