module ConfCtl
  class HealthChecks::Systemd::PropertyList < Hash
    def self.from_enumerator(it)
      hash = new

      it.each do |line|
        stripped = line.strip
        eq = line.index('=')
        next if eq.nil?

        k = line[0..(eq - 1)]
        v = line[(eq + 1)..-1]

        hash[k] = v
      end

      hash
    end
  end
end
