# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'tmpdir'

RSpec.describe ConfCtl::Nix do
  it 'loads machine metadata from a built JSON file' do
    Dir.mktmpdir do |dir|
      json_path = File.join(dir, 'machine-list.json')
      File.write(
        json_path,
        {
          'host.example' => {
            'key' => 'm_host_example',
            '_module' => {},
            'metaConfig' => {
              '_module' => {}
            }
          }
        }.to_json
      )

      nix = described_class.new(conf_dir: dir, max_jobs: 'auto')
      allow(nix).to receive(:nix_build_json)
        .with(['.#confctl.machinesJson'])
        .and_return([{ 'outputs' => { 'out' => json_path } }])
      allow(nix).to receive(:nix_eval_json)

      machines = nix.list_machines

      expect(machines).to eq(
        'host.example' => {
          'key' => 'm_host_example',
          'metaConfig' => {}
        }
      )
      expect(nix).not_to have_received(:nix_eval_json)
    end
  end
end

RSpec.describe ConfCtl::Nix do
  it 'rejects evaluation without a flake before running Nix' do
    Dir.mktmpdir do |dir|
      nix = described_class.new(conf_dir: dir, max_jobs: 'auto')
      cmd = instance_double(TTY::Command)
      allow(nix).to receive(:cmd).and_return(cmd)
      expect(cmd).not_to receive(:run)
      expect { nix.list_machine_fqdns }.to raise_error(ConfCtl::Error, /no flake.nix/)
    end
  end

  it 'reads cluster options from the supported flake output' do
    nix = described_class.stateless
    expect(nix).to receive(:nix_eval_json).with('.#confctl.moduleOptions').and_return([{ 'name' => 'cluster.<name>.custom' }])
    expect(nix.module_options).not_to be_empty
  end

  it 'uses package options without requiring a caller configuration' do
    nix = described_class.stateless
    expect(nix).to receive(:nix_eval_json).with(
      "#{ConfCtl.root}#moduleOptions", impure: false, settings: {}, configuration: false
    ).and_return([{ 'name' => 'confctl.nix.legacyNixPath' }])
    expect(nix.documentation_options).not_to be_empty
  end

  it 'requires impurity for the optional legacy NIX_PATH bridge' do
    nix = described_class.stateless
    allow(nix).to receive(:confctl_settings).and_return('nix' => { 'legacyNixPath' => true, 'impureEval' => false })
    expect { nix.send(:legacy_nix_path_args, ['host']) }.to raise_error(ConfCtl::Error, /requires impureEval/)
  end
end
