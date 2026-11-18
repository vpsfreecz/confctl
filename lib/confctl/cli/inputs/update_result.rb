module ConfCtl::Cli
  module Inputs::UpdateResult
    protected

    def print_guarded_update_summary(result, context: nil)
      puts "Input update for #{context}:" if context
      result.fetch(:skipped, []).each do |attempt|
        puts "Skipped update for #{attempt[:requested_inputs].join(', ')} (#{attempt[:input]}): upstream is behind."
        attempt[:rejections].each do |rejection|
          puts "  #{rejection.path}: retaining #{rejection.old_rev}; candidate #{rejection.new_rev}"
        end
      end
      print_update_summary(result[:changes])
    end

    def print_update_summary(changes)
      if changes.empty?
        puts 'No changes.'
        return
      end

      puts "Updated inputs (#{changes.length}):"
      changes.each do |ch|
        puts "  #{ch.name}: #{ch.old_short_rev || '-'} -> #{ch.new_short_rev || '-'}"
      end
    end

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
