module ConfCtl
  class FlakeLockDiff
    Change = Struct.new(
      :name,
      :old_info,
      :new_info
    ) do
      def old_rev = old_info && old_info[:rev]
      def new_rev = new_info && new_info[:rev]
      def old_short_rev = old_info && old_info[:short_rev]
      def new_short_rev = new_info && new_info[:short_rev]
      def url = (new_info && new_info[:url]) || (old_info && old_info[:url])

      def changed?
        old_rev != new_rev
      end
    end

    def self.diff(old_lock, new_lock, inputs: nil)
      return [] if new_lock.nil?

      names =
        if inputs.nil?
          new_lock.root_inputs
        else
          inputs
        end

      names.map do |name|
        old_info = old_lock&.input_info(name)
        new_info = new_lock.input_info(name)
        Change.new(name: name, old_info: old_info, new_info: new_info)
      end.select(&:changed?)
    end
  end
end
