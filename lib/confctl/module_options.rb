require 'cgi'

module ConfCtl
  class ModuleOptions
    class Option
      attr_reader :name, :description, :type, :default, :example, :declarations

      def initialize(nixos_opt)
        @name = nixos_opt['name']
        @description = nixos_opt['description'] || 'This option has no description.'
        @type = nixos_opt['type']
        @default = nixos_opt['default']
        @example = nixos_opt['example']
        @declarations = nixos_opt['declarations'].map do |v|
          raise "unable to place module '#{v}'" unless %r{/confctl/([^$]+)} =~ v

          "<confctl/#{::Regexp.last_match(1)}>"
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

      def nixify(v)
        ConfCtl::Nixer.new(v).serialize
      end
    end

    # @return [Array<ModuleOptions::Option>]
    attr_reader :options

    # @param nix [Nix, nil]
    def initialize(nix: nil)
      @nix = nix || Nix.new
      @options = []
    end

    def read
      @options = nix.module_options.map do |opt|
        Option.new(opt)
      end
    end

    def confctl_settings
      options.select do |opt|
        opt.name.start_with?('confctl.') \
          && !opt.name.start_with?('confctl.swpins.')
      end
    end

    def swpin_settings
      options.select { |opt| opt.name.start_with?('confctl.swpins.') }
    end

    def machine_settings
      options.select { |opt| opt.name.start_with?('cluster.') }
    end

    def service_settings
      options.select { |opt| opt.name.start_with?('services.') }
    end

    protected

    attr_reader :nix
  end
end
