module ConfCtl
  class Swpins::ChangeSet
    class SpecOwner
      attr_reader :name, :path
    end

    SpecSet = Struct.new(:spec, :original_version, :original_info, keyword_init: true) do
      def name
        spec.name
      end

      def new_version
        spec.version
      end

      def new_info
        spec.info
      end

      def changed?
        original_version != new_version
      end

      def string_changelog(type)
        if original_info
          spec.string_changelog_info(type, original_info, color: false)
        else
          "Initial version\n"
        end
      end
    end

    def initialize
      @owners = {}
    end

    # @param owner [SpecOwner]
    # @param spec [Swpins::Spec]
    def add(owner, spec)
      @owners[owner] ||= []
      @owners[owner] << SpecSet.new(
        spec:,
        original_version: spec.valid? ? spec.version : nil,
        original_info: spec.valid? ? spec.info : nil
      )
      nil
    end

    # @param type [:upgrade, :downgrade]
    # @param changelog [Boolean]
    # @param editor [Boolean]
    def commit(type: :upgrade, changelog: false, editor: true)
      return if @owners.empty?

      git_commit = [
        'git',
        'commit',
        use_editor?(editor) ? '-e' : nil,
        '-m', build_message(type, changelog),
        *changed_files
      ].compact

      ret = Kernel.system(*git_commit)

      return if ret

      raise 'git commit exited with non-zero status code'
    end

    def any_changes?
      @owners.any? { |_, spec_sets| spec_sets.any?(&:changed?) }
    end

    protected

    def build_message(type, changelog)
      [
        build_title(type, changelog),
        build_description(type, changelog)
      ].join("\n\n")
    end

    def build_title(type, changelog)
      msg = 'swpins: '

      if same_changes?
        spec_sets = @owners.first[1]
        msg << "#{@owners.each_key.map(&:name).sort.join(', ')}: update "

        msg << spec_sets.map do |spec_set|
          "#{spec_set.name} to #{spec_set.new_version}"
        end.join(', ')
      else
        all_spec_names = []

        @owners.each_value do |spec_sets|
          spec_sets.each do |spec_set|
            all_spec_names << spec_set.name unless all_spec_names.include?(spec_set.name)
          end
        end

        msg << "#{@owners.each_key.map(&:name).join(', ')}: update #{all_spec_names.sort.join(', ')}"
      end

      msg
    end

    def build_description(type, changelog)
      msg = ''

      @owners.each do |owner, spec_sets|
        spec_sets.each do |spec_set|
          if spec_set.changed?
            msg << "#{owner.name} #{spec_set.name}: "

            msg << if spec_set.original_version
                     "#{spec_set.original_version} -> #{spec_set.new_version}\n"
                   else
                     "set to #{spec_set.new_version}\n"
                   end

            if changelog
              msg << spec_set.string_changelog(type)
              msg << "\n"
            end
          else
            msg << "  #{spec_set.name}: unchanged\n"
          end
        end

        msg << "\n" if changelog
      end

      msg
    end

    def changed_files
      @owners.each_key.map(&:path)
    end

    def same_changes?
      return true if @owners.length == 1

      expected_spec_sets = @owners.first[1]

      @owners.each_value do |spec_sets|
        return false if expected_spec_sets.length != spec_sets.length

        spec_sets.each do |spec_set|
          expected = expected_spec_sets.detect { |v| v.name == spec_set.name }
          return false if expected.nil? || expected.new_version != spec_set.new_version
        end
      end

      true
    end

    def use_editor?(editor)
      editor && $stdout.tty?
    end
  end
end
