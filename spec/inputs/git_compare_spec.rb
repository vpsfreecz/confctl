require 'spec_helper'
require 'confctl/inputs/git_compare'

RSpec.describe ConfCtl::Inputs::GitCompare do
  let(:old_rev) { 'a' * 40 }
  let(:new_rev) { 'b' * 40 }

  it 'uses the exact locked spelling and existing changelog URL without a suffix' do
    mirror = instance_double(ConfCtl::GitRepoMirror)
    expect(ConfCtl::GitRepoMirror).to receive(:new).with('https://github.com/NixOS/nixpkgs', quiet: true).and_return(mirror)
    expect(mirror).to receive(:compare).with(old_rev, new_rev).and_return(:ahead)
    expect(described_class.new.compare(owner: 'NixOS', repo: 'nixpkgs', old_rev: old_rev, new_rev: new_rev)).to eq(:ahead)
  end

  it 'accepts equality without constructing or accessing a mirror' do
    expect(ConfCtl::GitRepoMirror).not_to receive(:new)
    expect(described_class.new.compare(owner: 'owner', repo: 'repo', old_rev: old_rev, new_rev: old_rev)).to eq(:identical)
  end

  [nil, 'main', 'abc', '-bad', 'a' * 39].each do |rev|
    it "rejects nonimmutable revision #{rev.inspect}" do
      expect(ConfCtl::GitRepoMirror).not_to receive(:new)
      expect do
        described_class.new.compare(owner: 'owner', repo: 'repo', old_rev: old_rev, new_rev: rev)
      end.to raise_error(described_class::Error)
    end
  end

  ['.', '..', 'name/other', 'name?key=value', nil].each do |name|
    it "rejects invalid repository component #{name.inspect}" do
      expect do
        described_class.new.compare(owner: name, repo: 'repo', old_rev: old_rev, new_rev: new_rev)
      end.to raise_error(described_class::Error)
    end
  end

  it 'converts cache evidence failures into the provider error contract' do
    mirror = instance_double(ConfCtl::GitRepoMirror)
    allow(ConfCtl::GitRepoMirror).to receive(:new).and_return(mirror)
    allow(mirror).to receive(:compare).and_raise(ConfCtl::GitRepoMirror::Error, 'Git evidence unavailable')
    expect do
      described_class.new.compare(owner: 'owner', repo: 'repo', old_rev: old_rev, new_rev: new_rev)
    end.to raise_error(described_class::Error, 'Git evidence unavailable')
  end
end
