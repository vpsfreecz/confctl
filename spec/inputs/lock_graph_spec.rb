require 'spec_helper'
require 'confctl/inputs/lock_graph'

RSpec.describe ConfCtl::Inputs::LockGraph do
  def source(rev = 'a' * 40)
    { 'locked' => { 'type' => 'github', 'owner' => 'example', 'repo' => 'source', 'rev' => rev } }
  end

  def graph
    {
      'version' => 7,
      'root' => 'entry',
      'nodes' => {
        'entry' => { 'inputs' => { 'parent' => 'p', 'alias' => %w[parent middle leaf], 'chain' => ['alias'] } },
        'p' => source.merge('inputs' => { 'middle' => 'm' }),
        'm' => source.merge('inputs' => { 'leaf' => 'l' }),
        'l' => source
      }
    }
  end

  it 'uses lock.root and resolves full nested and chained follows to terminal update paths' do
    lock = described_class.new(graph)
    expect(lock.resolve_path('chain').node).to eq(graph['nodes']['l'])
    expect(lock.resolve_path('chain').terminal_path).to eq('parent/middle/leaf')
    expect(lock.update_inputs(%w[alias chain parent/middle/leaf])).to eq(['parent/middle/leaf'])
    expect(lock.entries.keys).to contain_exactly('parent', 'parent/middle', 'parent/middle/leaf', 'alias', 'chain')
  end

  it 'continues traversal below an alias and resolves nested follows from the root' do
    data = graph
    data['nodes']['entry']['inputs']['top'] = ['parent']
    data['nodes']['p']['inputs']['shared'] = ['alias']
    lock = described_class.new(data)
    expect(lock.resolve_path('top/middle/leaf').terminal_path).to eq('parent/middle/leaf')
    expect(lock.resolve_path('top/shared').terminal_path).to eq('parent/middle/leaf')
    expect(lock.entries).to have_key('top/shared')
  end

  it 'preserves all logical paths through shared nodes' do
    data = graph
    data['nodes']['entry']['inputs']['another'] = 'p'
    expect(described_class.new(data).entries).to have_key('another/middle/leaf')
  end

  it 'ignores unreachable nodes' do
    data = graph
    data['nodes']['unused'] = { 'inputs' => { 'bad' => 'missing' } }
    expect { described_class.new(data) }.not_to raise_error
  end

  it 'accepts raw non-flake sources' do
    data = graph
    data['nodes']['l']['flake'] = false
    expect(described_class.new(data).resolve_path('alias').node['flake']).to be(false)
  end

  {
    'unknown schema' => ->(data) { data['version'] = 8 },
    'noninteger schema' => ->(data) { data['version'] = 7.0 },
    'absent root' => ->(data) { data.delete('root') },
    'dangling node' => ->(data) { data['nodes'].delete('l') },
    'missing locked source' => ->(data) { data['nodes']['l'].delete('locked') },
    'malformed inputs' => ->(data) { data['nodes']['p']['inputs'] = [] },
    'malformed input name' => ->(data) { data['nodes']['p']['inputs'][4] = 'l' },
    'malformed edge' => ->(data) { data['nodes']['entry']['inputs']['alias'] = 1 },
    'malformed follows' => ->(data) { data['nodes']['entry']['inputs']['alias'] = ['parent', 4] },
    'dangling follows' => ->(data) { data['nodes']['entry']['inputs']['alias'] = %w[parent absent] },
    'direct follows cycle' => ->(data) { data['nodes']['entry']['inputs']['alias'] = ['alias'] },
    'chained follows cycle' => ->(data) { data['nodes']['entry']['inputs']['alias'] = ['chain'] },
    'dependency cycle' => ->(data) { data['nodes']['l']['inputs'] = { 'loop' => 'p' } },
    'root follows cycle' => ->(data) { data['nodes']['entry']['inputs']['alias'] = [] },
    'malformed flake flag' => ->(data) { data['nodes']['l']['flake'] = 'false' }
  }.each do |name, mutate|
    it "rejects #{name}" do
      data = graph
      mutate.call(data)
      expect { described_class.new(data) }.to raise_error(described_class::Error)
    end
  end

  it 'rejects missing or malformed requested paths instead of silently skipping them' do
    lock = described_class.new(graph)
    expect { lock.resolve_path('parent/absent') }.to raise_error(described_class::Error)
    expect { lock.resolve_path('parent//middle') }.to raise_error(described_class::Error)
  end
end
