# frozen_string_literal: true

require 'confctl/flake_lock_diff'
require 'confctl/inputs/commit_message'

RSpec.describe ConfCtl::Inputs::CommitMessage do
  def build_change(name:, old_rev:, new_rev:, url:)
    ConfCtl::FlakeLockDiff::Change.new(
      name: name,
      old_info: old_rev ? { rev: old_rev, short_rev: old_rev[0, 8], url: url } : nil,
      new_info: { rev: new_rev, short_rev: new_rev[0, 8], url: url }
    )
  end

  it 'includes the new revision in title for a single input' do
    changes = [
      build_change(
        name: 'nixpkgs',
        old_rev: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
        new_rev: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
        url: 'https://github.com/NixOS/nixpkgs'
      )
    ]

    title = described_class.build(changes: changes, changelog: false).lines.first.chomp

    expect(title).to eq('inputs: update nixpkgs to bbbbbbbb')
  end

  it 'includes the new revision in title for multiple inputs from the same source' do
    changes = [
      build_change(
        name: 'nixpkgsStable',
        old_rev: '1111111111111111111111111111111111111111',
        new_rev: '2222222222222222222222222222222222222222',
        url: 'https://github.com/NixOS/nixpkgs'
      ),
      build_change(
        name: 'nixpkgsUnstable',
        old_rev: '3333333333333333333333333333333333333333',
        new_rev: '2222222222222222222222222222222222222222',
        url: 'https://github.com/NixOS/nixpkgs/'
      )
    ]

    title = described_class.build(changes: changes, changelog: false).lines.first.chomp

    expect(title).to eq('inputs: update nixpkgsStable, nixpkgsUnstable to 22222222')
  end

  it 'does not include the revision in title for changes from different sources' do
    changes = [
      build_change(
        name: 'nixpkgs',
        old_rev: '4444444444444444444444444444444444444444',
        new_rev: '5555555555555555555555555555555555555555',
        url: 'https://github.com/NixOS/nixpkgs'
      ),
      build_change(
        name: 'vpsadminos',
        old_rev: '6666666666666666666666666666666666666666',
        new_rev: '5555555555555555555555555555555555555555',
        url: 'https://github.com/vpsfreecz/vpsadminos'
      )
    ]

    title = described_class.build(changes: changes, changelog: false).lines.first.chomp

    expect(title).to eq('inputs: update nixpkgs, vpsadminos')
  end

  it 'includes revision in title for set action' do
    changes = [
      build_change(
        name: 'vpsadminos',
        old_rev: nil,
        new_rev: '7777777777777777777777777777777777777777',
        url: 'https://github.com/vpsfreecz/vpsadminos'
      )
    ]

    title = described_class.build(changes: changes, changelog: false, action: :set).lines.first.chomp

    expect(title).to eq('inputs: set vpsadminos to 77777777')
  end
end
