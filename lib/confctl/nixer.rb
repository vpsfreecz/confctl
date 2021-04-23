require 'bundix/nixer'

module ConfCtl
  class Nixer < Bundix::Nixer
    def serialize
      super
    rescue RuntimeError
      case obj
      when Numeric
        obj.to_s
      when nil
        'null'
      else
        raise
      end
    end
  end
end
