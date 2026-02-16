require 'json'

module ConfCtl
  class PinsInfo
    KEYS = %w[input url rev shortRev lastModified].freeze

    def self.parse(json)
      normalize(JSON.parse(json))
    rescue JSON::ParserError => e
      raise Error, "unable to parse pins info: #{e.message}"
    end

    def self.normalize(hash)
      return nil unless hash.is_a?(Hash)

      hash.each_with_object({}) do |(role, info), acc|
        acc[role.to_s] = normalize_info(info)
      end
    end

    def self.normalize_info(info)
      case info
      when String
        rev = info
        data = { 'rev' => rev, 'shortRev' => rev && rev[0, 8] }
        data.compact
      when Hash
        normalized = {}

        info.each do |k, v|
          key = k.to_s
          case key
          when 'input', 'url', 'rev', 'shortRev', 'lastModified'
            normalized[key] = v
          when 'short_rev'
            normalized['shortRev'] = v
          end
        end

        if normalized['shortRev'].nil? && normalized['rev']
          normalized['shortRev'] = normalized['rev'][0, 8]
        end

        normalized
      else
        {}
      end
    end
  end
end
