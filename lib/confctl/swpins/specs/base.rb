module ConfCtl
  class Swpins::Specs::Base
    class << self
      attr_reader :type

      # @param name [Symbol]
      def handle(name)
        @type = name
        Swpins::Spec.register(name, self)
      end
    end

    # @return [String]
    attr_reader :name

    # @param [String]
    # @return [String, nil]
    attr_accessor :channel

    # @return [Array]
    attr_reader :errors

    # @param name [String]
    # @param nix_opts [Hash] options from the user's nix configuration
    # @param json_opts [Hash] options from generated json file
    def initialize(name, nix_opts, json_opts)
      @name = name
      @nix_opts = nix_opts
      @json_opts = json_opts
      @state = nil
      @info = nil
      @fetcher = nil
      @fetcher_opts = nil
      @errors = []
      parse
    end

    def type
      self.class.type
    end

    # Check that current nix options match those that generated json options
    # @return [Boolean]
    def check_opts
      raise NotImplementedError
    end

    def valid?
      errors.empty?
    end

    def can_update?
      nix_opts['update']['ref'] ? true : false
    end

    def auto_update?
      can_update? && nix_opts['update']['auto']
    end

    def from_channel?
      !channel.nil?
    end

    def status
      valid? ? version : 'update'
    end

    def version
      raise NotImplementedError
    end

    def prefetch_set(args)
      raise NotImplementedError
    end

    def prefetch_update
      raise NotImplementedError
    end

    # Check that `other_info` corresponds to this spec
    # @return [Boolean]
    def check_info(other_info)
      raise NotImplementedError
    end

    # Generate changelog between revisions
    # @param type [:upgrade, :downgrade]
    # @param other_info [any] version reference
    # @param opts [Hash] options
    # @option opts [Boolean] :color
    # @option opts [Boolean] :verbose
    # @option opts [Boolean] :patch
    # @return [String, nil]
    def string_changelog_info(type, other_info, opts = {})
      raise NotImplementedError
    end

    # Generate diff between revisions
    # @param type [:upgrade, :downgrade]
    # @param other_info [any] version reference
    # @param opts [Hash] options
    # @option opts [Boolean] :color
    # @return [String, nil]
    def string_diff_info(type, other_info, opts = {})
      raise NotImplementedError
    end

    def as_json
      ret = {}
      ret['type'] = type.to_s
      ret['nix_options'] = nix_opts
      ret['state'] = state
      ret['info'] = info
      ret['fetcher'] = fetcher && {
        'type' => fetcher,
        'options' => fetcher_opts,
      }
      ret
    end

    def to_json(*args)
      as_json.to_json(*args)
    end

    protected
    # @return [Hash]
    attr_reader :nix_opts

    # @return [Hash]
    attr_reader :json_opts

    # @return [Hash]
    attr_reader :state

    # @return [Hash]
    attr_reader :info

    # @return [String]
    attr_reader :fetcher

    # @return [Hash]
    attr_reader :fetcher_opts

    def parse
      errors.clear

      if json_opts.nil?
        errors << 'needs prefetch'
      elsif type.to_s != json_opts['type']
        errors << 'invalid type, needs update'
      elsif !json_opts['nix_options'] || !check_opts
        errors << 'outdated options, needs update'
      elsif json_opts['fetcher'].nil?
        errors << 'no fetcher, needs update'
      end

      return if errors.any?

      @state = json_opts['state']
      @info = json_opts['info']

      if json_opts['fetcher']
        @fetcher = json_opts['fetcher']['type']
        @fetcher_opts = json_opts['fetcher']['options']
      end
    end

    def set_state(v)
      @state = v
    end

    def set_info(v)
      @info = v
    end

    def set_fetcher(name, opts)
      @fetcher = name
      @fetcher_opts = opts
    end
  end
end
