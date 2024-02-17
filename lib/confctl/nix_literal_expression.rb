module ConfCtl
  class NixLiteralExpression
    def initialize(value)
      @value = value
    end

    def to_s
      @value
    end

    def to_nix(**)
      @value
    end
  end
end
