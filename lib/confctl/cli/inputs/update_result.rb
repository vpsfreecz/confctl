module ConfCtl::Cli
  module Inputs::UpdateResult
    protected

    def ensure_update_success!(result)
      return unless %i[rejected error].include?(result[:status])

      message = result[:issues].join("\n")
      if result[:status] == :rejected
        raise GLI::CustomExit.new("Forward-only update rejected:\n#{message}", 3)
      end

      raise ConfCtl::Error, "Forward-only update could not be verified:\n#{message}"
    end
  end
end
