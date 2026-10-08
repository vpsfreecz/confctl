require 'cgi'

module ConfCtl
  class ModuleOptions
    class Option
      attr_reader :name, :description, :type, :default, :example, :declarations

      def initialize(nixos_opt)
        @name = nixos_opt['name']
        @description = nixos_opt['description'] || 'This option has no description.'
        @type = nixos_opt['type']
        @default = extract_expression(nixos_opt['default'])
        @example = extract_expression(nixos_opt['example'])
        @declarations = nixos_opt['declarations'].map do |v|
          match = v.match(%r{\A(?:#{Regexp.escape(ConfCtl.root)}|/nix/store/[^/]+)/(nix/modules/(?:confctl|cluster)(?:/.*)?)\z})
          match ? "<confctl/#{match[1]}>" : v
        end
      end

      def md_description
        tagless = description
                  .gsub(%r{<literal>([^<]+)</literal>}, '`\1`')
                  .gsub(%r{<option>([^<]+)</option>}, '`\1`')

        CGI.unescapeHTML(tagless)
      end

      def nix_default
        nixify(default)
      end

      def nix_example
        example && nixify(example)
      end

      protected

      def extract_expression(v)
        if v.is_a?(Hash)
          case v['_type']
          when 'literalExpression'
            NixLiteralExpression.new(v['text'])
          else
            raise "Unsupported expression type #{v['_type'].inspect}"
          end
        else
          v
        end
      end

      def nixify(v)
        ConfCtl::NixFormat.to_nix(v)
      end
    end

    # @return [Array<ModuleOptions::Option>]
    attr_reader :options

    # @param nix [Nix, nil]
    def initialize(nix: nil, documentation: false)
      @nix = nix || Nix.new
      @documentation = documentation
      @options = []
    end

    def read
      @options = (@documentation ? nix.documentation_options : nix.module_options).map do |opt|
        Option.new(opt)
      end
    end

    def confctl_settings
      options.select { |opt| opt.name.start_with?('confctl.') && !system_option?(opt) }
    end

    def machine_settings
      options.select { |opt| opt.name.start_with?('cluster.') }
    end

    def system_settings
      options.select { |opt| system_option?(opt) }
    end

    protected

    attr_reader :nix

    def system_option?(opt)
      %w[confctl.carrier. confctl.programs. confctl.inputsInfo confctl.configurationInfo].any? do |prefix|
        opt.name.start_with?(prefix)
      end
    end
  end
end
