# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'confctl/cli'
require 'tmpdir'
require 'fileutils'

RSpec.describe ConfCtl::Generation::Build do
  let(:host) { 'nested/host' }
  let(:date) { Time.utc(2026, 10, 8, 12) }
  let(:inputs) { { 'nixpkgs' => File.join(@tmp, 'nixpkgs') } }
  let(:info) { { 'nixpkgs' => { 'rev' => 'abcd1234' } } }

  around do |example|
    Dir.mktmpdir do |tmp|
      @tmp = tmp
      example.run
    end
  end

  before do
    allow(ConfCtl::ConfDir).to receive(:generation_dir).and_return(File.join(@tmp, 'generations'))
    allow(ConfCtl::GCRoot).to receive(:dir).and_return(File.join(@tmp, 'gcroots'))
    @toplevel = File.join(@tmp, 'system')
    FileUtils.mkdir_p(@toplevel)
    FileUtils.mkdir_p(inputs['nixpkgs'])
    File.symlink('/nix/store/kernel-linux-6.12.3/bzImage', File.join(@toplevel, 'kernel'))
  end

  def create_generation(time = date)
    generation = described_class.new(host)
    generation.create_flake(@toplevel, File.join(@tmp, 'rollback'), inputs:, inputs_info: info, date: time)
    generation.save
    generation
  end

  def seed_record(name, data)
    path = File.join(ConfCtl::ConfDir.generation_dir, ConfCtl.safe_host_name(host), name)
    FileUtils.mkdir_p(path)
    File.write(File.join(path, 'generation.json'), data.is_a?(String) ? data : JSON.generate(data))
    path
  end

  def inventory
    ConfCtl::Generation::BuildList.new(host)
  end

  it 'round trips the existing flake schema and preserves input links and root names' do
    gen = create_generation
    list = inventory
    loaded = list[gen.name]
    expect(loaded.mode).to eq('flakes')
    expect(loaded.inputs).to eq(inputs)
    expect(loaded.inputs_info).to eq(info)
    expect(loaded.kernel_version).to eq('6.12.3')
    expect(list.find(@toplevel, inputs)).to eq(loaded)
    expect(list.find(@toplevel, {})).to be_nil
    expect(JSON.parse(File.read(File.join(gen.dir, 'generation.json'))).keys.sort)
      .to eq(%w[auto_rollback date inputs inputs_info mode toplevel])
    expect(File.readlink(File.join(gen.dir, 'nixpkgs.input'))).to eq(inputs['nixpkgs'])
    expect(File.readlink(File.join(ConfCtl::GCRoot.dir, "nested:host-generation-#{gen.name}-input.nixpkgs")))
      .to eq(File.join(gen.dir, 'nixpkgs.input'))
    loaded.destroy
    expect(File).not_to exist(gen.dir)
    expect(Dir.children(ConfCtl::GCRoot.dir)).to be_empty
  end

  it 'retains the accepted inputsInfo spelling' do
    gen = create_generation
    path = File.join(gen.dir, 'generation.json')
    payload = JSON.parse(File.read(path))
    payload['inputsInfo'] = payload.delete('inputs_info')
    File.write(path, JSON.generate(payload))
    expect(inventory[gen.name].inputs_info).to eq(info)
  end

  [nil, 'swpins', 'unknown'].each do |mode|
    it "rejects #{mode.inspect} mode before parsing any pin payload" do
      payload = { 'swpins' => 'unreadable', 'inputs' => inputs }
      payload['mode'] = mode if mode
      seed_record('unsupported', payload)
      expect { described_class.new(host).load('unsupported') }
        .to raise_error(described_class::UnsupportedFormat, /unsupported generation mode/)
    end
  end

  ['{', { 'mode' => 'flakes', 'date' => 'bad', 'toplevel' => 123 }].each do |payload|
    it 'distinguishes invalid JSON and flake payloads from unsupported formats' do
      seed_record('invalid', payload)
      expect { described_class.new(host).load('invalid') }
        .to raise_error(described_class::InvalidGeneration, /generation.json/)
    end
  end

  it 'leaves excluded records, old input links, roots and current untouched during inventory and new builds' do
    legacy = seed_record('legacy', { 'mode' => 'swpins' })
    File.symlink('/nix/store/old-input', File.join(legacy, 'nixpkgs.swpin'))
    FileUtils.mkdir_p(ConfCtl::GCRoot.dir)
    root = File.join(ConfCtl::GCRoot.dir, 'old-swpin-root')
    File.symlink(File.join(legacy, 'nixpkgs.swpin'), root)
    current = File.join(File.dirname(legacy), 'current')
    File.symlink('legacy', current)
    original = File.binread(File.join(legacy, 'generation.json'))
    supported = create_generation
    list = nil
    expect { list = inventory }.to output(/legacy.*unsupported.*left untouched/m).to_stderr
    expect(list.count).to eq(1)
    expect(list.current).to be_nil
    expect { list.select_current! }.to raise_error(ConfCtl::Error, /unsupported/)
    expect { list['legacy'] }.to raise_error(ConfCtl::Error, /unsupported/)
    expect { list.at_offset(-1) }.to raise_error(ConfCtl::Error, /ambiguous/)
    expect { list.require_resolved_current! }.to raise_error(ConfCtl::Error)
    expect(File.readlink(current)).to eq('legacy')
    list.current = supported
    expect(list.select_current!).to eq(supported)
    expect(File.readlink(current)).to eq(supported.name)
    expect(File.binread(File.join(legacy, 'generation.json'))).to eq(original)
    expect(File.readlink(root)).to eq(File.join(legacy, 'nixpkgs.swpin'))
    expect(File.readlink(File.join(legacy, 'nixpkgs.swpin'))).to eq('/nix/store/old-input')
  end

  it 'does not replace a broken current link with the newest supported generation' do
    gen = create_generation
    link = File.join(File.dirname(gen.dir), 'current')
    File.symlink('missing', link)
    list = inventory
    expect(list.current).to be_nil
    expect { list.select_current! }.to raise_error(ConfCtl::Error, /missing generation missing/)
    expect(File.readlink(link)).to eq('missing')
  end

  %i[broken external].each do |target_type|
    it "rejects a #{target_type} current target with a supported local basename without changing records or roots" do
      gen = create_generation
      link = File.join(File.dirname(gen.dir), 'current')
      target = target_type == :broken ? File.join('missing-directory', gen.name) : File.join(@tmp, 'external-directory', gen.name)
      if target_type == :external
        FileUtils.mkdir_p(File.dirname(target))
        FileUtils.cp_r(gen.dir, target)
      end
      File.symlink(target, link)
      original = File.binread(File.join(gen.dir, 'generation.json'))
      external_original = File.binread(File.join(target, 'generation.json')) if target_type == :external
      roots = Dir.children(ConfCtl::GCRoot.dir).to_h do |name|
        [name, File.readlink(File.join(ConfCtl::GCRoot.dir, name))]
      end

      list = inventory
      expect(list.current).to be_nil
      expect(list[gen.name].current).to be_falsey
      expect(list.current_error).to include(link, 'target')
      expect { list.select_current! }.to raise_error(ConfCtl::Error, /current/)
      machines = ConfCtl::MachineList.new(machines: { host => double })
      command = ConfCtl::Cli::Generation.new({}, { local: true }, [])
      allow(ConfCtl::Settings.instance).to receive(:build_generations).and_return({})
      expect { command.send(:select_generations, machines, 'current') }.to raise_error(ConfCtl::Error, /current/)
      expect { command.send(:select_generations, machines, 'old') }.to raise_error(ConfCtl::Error, /current/)
      expect { command.send(:build_generations_rotate, machines) }.to raise_error(ConfCtl::Error, /current/)

      expect(File.readlink(link)).to eq(target)
      expect(File.binread(File.join(gen.dir, 'generation.json'))).to eq(original)
      expect(Dir.children(ConfCtl::GCRoot.dir).sort).to eq(roots.keys.sort)
      roots.each do |name, root_target|
        expect(File.readlink(File.join(ConfCtl::GCRoot.dir, name))).to eq(root_target)
      end
      if target_type == :external
        expect(File.binread(File.join(target, 'generation.json'))).to eq(external_original)
      else
        expect(File).not_to exist(File.expand_path(target, File.dirname(gen.dir)))
      end
    end
  end

  %i[relative absolute].each do |target_type|
    it "accepts a valid #{target_type} current target within the local generation directory" do
      gen = create_generation
      create_generation(date + 60)
      link = File.join(File.dirname(gen.dir), 'current')
      target = target_type == :relative ? gen.name : gen.dir
      File.symlink(target, link)
      list = inventory
      expect(list.current_error).to be_nil
      expect(list.select_current!.name).to eq(gen.name)
      expect(list[gen.name].current).to be(true)
      expect(File.readlink(link)).to eq(target)
    end
  end

  it 'reports the absent-current convention when unsupported records are present' do
    seed_record('legacy', {})
    gen = create_generation
    list = nil
    expect { list = inventory }.to output(/selecting newest supported generation/).to_stderr
    expect(list.current.name).to eq(gen.name)
  end

  it 'refuses mixed-host unsupported deployment selection before copying or activation' do
    gen = create_generation
    seed_record('legacy', {})
    command = ConfCtl::Cli::Cluster.new({ color: 'never' }, { yes: true, generation: 'legacy' }, [nil])
    machines = ConfCtl::MachineList.new(machines: { 'empty' => double, host => double })
    allow(command).to receive(:select_machines).and_return(machines)
    allow(machines).to receive(:managed).and_return(machines)
    allow(command).to receive(:list_machines)
    allow(command).to receive(:deploy_in_bulk)
    expect { command.deploy }.to raise_error(ConfCtl::Error, /unsupported/)
    expect(command).not_to have_received(:deploy_in_bulk)
    expect(inventory[gen.name]).not_to be_nil
  end

  it 'blocks numeric local CLI selection and retention with an unresolved current link' do
    gen = create_generation
    legacy = seed_record('legacy', {})
    File.symlink('legacy', File.join(File.dirname(legacy), 'current'))
    machines = ConfCtl::MachineList.new(machines: { host => double })
    command = ConfCtl::Cli::Generation.new({}, { local: true }, [])
    expect { command.send(:select_generations, machines, '-1') }.to raise_error(ConfCtl::Error, /ambiguous/)
    expect { command.send(:select_generations, machines, 'old') }.to raise_error(ConfCtl::Error, /current/)
    expect(File).to exist(gen.dir)
  end
  it 'deduplicates a new flake build in a mixed inventory and deliberately updates current' do
    saved = create_generation
    legacy = seed_record('legacy', {})
    current = File.join(File.dirname(legacy), 'current')
    File.symlink('legacy', current)
    original = File.read(File.join(legacy, 'generation.json'))
    nix = ConfCtl::Nix.stateless
    allow(nix).to receive(:build_plan).and_return(host => { 'key' => 'm_host', 'inputs' => inputs })
    allow(nix).to receive(:legacy_nix_path_args).and_return([])
    allow(nix).to receive(:nix_build_json).and_return([])
    allow(nix).to receive(:build_outputs_for_keys).and_return(
      'm_host' => { 'toplevel' => @toplevel, 'autoRollback' => File.join(@tmp, 'rollback') }
    )
    result = nix.build_attributes(hosts: [host])
    expect(result.fetch(host).name).to eq(saved.name)
    expect(File.readlink(current)).to eq(saved.name)
    expect(File.read(File.join(legacy, 'generation.json'))).to eq(original)
    expect(inventory.count).to eq(1)
  end

  it 'keeps remote-only generation listing and removal independent of excluded local records' do
    seed_record('legacy', {})
    machine = instance_double(ConfCtl::Machine, name: host, carried?: false)
    control = instance_double(ConfCtl::MachineControl)
    remote = ConfCtl::Generation::Host.new(machine, '/profile', 5, @toplevel, date, nil, mc: control)
    generations = instance_double(ConfCtl::Generation::HostList)
    allow(generations).to receive(:each).and_yield(remote)
    status = instance_double(ConfCtl::MachineStatus, generations:)
    allow(status).to receive(:query)
    allow(ConfCtl::MachineStatus).to receive(:new).with(machine).and_return(status)
    command = ConfCtl::Cli::Generation.new({}, { remote: true }, [])
    machines = ConfCtl::MachineList.new(machines: { host => machine })
    selected = command.send(:select_generations, machines, '0')
    expect(selected.count).to eq(1)
    expect(selected.first.inputs_info).to be_nil
    expect(control).to receive(:execute).with('nix-env', '-p', '/profile', '--delete-generations', '5')
    selected.first.destroy
  end
end
