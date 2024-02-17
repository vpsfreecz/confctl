require 'pathname'

module ConfCtl
  # Format Ruby values in Nix
  class NixFormat
    # @param value [any]
    # @param sort [Boolean] sort hash keys
    # @return [String]
    def self.to_nix(value, sort: true)
      new(sort:).to_nix(value)
    end

    # @param sort [Boolean] sort hash keys
    def initialize(sort: true)
      @sort = sort
      @output = ''
      @level = 0
    end

    # @param value [any]
    # @return [String]
    def to_nix(value)
      format_value(value).strip
    end

    def pad(str, indent: true)
      if indent
        "#{' ' * @level}#{str}"
      else
        str
      end
    end

    def indent
      @level += 2
      yield
      @level -= 2
    end

    protected

    def format_value(value, indent: true, semicolon: false, nl: false)
      if value.respond_to?(:to_nix)
        return value.to_nix(nix_format: self, indent:, semicolon:, nl:)
      end

      case value
      when Hash
        format_hash(value, indent:, semicolon:, nl:)

      when Array
        format_array(value, indent:, semicolon:, nl:)

      else
        simple =
          case value
          when String
            value.dump

          when Symbol
            value.to_s.dump

          when Numeric, true, false
            value.to_s

          when Pathname
            str = value.to_s

            if str.start_with?('/')
              str
            else
              "./#{str}"
            end

          when nil
            'null'

          else
            raise "Unable to format #{value.inspect} (#{value.class}) to nix: " \
                  'implement #to_nix()?'
          end

        pad("#{simple}#{semicolon ? ';' : ''}#{nl ? "\n" : ''}", indent:)
      end
    end

    def format_hash(hash, indent: true, semicolon: false, nl: true)
      ret = pad("{\n", indent:)

      quote_keys = !hash.each_key.all? { |k| /^[a-zA-Z_][a-zA-Z_\-']*$/ =~ k }

      sorted =
        if @sort
          hash.sort do |a, b|
            a[0] <=> b[0]
          end
        else
          hash
        end

      indent do
        sorted.each do |k, v|
          ret << (quote_keys ? pad("\"#{k}\"") : pad(k.to_s))
          ret << " = #{format_value(v, indent: false, semicolon: true, nl: false)}\n"
        end
      end

      ret << pad("}#{semicolon ? ';' : ''}#{nl ? "\n" : ''}")
      ret
    end

    def format_array(array, indent: true, semicolon: false, nl: true)
      ret = pad("[\n", indent:)

      indent do
        array.each do |v|
          ret << "#{pad(format_value(v, indent: false, nl: false))}\n"
        end
      end

      ret << pad("]#{semicolon ? ';' : ''}#{nl ? "\n" : ''}")
    end
  end
end
