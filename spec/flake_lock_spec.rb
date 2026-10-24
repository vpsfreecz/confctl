# frozen_string_literal: true

require 'spec_helper'
require 'confctl/flake_lock'

RSpec.describe ConfCtl::FlakeLock do
  it 'reads root inputs and nested chained follows using the declared root node' do
    lock = described_class.new('flake.lock', {
      'version' => 7, 'root' => 'actual-root',
      'nodes' => {
        'actual-root' => { 'inputs' => { 'package' => 'arbitrary', 'os' => %w[package os], 'nixpkgs' => %w[os nixpkgs] } },
        'arbitrary' => { 'locked' => {}, 'inputs' => { 'os' => 'arbitrary-2' } },
        'arbitrary-2' => { 'locked' => {}, 'inputs' => { 'nixpkgs' => 'leaf-3' } },
        'leaf-3' => { 'locked' => { 'type' => 'github', 'owner' => 'NixOS', 'repo' => 'nixpkgs', 'rev' => 'a' * 40 } }
      }
    })
    expect(lock.root_inputs).to eq(%w[nixpkgs os package])
    expect(lock.input_info('nixpkgs')).to include(rev: 'a' * 40, url: 'https://github.com/NixOS/nixpkgs')
    expect(lock.input_info('package/os/nixpkgs')).to eq(lock.input_info('nixpkgs'))
  end
end
