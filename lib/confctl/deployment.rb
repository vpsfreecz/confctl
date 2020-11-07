module ConfCtl
  class Deployment
    attr_reader :name, :safe_name, :managed, :spin, :opts

    # @param opts [Hash]
    def initialize(opts)
      @opts = opts
      @name = opts['name']
      @safe_name = opts['name'].gsub(/\//, ':')
      @managed = opts['managed']
      @spin = opts['spin']
    end

    def target_host
      (opts['host'] && opts['host']['target']) || name
    end

    def localhost?
      target_host == 'localhost'
    end

    def nix_paths
      Hash[opts['nix']['nixPath'].map do |v|
        eq = v.index('=')
        fail "'#{v}' is not a valid nix path entry " if eq.nil?
        [v[0..eq-1], v[eq+1..-1]]
      end]
    end

    def [](key)
      if key.index('.')
        get(opts, key.split('.'))
      else
        opts[key]
      end
    end

    def to_s
      name
    end

    protected
    def get(hash, keys)
      k = keys.shift

      if hash.has_key?(k)
        if keys.empty?
          hash[k]
        elsif hash[k].nil?
          nil
        else
          get(hash[k], keys)
        end
      else
        nil
      end
    end
  end
end
