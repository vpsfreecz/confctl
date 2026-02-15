require 'json'

module ConfCtl
  class FlakeLock
    def self.load(path)
      new(path, JSON.parse(File.read(path)))
    end

    def self.load_optional(path)
      return nil unless File.exist?(path)

      load(path)
    end

    attr_reader :path, :data

    def initialize(path, data)
      @path = path
      @data = data
    end

    def root_inputs
      inputs_map = data.dig('nodes', 'root', 'inputs') || {}
      inputs_map.keys.sort
    end

    def node_for_input(input_name)
      node_id = data.dig('nodes', 'root', 'inputs', input_name)

      if node_id.is_a?(String)
        return data.dig('nodes', node_id)
      end

      if node_id.is_a?(Array) && node_id[0].is_a?(String)
        return data.dig('nodes', node_id[0])
      end

      nil
    end

    def input_info(input_name)
      node = node_for_input(input_name) || {}
      locked = node['locked'] || {}
      original = node['original'] || {}

      type = locked['type'] || original['type'] || '-'
      rev = locked['rev']
      short_rev = rev ? rev[0, 8] : nil
      ref = original['ref'] || locked['ref']
      url = derive_url(original, locked)

      {
        type: type,
        rev: rev,
        short_rev: short_rev,
        ref: ref,
        url: url,
        locked: locked,
        original: original
      }
    end

    def derive_url(original, locked)
      url = (locked && locked['url']) || (original && original['url'])
      url = url.sub(/^git\+/, '') if url.is_a?(String)

      if (locked && locked['type'] == 'github') || (original && original['type'] == 'github')
        owner = (locked && locked['owner']) || (original && original['owner'])
        repo = (locked && locked['repo']) || (original && original['repo'])
        if owner && repo
          return "https://github.com/#{owner}/#{repo}"
        end
      end

      url
    end
  end
end
