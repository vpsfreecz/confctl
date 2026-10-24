module ConfCtl
  module Inputs
    # A semantic view of a version-7 lock, independent of generated node IDs.
    class LockGraph
      class Error < StandardError; end

      Entry = Struct.new(:path, :node, :terminal_path)

      attr_reader :entries

      def initialize(lock)
        @data = lock.respond_to?(:data) ? lock.data : lock
        unless @data.is_a?(Hash) && @data['version'].eql?(7) && @data['nodes'].is_a?(Hash)
          raise Error, 'Expected a version-7 flake lock graph'
        end

        @nodes = @data['nodes']
        @root = @data['root']
        node!(@root)
        @entries = {}
        walk([], @root, [@root])
      end

      def resolve_path(path)
        components = path.is_a?(String) ? path.split('/', -1) : path
        validate_path!(components)
        id, terminal = resolve(components, [])
        Entry.new(path: components.join('/'), node: node!(id), terminal_path: terminal.join('/'))
      end

      def update_inputs(names)
        names.map { |name| resolve_path(name).terminal_path }.uniq
      end

      protected

      def node!(id)
        node = @nodes[id]
        unless id.is_a?(String) && node.is_a?(Hash) &&
               (!node.has_key?('inputs') || node['inputs'].is_a?(Hash)) &&
               (id == @root || node['locked'].is_a?(Hash)) &&
               (!node.has_key?('original') || node['original'].is_a?(Hash)) &&
               (!node.has_key?('flake') || [true, false].include?(node['flake']))
          raise Error, 'Malformed or dangling lock graph node'
        end

        validate_path!((node['inputs'] || {}).keys)
        node
      end

      def validate_path!(path)
        return if path.is_a?(Array) && path.all? { |name| name.is_a?(String) && !name.empty? && !name.include?('/') }

        raise Error, 'Malformed lock input path'
      end

      def resolve(components, stack)
        id = @root
        terminal = []
        components.each do |name|
          key = [id, name]
          raise Error, 'Cyclic follows reference in lock graph' if stack.include?(key)

          edge = (node!(id)['inputs'] || {})[name]
          case edge
          when String
            id = edge
            node!(id)
            terminal << name
          when Array
            validate_path!(edge)
            id, terminal = resolve(edge, stack + [key])
          else
            raise Error, 'Malformed or dangling lock input reference'
          end
        end
        [id, terminal]
      end

      def walk(prefix, id, ancestors)
        (node!(id)['inputs'] || {}).keys.sort.each do |name|
          validate_path!([name])
          path = prefix + [name]
          entry = resolve_path(path)
          target, = resolve(path, [])
          raise Error, 'Cyclic dependency in lock graph' if ancestors.include?(target)

          @entries[entry.path] = entry
          walk(path, target, ancestors + [target])
        end
      end
    end
  end
end
