# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'confctl/cli'

RSpec.describe ConfCtl::Cli::Cluster do
  let(:command) { described_class.new({}, {}, []) }
  let(:paths) { { 'one' => { 'nixpkgs' => '/one' }, 'two' => { 'nixpkgs' => '/two' }, 'three' => { 'nixpkgs' => '/one' } } }
  let(:nix) { instance_double(ConfCtl::Nix, confctl_settings: settings) }

  context 'with ordinary flake builds' do
    let(:settings) { { 'nix' => { 'legacyNixPath' => false } } }

    it 'batches hosts with differing input roles into one build' do
      expect(command.send(:input_build_groups, paths, nix)).to eq([[paths.keys, {}]])
    end
  end

  context 'with the optional legacy NIX_PATH bridge' do
    let(:settings) { { 'nix' => { 'legacyNixPath' => true } } }

    it 'separates incompatible input maps without splitting equal maps' do
      expect(command.send(:input_build_groups, paths, nix)).to eq([
                                                                    [%w[one three], { 'nixpkgs' => '/one' }],
                                                                    [['two'], { 'nixpkgs' => '/two' }]
                                                                  ])
    end
  end
end
