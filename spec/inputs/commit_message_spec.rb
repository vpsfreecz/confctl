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

  def expect_git_log(url:, from:, to:, output:)
    mirror = instance_double(ConfCtl::GitRepoMirror)

    expect(ConfCtl::GitRepoMirror).to receive(:new).with(url, quiet: true).and_return(mirror)
    expect(mirror).to receive(:synchronize).and_yield
    expect(mirror).to receive(:setup)
    expect(mirror).to receive(:log).with(from, to, opts: ['--oneline']).and_return(output)
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

  it 'keeps no-changelog input summaries compact' do
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
        url: 'https://github.com/NixOS/nixpkgs'
      )
    ]

    message = described_class.build(changes: changes, changelog: false)

    expect(message).to eq(<<~MSG.strip)
      inputs: update nixpkgsStable, nixpkgsUnstable to 22222222

      nixpkgsStable: 11111111 -> 22222222
      nixpkgsUnstable: 33333333 -> 22222222
    MSG
  end

  it 'groups changelog for inputs with the same source and same revision change' do
    old_rev = '1111111111111111111111111111111111111111'
    new_rev = '2222222222222222222222222222222222222222'
    url = 'https://github.com/vpsfreecz/vpsadminos'
    log = <<~LOG.strip
      git log for #{old_rev}..#{new_rev}
      > 222222222 shared change
    LOG

    changes = [
      build_change(
        name: 'vpsadminosStaging',
        old_rev: old_rev,
        new_rev: new_rev,
        url: url
      ),
      build_change(
        name: 'vpsadminosProduction',
        old_rev: old_rev,
        new_rev: new_rev,
        url: "#{url}/"
      )
    ]

    expect_git_log(url: url, from: old_rev, to: new_rev, output: log)

    message = described_class.build(changes: changes, changelog: true)

    expect(message).to eq(<<~MSG.strip)
      inputs: update vpsadminosProduction, vpsadminosStaging to 22222222

      vpsadminosStaging: 11111111 -> 22222222
      vpsadminosProduction: 11111111 -> 22222222
      git log for #{old_rev}..#{new_rev}
      > 222222222 shared change
    MSG
  end

  it 'keeps separate changelog groups for inputs with different old revisions' do
    old_staging_rev = '1111111111111111111111111111111111111111'
    old_os_staging_rev = '3333333333333333333333333333333333333333'
    new_rev = '2222222222222222222222222222222222222222'
    url = 'https://github.com/vpsfreecz/vpsadminos'
    staging_log = <<~LOG.strip
      git log for #{old_staging_rev}..#{new_rev}
      > 222222222 staging change
    LOG
    os_staging_log = <<~LOG.strip
      git log for #{old_os_staging_rev}..#{new_rev}
      > 222222222 os staging change
    LOG

    changes = [
      build_change(
        name: 'vpsadminosStaging',
        old_rev: old_staging_rev,
        new_rev: new_rev,
        url: url
      ),
      build_change(
        name: 'vpsadminosProduction',
        old_rev: old_os_staging_rev,
        new_rev: new_rev,
        url: url
      )
    ]

    expect_git_log(url: url, from: old_staging_rev, to: new_rev, output: staging_log)
    expect_git_log(url: url, from: old_os_staging_rev, to: new_rev, output: os_staging_log)

    message = described_class.build(changes: changes, changelog: true)

    expect(message).to eq(<<~MSG.strip)
      inputs: update vpsadminosProduction, vpsadminosStaging to 22222222

      vpsadminosStaging: 11111111 -> 22222222
      git log for #{old_staging_rev}..#{new_rev}
      > 222222222 staging change

      vpsadminosProduction: 33333333 -> 22222222
      git log for #{old_os_staging_rev}..#{new_rev}
      > 222222222 os staging change
    MSG
  end

  it 'keeps changelog evidence failures soft after a guarded update was accepted' do
    changes = [build_change(name: 'source', old_rev: 'a' * 40, new_rev: 'b' * 40, url: 'https://github.com/owner/repo')]
    mirror = instance_double(ConfCtl::GitRepoMirror)
    allow(ConfCtl::GitRepoMirror).to receive(:new).and_return(mirror)
    allow(mirror).to receive(:synchronize).and_raise(ConfCtl::GitRepoMirror::Error, 'Git mirror operation failed')
    expect(described_class.build(changes: changes, changelog: true)).to include('changelog unavailable: Git mirror operation failed')
  end

  context 'with explicit message input selection' do
    let(:old_rev) { 'a' * 40 }
    let(:new_rev) { 'b' * 40 }
    let(:os_url) { 'https://github.com/vpsfreecz/vpsadminos' }
    let(:pkgs_url) { 'https://github.com/NixOS/nixpkgs' }
    let(:changes) do
      [
        build_change(name: 'os', old_rev: old_rev, new_rev: new_rev, url: os_url),
        build_change(name: 'os/nixpkgs', old_rev: old_rev, new_rev: 'c' * 40, url: pkgs_url)
      ]
    end

    it 'limits the entire message to exact selected paths without mutating the full changes' do
      expect_git_log(url: os_url, from: old_rev, to: new_rev, output: 'OS log marker')
      expect(ConfCtl::GitRepoMirror).not_to receive(:new).with(pkgs_url, quiet: true)

      message = described_class.build(changes: changes, changelog: true, selected_inputs: ['os'])

      expect(message).to eq(<<~MSG.strip)
        inputs: update os to bbbbbbbb

        os: aaaaaaaa -> bbbbbbbb
        OS log marker
      MSG
      expect(changes.map(&:name)).to eq(['os', 'os/nixpkgs'])
    end

    it 'does not widen nested selection to a parent or authorize logs by repository name' do
      expect_git_log(url: pkgs_url, from: old_rev, to: 'c' * 40, output: 'selected nested log marker')
      expect(ConfCtl::GitRepoMirror).not_to receive(:new).with(os_url, quiet: true)

      message = described_class.build(changes: changes, changelog: true, selected_inputs: ['os/nixpkgs'])

      expect(message).to eq("inputs: update os/nixpkgs to cccccccc\n\nos/nixpkgs: aaaaaaaa -> cccccccc\nselected nested log marker")
    end

    it 'keeps existing all-group behavior for nil selection' do
      expect_git_log(url: os_url, from: old_rev, to: new_rev, output: 'OS log marker')
      expect_git_log(url: pkgs_url, from: old_rev, to: 'c' * 40, output: 'Nixpkgs log marker')

      message = described_class.build(changes: changes, changelog: true, selected_inputs: nil)

      expect(message).to include('OS log marker', 'Nixpkgs log marker')
    end

    it 'uses only the sorted requested-input title when no selected revision changed' do
      expect(ConfCtl::GitRepoMirror).not_to receive(:new)
      message = described_class.build(changes: [changes.last], changelog: true, selected_inputs: %w[zRoot os])
      expect(message).to eq('inputs: update os, zRoot')
    end

    [nil, []].each do |original_changes|
      it "retains the no-changes error for original #{original_changes.inspect} changes" do
        expect do
          described_class.build(changes: original_changes, changelog: true, selected_inputs: ['os'])
        end.to raise_error(ArgumentError, 'no changes')
      end
    end

    it 'lets no-changelog override explicit selection' do
      expect(ConfCtl::GitRepoMirror).not_to receive(:new)
      message = described_class.build(changes: changes, changelog: false, selected_inputs: ['os/nixpkgs'])
      expect(message).to eq("inputs: update os/nixpkgs to cccccccc\n\nos/nixpkgs: aaaaaaaa -> cccccccc")
    end

    it 'shows only a selected alias and preserves downgrade direction' do
      changes << build_change(name: 'alias', old_rev: old_rev, new_rev: new_rev, url: "#{os_url}/")
      expect_git_log(url: "#{os_url}/", from: new_rev, to: old_rev, output: 'reverse OS log marker')
      expect(ConfCtl::GitRepoMirror).not_to receive(:new).with(pkgs_url, quiet: true)

      message = described_class.build(changes: changes, changelog: true, downgrade: true, selected_inputs: ['alias'])

      expect(message).to eq("inputs: update alias to bbbbbbbb\n\nalias: aaaaaaaa -> bbbbbbbb\nreverse OS log marker")
    end

    it 'queries a shared selected group only once' do
      changes << build_change(name: 'alias', old_rev: old_rev, new_rev: new_rev, url: "#{os_url}/")
      expect_git_log(url: os_url, from: old_rev, to: new_rev, output: 'shared OS log marker')
      expect(ConfCtl::GitRepoMirror).not_to receive(:new).with(pkgs_url, quiet: true)

      message = described_class.build(changes: changes, changelog: true, selected_inputs: %w[os alias])

      expect(message).to eq("inputs: update alias, os to bbbbbbbb\n\nos: aaaaaaaa -> bbbbbbbb\nalias: aaaaaaaa -> bbbbbbbb\nshared OS log marker")
    end
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
