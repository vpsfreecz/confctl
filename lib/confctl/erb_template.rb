require 'erb'

module ConfCtl
  class ErbTemplate
    def self.render(name, vars)
      t = new(name, vars)
      t.render
    end

    def self.render_to(name, vars, path)
      File.write("#{path}.new", render(name, vars))
      File.rename("#{path}.new", path)
    end

    def initialize(name, vars)
      @_tpl = ERB.new(
        File.read(
          File.join(ConfCtl.root, 'template', "#{name}.erb")
        ),
        0,
        '-',
      )

      vars.each do |k, v|
        if v.is_a?(Proc)
          define_singleton_method(k, &v)
        elsif v.is_a?(Method)
          define_singleton_method(k) { |*args| v.call(*args) }
        else
          define_singleton_method(k) { v }
        end
      end
    end

    def render
      @_tpl.result(binding)
    end
  end
end
