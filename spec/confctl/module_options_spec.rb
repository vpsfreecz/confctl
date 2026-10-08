# frozen_string_literal: true

require 'spec_helper'
require 'confctl'

RSpec.describe ConfCtl::ModuleOptions::Option do
  def option_with_declarations(*declarations)
    described_class.new('name' => 'confctl.nix.legacyNixPath', 'declarations' => declarations)
  end

  it 'uses identical source-relative labels for owned declarations from distinct store hashes' do
    declarations = %w[hash-one-source hash-two-source].map do |source|
      "/nix/store/#{source}/nix/modules/confctl/nix.nix"
    end
    expect(option_with_declarations(*declarations).declarations)
      .to eq(['<confctl/nix/modules/confctl/nix.nix>'] * 2)
    expect(option_with_declarations('/nix/store/hash-source/nix/modules/cluster/default.nix').declarations)
      .to eq(['<confctl/nix/modules/cluster/default.nix>'])
  end

  it 'normalizes owning module directories and descendant directory modules' do
    expected = [
      '<confctl/nix/modules/cluster>',
      '<confctl/nix/modules/confctl>',
      '<confctl/nix/modules/confctl/kexec-netboot>'
    ]
    expect(option_with_declarations(
      '/nix/store/hash-source/nix/modules/cluster',
      '/nix/store/hash-source/nix/modules/confctl',
      '/nix/store/hash-source/nix/modules/confctl/kexec-netboot'
    ).declarations).to eq(expected)
  end

  it 'normalizes declarations from the local owning source tree' do
    expect(option_with_declarations(File.join(ConfCtl.root, 'nix/modules/confctl/nix.nix')).declarations)
      .to eq(['<confctl/nix/modules/confctl/nix.nix>'])
  end

  it 'preserves custom cluster modules and foreign declarations' do
    declarations = [
      '/nix/store/custom-source/modules/cluster/default.nix',
      '/nix/store/nixpkgs-source/nixos/modules/services/example.nix',
      '/tmp/foreign-source/nix/modules/cluster/default.nix',
      '/nix/store/custom-source/nix/custom/module.nix',
      '/nix/store/custom-source/nix/modules/cluster-extra/default.nix'
    ]
    expect(option_with_declarations(*declarations).declarations).to eq(declarations)
  end
end
