# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'tmpdir'

RSpec.describe ConfCtl::NixFlake do
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
