require 'json'

module ConfCtl
  class Swpins::DeployedInfo
    # @param json [String]
    def self.parse!(json)
      new(JSON.parse(json))
    rescue JSON::ParserError => e
      raise Error, "unable to parse swpins info: #{e.message}"
    end

    # @return [Hash]
    attr_reader :swpins

    def initialize(hash)
      @swpins = hash
    end

    def [](swpin)
      swpins[swpin]
    end
  end
end
