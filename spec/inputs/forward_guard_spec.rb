require 'spec_helper'
require 'confctl/flake_lock'
require 'confctl/inputs/forward_guard'

RSpec.describe ConfCtl::Inputs::ForwardGuard do
  let(:provider) { instance_double(ConfCtl::Inputs::GitHubCompare) }
  let(:guard) { described_class.new(provider: provider) }

  def source(rev = 'a' * 40, repo = 'source')
    { 'locked' => { 'type' => 'github', 'owner' => 'example', 'repo' => repo, 'rev' => rev } }
  end

  def graph
    {
      'version' => 7,
      'root' => 'entry',
      'nodes' => {
        'entry' => { 'inputs' => { 'parent' => 'p', 'alias' => %w[parent leaf] } },
        'p' => source.merge('inputs' => { 'leaf' => 'l' }),
        'l' => source('b' * 40, 'dependency')
      }
    }
  end

  def compare(repo: 'source', old_rev: 'a' * 40, new_rev: 'c' * 40, result: :ahead)
    expect(provider).to receive(:compare).with(owner: 'example', repo: repo, old_rev: old_rev, new_rev: new_rev)
                                         .once.and_return(result)
  end

  it 'accepts identical revisions locally, including an ordinary FlakeLock object' do
    expect(provider).not_to receive(:compare)
    result = guard.validate(ConfCtl::FlakeLock.new('old', graph), graph)
    expect(result.status).to eq(:unchanged)
    expect(result).to be_success
    expect(result.changes).to be_empty
  end

  it 'ignores arbitrary generated node renumbering and input ordering' do
    candidate = graph
    candidate['nodes']['renumbered'] = candidate['nodes'].delete('l')
    candidate['nodes']['p']['inputs']['leaf'] = 'renumbered'
    expect(guard.validate(graph, candidate).status).to eq(:unchanged)
  end

  it 'accepts forward root revisions' do
    candidate = graph
    candidate['nodes']['p']['locked']['rev'] = 'c' * 40
    compare
    result = guard.validate(graph, candidate)
    expect(result.status).to eq(:updated)
    expect(result.changes.map(&:path)).to eq(['parent'])
    expect(result.changes.first.old_rev).to eq('a' * 40)
  end

  it 'checks all nested changes when the root revision is equal and deduplicates alias evidence' do
    candidate = graph
    candidate['nodes']['l']['locked']['rev'] = 'c' * 40
    compare(repo: 'dependency', old_rev: 'b' * 40)
    result = guard.validate(graph, candidate)
    expect(result.status).to eq(:updated)
    expect(result.changes.map(&:path)).to eq(['alias', 'parent/leaf'])
  end

  it 'rejects a nested rollback even when its parent advances' do
    candidate = graph
    candidate['nodes']['p']['locked']['rev'] = 'c' * 40
    candidate['nodes']['l']['locked']['rev'] = 'd' * 40
    compare
    compare(repo: 'dependency', old_rev: 'b' * 40, new_rev: 'd' * 40, result: :behind)
    result = guard.validate(graph, candidate)
    expect(result).to be_rejected
    expect(result).not_to be_success
    expect(result.issues.join).to include('parent/leaf', 'behind', 'b' * 40, 'd' * 40)
  end

  %i[behind diverged].each do |outcome|
    it "rejects #{outcome} history despite newer candidate metadata" do
      candidate = graph
      candidate['nodes']['p']['locked'].merge!('rev' => 'c' * 40, 'lastModified' => 2_000_000_000)
      compare(result: outcome)
      expect(guard.validate(graph, candidate)).to be_rejected
    end
  end

  it 'accepts merge and later-revert commits when they contain the original commit' do
    candidate = graph
    candidate['nodes']['p']['locked'].merge!('rev' => 'c' * 40, 'lastModified' => 1, 'narHash' => 'sha256-new')
    compare
    expect(guard.validate(graph, candidate)).to be_success
  end

  %w[repo owner host type dir].each do |attribute|
    it "does not accept a changed #{attribute} as ordinary forward movement" do
      candidate = graph
      candidate['nodes']['p']['locked'].merge!(attribute => 'different', 'rev' => 'c' * 40)
      expect(provider).not_to receive(:compare)
      expect(guard.validate(graph, candidate)).not_to be_success
    end
  end

  it 'normalizes github.com source names for ancestry queries' do
    candidate = graph
    candidate['nodes']['p']['locked'].merge!('rev' => 'c' * 40, 'owner' => 'EXAMPLE', 'repo' => 'SOURCE')
    compare
    expect(guard.validate(graph, candidate)).to be_success
  end

  %w[narHash lastModified dir].each do |attribute|
    it "rejects changed #{attribute} at an equal revision" do
      candidate = graph
      candidate['nodes']['p']['locked'][attribute] = attribute == 'lastModified' ? 100 : 'changed'
      expect(provider).not_to receive(:compare)
      expect(guard.validate(graph, candidate)).to be_rejected
    end
  end

  it 'rejects switching between a flake and raw source' do
    candidate = graph
    candidate['nodes']['p']['flake'] = false
    expect(guard.validate(graph, candidate)).to be_rejected
  end

  it 'permits revision updates to raw GitHub sources' do
    old = graph
    old['nodes']['l']['flake'] = false
    candidate = Marshal.load(Marshal.dump(old))
    candidate['nodes']['l']['locked']['rev'] = 'c' * 40
    compare(repo: 'dependency', old_rev: 'b' * 40)
    expect(guard.validate(old, candidate)).to be_success
  end

  it 'rejects removed logical paths even when they were aliases' do
    candidate = graph
    candidate['nodes']['entry']['inputs'].delete('alias')
    result = guard.validate(graph, candidate)
    expect(result).to be_rejected
    expect(result.issues.join).to include('alias', 'removed')
  end

  it 'accepts new supported immutable dependencies and reports them' do
    candidate = graph
    candidate['nodes']['entry']['inputs']['new'] = 'n'
    candidate['nodes']['n'] = source
    result = guard.validate(graph, candidate)
    expect(result.status).to eq(:updated)
    expect(result.changes.map(&:path)).to eq(['new'])
    expect(result.changes.first.old_node).to be_nil
  end

  it 'retains unchanged unsupported path sources without giving changed sources a bypass' do
    old = graph
    old['nodes']['l']['locked'] = { 'type' => 'path', 'path' => '/source' }
    expect(guard.validate(old, old).status).to eq(:unchanged)
    candidate = Marshal.load(Marshal.dump(old))
    candidate['nodes']['l']['locked']['path'] = '/changed'
    expect(guard.validate(old, candidate)).to be_error
  end

  it 'fails closed for unsupported new dependencies' do
    candidate = graph
    candidate['nodes']['entry']['inputs']['new'] = 'n'
    candidate['nodes']['n'] = { 'locked' => { 'type' => 'tarball', 'url' => 'https://example.com/archive' } }
    expect(guard.validate(graph, candidate)).to be_error
  end

  [nil, 'short', 'main'].each do |rev|
    it "fails closed for nonimmutable revision #{rev.inspect}" do
      candidate = graph
      candidate['nodes']['p']['locked']['rev'] = rev
      expect(guard.validate(graph, candidate)).to be_error
    end
  end

  it 'fails closed for unknown extraction attributes without exposing their values' do
    candidate = graph
    candidate['nodes']['p']['locked']['privateToken'] = 'secret-value'
    result = guard.validate(graph, candidate)
    expect(result).to be_error
    expect(result.issues.join).not_to include('secret-value')
  end

  it 'reports unavailable provider evidence as an error even alongside a known rejection' do
    candidate = graph
    candidate['nodes']['p']['locked']['rev'] = 'c' * 40
    candidate['nodes']['l']['locked']['rev'] = 'd' * 40
    allow(provider).to receive(:compare).and_raise(ConfCtl::Inputs::GitHubCompare::Error, 'Unavailable evidence')
    compare(result: :behind)
    expect(guard.validate(graph, candidate)).to be_error
  end

  it 'fails closed for unexpected provider outcomes' do
    candidate = graph
    candidate['nodes']['p']['locked']['rev'] = 'c' * 40
    compare(result: nil)
    expect(guard.validate(graph, candidate)).to be_error
  end

  it 'deduplicates failed evidence requests shared by aliases' do
    candidate = graph
    candidate['nodes']['l']['locked']['rev'] = 'c' * 40
    expect(provider).to receive(:compare).once.and_raise(ConfCtl::Inputs::GitHubCompare::Error, 'Unavailable evidence')
    result = guard.validate(graph, candidate)
    expect(result).to be_error
    expect(result.issues.size).to eq(2)
  end

  it 'does not reuse comparison evidence for another revision pair or validation run' do
    candidate = graph
    candidate['nodes']['p']['locked']['rev'] = 'c' * 40
    expect(provider).to receive(:compare).twice.and_return(:ahead)
    2.times { expect(guard.validate(graph, candidate)).to be_success }
  end

  it 'returns typed graph errors rather than accepting unsupported lock versions' do
    candidate = graph
    candidate['version'] = 100
    expect(guard.validate(graph, candidate)).to be_error
  end
end
