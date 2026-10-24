require 'spec_helper'
require 'confctl/git_repo_mirror'
require 'rbconfig'
require 'io/wait'

RSpec.describe ConfCtl::GitRepoMirror do
  around do |example|
    Dir.mktmpdir('confctl-git-mirror-') do |dir|
      @dir = dir
      @origin = File.join(dir, 'origin')
      @cache = File.join(dir, 'cache')
      @git_env = ENV.keys.grep(/\AGIT_/).to_h { |key| [key, nil] }.merge(
        'GIT_CONFIG_GLOBAL' => File::NULL, 'GIT_CONFIG_NOSYSTEM' => '1', 'GIT_TERMINAL_PROMPT' => '0'
      )
      git('init', '--initial-branch=main', @origin)
      git('-C', @origin, 'config', 'user.name', 'fixture')
      git('-C', @origin, 'config', 'user.email', 'fixture@example.invalid')
      @base = commit('base', 'file', 'base')
      @forward = commit('forward', 'file', 'forward')
      git('-C', @origin, 'checkout', '-b', 'side', @base)
      @side = commit('side', 'side-file', 'side')
      git('-C', @origin, 'checkout', 'main')
      git('-C', @origin, 'merge', '--no-ff', 'side', '-m', 'merge')
      @merge = git('-C', @origin, 'rev-parse', 'HEAD').strip
      git('-C', @origin, 'revert', '--no-edit', @forward)
      @revert = git('-C', @origin, 'rev-parse', 'HEAD').strip
      @mirror = described_class.new(@origin, quiet: true)
      @path = File.join(@cache, 'git-mirrors', @mirror.name)
      example.run
    end
  end

  before { allow(ConfCtl).to receive(:cache_dir).and_return(@cache) }

  def git(*args)
    out, err, status = Open3.capture3(@git_env, 'git', *args)
    raise "fixture Git failed: #{err}" unless status.success?

    out
  end

  def commit(message, file, contents)
    File.write(File.join(@origin, file), contents)
    git('-C', @origin, 'add', file)
    git('-C', @origin, 'commit', '-m', message)
    git('-C', @origin, 'rev-parse', 'HEAD').strip
  end

  def cached_git(*args)
    git('--git-dir', @path, *args)
  end

  def under_fixture_environment
    keys = @git_env.keys | %w[GIT_DIR GIT_OBJECT_DIRECTORY GIT_CONFIG]
    previous = keys.to_h { |key| [key, ENV.fetch(key, nil)] }
    @git_env.each { |key, value| value.nil? ? ENV.delete(key) : ENV[key] = value }
    yield
  ensure
    previous&.each { |key, value| value.nil? ? ENV.delete(key) : ENV[key] = value }
  end

  def compare(old_rev, new_rev)
    under_fixture_environment { @mirror.compare(old_rev, new_rev) }
  end

  def setup_mirror
    under_fixture_environment { @mirror.setup }
  end

  it 'uses the existing exact-URL SHA256 cache layout and a sibling lock' do
    expect(compare(@base, @forward)).to eq(:ahead)
    expect(@mirror.name).to eq(Digest::SHA256.hexdigest(@origin))
    expect(File).to be_directory(@path)
    expect(File).to be_file("#{@path}.lock")
    expect(cached_git('rev-parse', '--is-bare-repository').strip).to eq('true')
    expect(cached_git('config', 'remote.origin.url').strip).to eq(@origin)
  end

  it 'accepts equality without a cache or any Git child' do
    expect(@mirror).not_to receive(:git)
    expect(compare(@base, @base)).to eq(:identical)
    expect(File).not_to exist(@path)
  end

  it 'distinguishes ahead, behind and diverged history using real Git' do
    expect(compare(@base, @forward)).to eq(:ahead)
    expect(compare(@forward, @base)).to eq(:behind)
    expect(compare(@forward, @side)).to eq(:diverged)
  end

  it 'accepts containing merges and subsequent reverts' do
    expect(compare(@side, @merge)).to eq(:ahead)
    expect(compare(@forward, @revert)).to eq(:ahead)
  end

  it 'rejects unrelated roots and equivalent cherry-picks as divergent history' do
    git('-C', @origin, 'checkout', 'side')
    git('-C', @origin, 'cherry-pick', @forward)
    cherry_pick = git('-C', @origin, 'rev-parse', 'HEAD').strip
    git('-C', @origin, 'checkout', '--orphan', 'unrelated')
    git('-C', @origin, 'rm', '--cached', '-r', '.')
    unrelated = commit('unrelated root', 'unrelated-file', 'unrelated')
    expect(compare(@forward, cherry_pick)).to eq(:diverged)
    expect(compare(@forward, unrelated)).to eq(:diverged)
  end

  it 'uses complete warm objects without contacting an unavailable origin' do
    setup_mirror
    File.rename(@origin, "#{@origin}-offline")
    expect(compare(@base, @forward)).to eq(:ahead)
    expect(compare(@forward, @side)).to eq(:diverged)
  end

  it 'fetches a missing full SHA without refreshing unrelated mirror heads' do
    setup_mirror
    previous_main = cached_git('rev-parse', 'refs/heads/main').strip
    newer = commit('newer', 'new-file', 'new')
    expect(compare(@revert, newer)).to eq(:ahead)
    expect(cached_git('cat-file', '-t', newer).strip).to eq('commit')
    expect(cached_git('rev-parse', 'refs/heads/main').strip).to eq(previous_main)
  end

  it 'leaves no authoritative partial cache after clone failure' do
    mirror = described_class.new(File.join(@dir, 'unavailable'), quiet: true)
    expect { under_fixture_environment { mirror.compare(@base, @forward) } }.to raise_error(described_class::Error)
    path = File.join(@cache, 'git-mirrors', mirror.name)
    expect(File).not_to exist(path)
    expect(Dir.glob(File.join(@cache, 'git-mirrors', ".#{mirror.name}-*"))).to be_empty
    git('clone', @origin, File.join(@dir, 'unavailable'))
    expect(under_fixture_environment { mirror.compare(@base, @forward) }).to eq(:ahead)
  end

  it 'does not overwrite an invalid existing cache' do
    FileUtils.mkdir_p(@path)
    marker = File.join(@path, 'keep')
    File.write(marker, 'original')
    expect { compare(@base, @forward) }.to raise_error(described_class::Error, /Invalid Git mirror/)
    expect(File.read(marker)).to eq('original')
  end

  it 'rejects an unexpected origin without fetching or replacing the cache' do
    setup_mirror
    cached_git('config', 'remote.origin.url', File.join(@dir, 'other'))
    expect { compare(@base, @forward) }.to raise_error(described_class::Error, /unexpected origin/)
  end

  it 'rejects shallow mirrors' do
    FileUtils.mkdir_p(File.dirname(@path))
    git('clone', '--mirror', '--depth=1', "file://#{@origin}", @path)
    expect { compare(@base, @revert) }.to raise_error(described_class::Error, /shallow/)
  end

  %w[info/grafts objects/info/alternates objects/info/http-alternates commondir].each do |entry|
    it "rejects externally redirected history through #{entry}" do
      setup_mirror
      path = File.join(@path, entry)
      FileUtils.mkdir_p(File.dirname(path))
      File.write(path, "#{@origin}\n")
      expect { compare(@base, @forward) }.to raise_error(described_class::Error, /external object history/)
    end
  end

  %w[extensions.partialClone remote.origin.promisor remote.origin.partialclonefilter].each do |key|
    it "rejects partial-clone configuration #{key}" do
      setup_mirror
      cached_git('config', key, key.end_with?('promisor') ? 'true' : 'origin')
      expect { compare(@base, @forward) }.to raise_error(described_class::Error, /partial/)
    end
  end

  it 'rejects a symlinked external object directory' do
    setup_mirror
    File.rename(File.join(@path, 'objects'), File.join(@dir, 'outside-objects'))
    File.symlink(File.join(@dir, 'outside-objects'), File.join(@path, 'objects'))
    expect { compare(@base, @forward) }.to raise_error(described_class::Error, /self-contained/)
  end

  it 'rejects a symlinked external pack store' do
    setup_mirror
    pack = File.join(@path, 'objects', 'pack')
    File.rename(pack, File.join(@dir, 'outside-pack'))
    File.symlink(File.join(@dir, 'outside-pack'), pack)
    expect { compare(@base, @forward) }.to raise_error(described_class::Error, /self-contained/)
  end

  it 'does not peel annotated tags or accept tree/blob objects as commits' do
    git('-C', @origin, 'tag', '-a', 'tagged', '-m', 'tag', @base)
    tag = git('-C', @origin, 'rev-parse', 'tagged').strip
    tree = git('-C', @origin, 'rev-parse', "#{@base}^{tree}").strip
    blob = git('-C', @origin, 'rev-parse', "#{@base}:file").strip
    [tag, tree, blob].each do |object|
      expect { compare(object, @forward) }.to raise_error(described_class::Error, /exact commit objects/)
    end
  end

  it 'returns an evidence error for an unavailable full SHA' do
    expect { compare(@base, 'f' * 40) }.to raise_error(described_class::Error, /operation failed/)
  end

  it 'disables replacement refs rather than allowing a false containing merge' do
    setup_mirror
    cached_git('replace', @forward, @merge)
    expect(compare(@side, @forward)).to eq(:diverged)
  end

  it 'fails before a negative conclusion when a reachable parent object is missing' do
    setup_mirror
    File.unlink(File.join(@path, 'objects', @base[0, 2], @base[2..]))
    allow(@mirror).to receive(:ancestor?).and_return(false)
    expect { compare(@forward, @side) }.to raise_error(described_class::Error, /operation failed/)
  end

  it 'does not require irrelevant tree or blob objects for commit-parent ancestry' do
    setup_mirror
    tree = cached_git('rev-parse', "#{@base}^{tree}").strip
    blob = cached_git('rev-parse', "#{@base}:file").strip
    [tree, blob].each { |object| File.unlink(File.join(@path, 'objects', object[0, 2], object[2..])) }
    expect(compare(@base, @forward)).to eq(:ahead)
    expect(compare(@forward, @side)).to eq(:diverged)
  end

  it 'treats unexpected merge-base status as an error, never a rejection' do
    setup_mirror
    allow(@mirror).to receive(:git).and_call_original
    allow(@mirror).to receive(:git).with('--git-dir', @path, 'merge-base', '--is-ancestor', @base, @forward).and_return(['', 128])
    expect { compare(@base, @forward) }.to raise_error(described_class::Error, /checking commit ancestry/)
  end

  it 'ignores enclosing repository/object redirection without changing the parent environment' do
    setup_mirror
    under_fixture_environment do
      ENV['GIT_DIR'] = File.join(@dir, 'wrong')
      ENV['GIT_OBJECT_DIRECTORY'] = File.join(@dir, 'wrong-objects')
      ENV['GIT_CONFIG'] = File.join(@dir, 'wrong-config')
      expect(@mirror.compare(@base, @forward)).to eq(:ahead)
      expect(ENV.fetch('GIT_DIR', nil)).to eq(File.join(@dir, 'wrong'))
    end
  end

  it 'releases the URL lock and owned clone attempt on interruption' do
    allow(@mirror).to receive(:git!).and_wrap_original do |original, *args, **options|
      if args.first == 'clone'
        FileUtils.mkdir_p(args.last)
        raise Interrupt
      end
      original.call(*args, **options)
    end
    expect { compare(@base, @forward) }.to raise_error(Interrupt)
    expect(File).not_to exist(@path)
    expect(Dir.glob(File.join(@cache, 'git-mirrors', ".#{@mirror.name}-*"))).to be_empty
    expect(@mirror.synchronize { true }).to be(true)
  end

  it 'reports a safe cache key without copying Git transport diagnostics' do
    allow(Open3).to receive(:capture3).and_raise(IOError, 'untrusted transport details')
    expect { compare(@base, @forward) }.to raise_error(described_class::Error) do |error|
      expect(error.message).to include(@mirror.name)
      expect(error.message).not_to include('untrusted transport details')
    end
  end

  it 'preserves ordinary Git URL configuration and its warm changelog cache' do
    configured_url = 'https://fixture.invalid/repository'
    config = File.join(@dir, 'fixture-git-config')
    git('config', '--file', config, "url.#{@origin}.insteadOf", configured_url)
    mirror = described_class.new(configured_url, quiet: true)
    under_fixture_environment do
      ENV['GIT_CONFIG_GLOBAL'] = config
      expect(mirror.compare(@base, @forward)).to eq(:ahead)
      mirror.synchronize do
        mirror.setup
        expect(mirror.log(@base, @forward, opts: ['--oneline'])).to include('forward')
      end
    end
  end

  it 'preserves fabricated SSH transport configuration without accessing a network or credentials' do
    configured_url = 'fixture://repository'
    marker = File.join(@dir, 'transport-used')
    helper = File.join(@dir, 'ssh-fixture.rb')
    File.write(helper, "File.write(#{marker.inspect}, 'called')\nexit 1\n")
    config = File.join(@dir, 'fixture-git-config')
    git('config', '--file', config, 'url.ssh://fixture.invalid/repository.insteadOf', configured_url)
    git('config', '--file', config, 'core.sshCommand', "#{RbConfig.ruby} --disable=rubyopt #{helper}")
    mirror = described_class.new(configured_url, quiet: true)
    under_fixture_environment do
      ENV['GIT_CONFIG_GLOBAL'] = config
      expect { mirror.compare(@base, @forward) }.to raise_error(described_class::Error)
      expect(File.read(marker)).to eq('called')
    end
  end

  it 'serializes concurrent cold comparisons and reuses their mirror for changelogs without deadlocking' do
    program = <<~RUBY
      module ConfCtl
        def self.cache_dir = ARGV[1]
      end
      $stdout.sync = true
      puts 'ready'
      puts ConfCtl::GitRepoMirror.new(ARGV[0]).compare(ARGV[2], ARGV[3])
    RUBY
    under_fixture_environment do
      @mirror.synchronize do
        @child = Open3.popen3(RbConfig.ruby, '-I', File.expand_path('../lib', __dir__),
                              '-rconfctl/git_repo_mirror', '-e', program, @origin, @cache, @base, @forward)
        expect(@child[1].gets).to eq("ready\n")
        expect(@child[1].wait_readable(0.1)).to be_nil
        expect(@mirror.compare(@base, @forward)).to eq(:ahead)
        File.rename(@origin, "#{@origin}-offline")
      end
      expect(@child[1].gets).to eq("ahead\n")
      expect(@child[3].value.success?).to be(true), @child[2].read
      File.rename("#{@origin}-offline", @origin)
      @mirror.synchronize do
        @mirror.setup
        expect(@mirror.log(@base, @forward, opts: ['--oneline'])).to include('forward')
      end
    ensure
      if @child && @child[3].alive?
        Process.kill('TERM', @child[3].pid)
        @child[3].value
      end
      @child&.first(3)&.each { |io| io.close unless io.closed? }
    end
  end
end
