require 'spec_helper'
require 'confctl'
require 'confctl/cli'

RSpec.describe ConfCtl::Cli::Cluster, 'input changelog cache' do
  it 'holds the shared mirror lock across setup and log with the existing URL and options' do
    command = described_class.new({ color: 'never' }, { downgrade: false, verbose: false, patch: false }, [])
    url = 'https://github.com/NixOS/nixpkgs'
    mirror = instance_double(ConfCtl::GitRepoMirror)
    expect(ConfCtl::GitRepoMirror).to receive(:new).with(url, quiet: true).and_return(mirror)
    expect(mirror).to receive(:synchronize).ordered.and_yield
    expect(mirror).to receive(:setup).ordered
    expect(mirror).to receive(:log).with('a' * 40, 'b' * 40, opts: ['--oneline', '--max-count=50']).ordered.and_return('log')
    expect(command.send(:inputs_git_log, { 'url' => url, 'rev' => 'b' * 40 }, { 'rev' => 'a' * 40 })).to eq('log')
  end
end
