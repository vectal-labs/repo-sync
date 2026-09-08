# frozen_string_literal: true

require "json"
require "yaml"
require "minitest/autorun"
require "tmpdir"
require "fileutils"
require "open3"
require "pathname"

# Minimal artifact interface for exercising the scoped lifecycle extension.
module Cask
  module Artifact
    class PreflightBlock
      def initialize(context, block)
        @context, @block = context, block
      end

      def uninstall_phase(**_options)
        @context.instance_eval(&@block)
      end

      def cask
        @context
      end
    end
  end
end

# Execute the actual cask hooks, using native plutil against temporary files.
# Service control and quarantine changes are recorded, never executed.
class CaskLifecycleHarness
  Result = Struct.new(:exit_status, :stdout, :stderr)
  attr_reader :calls, :staged_path, :artifacts
  attr_accessor :fail_bootstrap, :never_stops, :inspect_error, :fail_skill
  attr_reader :exit_probes
  attr_reader :skill_update_locks

  def initialize(root)
    @calls = []
    @root = root
    @staged_path = Pathname.new(File.join(root, "Caskroom", "repo-sync", "2.0.0"))
    @skill_update_locks = []
    @loaded = {}
    @old_process_alive = false
    @exit_probes = 0
    @artifacts = []
    instance_eval(CaskLifecycleTest::CONFIG.fetch("custom_block"))
  end

  def uninstall_preflight(&block)
    @artifacts << Cask::Artifact::PreflightBlock.new(self, block)
  end

  def run!(command, **options)
    system_command(command, **options)
  end

  def load_updater
    @loaded["com.vectal-labs.repo-sync.updates"] = true
  end

  def updater_loaded?
    @loaded["com.vectal-labs.repo-sync.updates"]
  end

  def load_service
    @loaded["com.vectal-labs.repo-sync"] = true
    @old_process_alive = true
  end

  def probe_exit(_pid)
    @exit_probes += 1
    return 1 if never_stops || @exit_probes < 3

    @old_process_alive = false
    raise Errno::ESRCH
  end

  def system_command(command, args:, **options)
    @calls << [command, args, options]
    case command
    when "/usr/bin/plutil"
      raise "plist must stay in the test folder" unless args.last.start_with?(@root + "/")

      output, status = Open3.capture2e(command, *args)
      raise output unless status.success?

      Result.new(0, output, "")
    when "/bin/launchctl"
      raise "launchctl call needs a timeout" unless options[:timeout] && options[:timeout] <= 45

      label = args.last.end_with?(".plist") ? File.basename(args.last, ".plist") : args.last.split("/").last
      case args.first
      when "print"
        return Result.new(1, "", "permission denied") if inspect_error
        return Result.new(113, "", "Could not find service #{label}") unless @loaded[label]

        Result.new(0, label.end_with?(".updates") ? "state = waiting\n" : "  pid = 424242\n", "")
      when "bootout"
        @loaded[label] = false
        Result.new(0, "", "")
      when "bootstrap"
        raise "new daemon would overlap old daemon" if @old_process_alive
        raise "simulated service startup failure" if fail_bootstrap

        @loaded[label] = true
        Result.new(0, "", "")
      else
        raise "unexpected launchctl action"
      end
    when "/usr/bin/xattr"
      Result.new(0, "", "")
    else
      if command == File.join(staged_path, "repo-sync") && args.first == "install-updater"
        load_updater
        return Result.new(0, "", "")
      end
      if command == File.join(staged_path, "repo-sync") && args.first == "skill"
        raise "unexpected skill operation" unless [%w[skill refresh], %w[skill uninstall]].include?(args)
        raise "skill command needs a bounded timeout" unless options[:timeout] && options[:timeout] <= 30
        if args.last == "uninstall" && (@loaded.values.any? || @old_process_alive)
          raise "skill cleanup ran before the services stopped"
        end
        raise "simulated skill cleanup failure" if fail_skill

        lock_path = File.join(@root, "Library", "Caches", "repo-sync", "update.lock")
        held = File.exist?(lock_path) && File.open(lock_path, File::RDWR) { |lock| !lock.flock(File::LOCK_EX | File::LOCK_NB) }
        @skill_update_locks << held
        return Result.new(0, "", "")
      end
      raise "unexpected command: #{command}"
    end
  end
end

class CaskLifecycleTest < Minitest::Test
  CONFIG = YAML.load_file(File.expand_path("../.github/.goreleaser.yaml", __dir__))
               .fetch("homebrew_casks").first

  def setup
    @root = Dir.mktmpdir("repo-sync cask '")
    @previous_home = ENV["HOME"]
    ENV["HOME"] = @root
    @plist = File.join(@root, "Library", "LaunchAgents", "com.vectal-labs.repo-sync.plist")
    Object.const_set(:HOMEBREW_PREFIX, @root)
    @updater_plist = File.join(File.dirname(@plist), "com.vectal-labs.repo-sync.updates.plist")
    @harness = CaskLifecycleHarness.new(@root)
    @elapsed = 0
  end

  def teardown
    @harness.artifacts.each { |artifact| artifact.instance_variable_get(:@repo_sync_uninstall_lock)&.close }
    Object.send(:remove_const, :HOMEBREW_PREFIX)
    ENV["HOME"] = @previous_home
    FileUtils.remove_entry(@root)
  end

  def run_hook(phase, action, **options)
    # Advance a fake monotonic clock so stuck-process tests do not really wait.
    Process.stub(:clock_gettime, ->(_clock) { @elapsed }) do
      Process.stub(:kill, ->(_signal, pid) { @harness.probe_exit(pid) }) do
        Kernel.stub(:sleep, ->(delay) { @elapsed += delay }) do
          if phase == "pre" && action == "uninstall"
            @harness.artifacts.first.uninstall_phase(command: @harness, **options)
          else
            @harness.instance_eval(CONFIG.fetch("hooks").fetch(phase).fetch(action))
          end
        end
      end
    end
  end

  def install
    run_hook("post", "install")
  end

  def uninstall(**options)
    run_hook("pre", "uninstall", **options)
  end

  def write_plist(config_path)
    FileUtils.mkdir_p(File.dirname(@plist))
    document = {
      "Label" => "com.vectal-labs.repo-sync",
      "ProgramArguments" => ["/old/Caskroom/repo-sync", "run", "--config", config_path],
      "KeepAlive" => true,
      "EnvironmentVariables" => { "PATH" => "/opt/homebrew/bin:/usr/bin:/bin" }
    }
    File.write(@plist, JSON.generate(document))
    @harness.load_service
    document
  end

  def read_plist
    output, status = Open3.capture2e("/usr/bin/plutil", "-convert", "json", "-o", "-", @plist)
    assert status.success?, output
    JSON.parse(output)
  end

  def test_fresh_install_does_not_start_a_service
    install
    refute File.exist?(@plist)
    refute @harness.calls.any? { |command, _args| command == "/bin/launchctl" }
    skill_calls = @harness.calls.select { |_command, args| args.first == "skill" }
    assert_equal [%w[skill refresh]], skill_calls.map { |_command, args| args }
  end

  def test_upgrade_preserves_custom_config_and_all_other_settings
    expected = write_plist(File.join(@root, "custom config.json"))
    uninstall(upgrade: true)
    assert File.exist?(@plist)
    install

    expected["ProgramArguments"][0] = File.join(@root, "bin", "repo-sync")
    assert_equal expected, read_plist
    service_actions = @harness.calls.select { |command, _args| command == "/bin/launchctl" }
    assert_equal %w[print bootout print bootstrap print], service_actions.map { |_command, args| args.first }
  end

  def test_uninstall_stops_the_service_without_removing_user_files
    write_plist(File.join(@root, "config.json"))
    before = File.read(@plist)
    uninstall
    assert_equal before, File.read(@plist)
    assert @harness.calls.any? { |_command, args| args == ["bootout", "gui/#{Process.uid}/com.vectal-labs.repo-sync"] }
    assert_equal %w[skill uninstall], @harness.calls.last[1]
    assert_operator @harness.exit_probes, :>=, 3
  end

  def test_upgrade_from_old_cask_waits_before_starting_the_new_daemon
    write_plist(File.join(@root, "config.json"))
    install
    assert_operator @harness.exit_probes, :>=, 3
    assert_operator @elapsed, :>=, 0.2
  end

  def test_stuck_shutdown_aborts_upgrade_after_a_bounded_wait
    write_plist(File.join(@root, "config.json"))
    before = File.read(@plist)
    @harness.never_stops = true
    error = assert_raises(RuntimeError) { install }
    assert_match "did not stop within 45 seconds", error.message
    assert_operator @elapsed, :<=, 45.2
    assert_equal before, File.read(@plist)
    refute @harness.calls.any? { |_command, args| args.first == "bootstrap" }
  end

  def test_inspection_errors_abort_uninstall
    write_plist(File.join(@root, "config.json"))
    @harness.inspect_error = true
    error = assert_raises(RuntimeError) { uninstall }
    assert_match "Cannot inspect", error.message
    refute @harness.calls.any? { |_command, args| args.first == "bootout" }
    File.open(File.join(@root, "Library", "Caches", "repo-sync", "update.lock"), File::RDWR) do |lock|
      assert lock.flock(File::LOCK_EX | File::LOCK_NB), "failed preflight must release the lock"
    end
  end

  def test_startup_failure_is_reported
    write_plist(File.join(@root, "config.json"))
    @harness.fail_bootstrap = true
    error = assert_raises(RuntimeError) { install }
    assert_match "service startup failure", error.message
  end

  def test_user_data_is_only_removed_by_zap
    assert_empty CONFIG.fetch("uninstall", {})
    assert_equal [
      "~/Library/LaunchAgents/com.vectal-labs.repo-sync.plist",
      "~/Library/LaunchAgents/com.vectal-labs.repo-sync.updates.plist",
      "~/Library/Application Support/repo-sync",
      "~/Library/Logs/repo-sync",
      "~/Library/Caches/repo-sync"
    ], CONFIG.fetch("zap").fetch("trash")
  end

  def test_upgrade_and_reinstall_preserve_the_running_updater
    [{ upgrade: true }, { reinstall: true }].each do |options|
      write_plist(File.join(@root, "config.json"))
      File.write(@updater_plist, "existing updater")
      @harness.load_updater
      uninstall(**options)
      assert @harness.updater_loaded?
      assert_equal "existing updater", File.read(@updater_plist)
      refute @harness.calls.any? { |_command, args| args.first == "bootout" && args.last.end_with?(".updates") }
      refute @harness.calls.any? { |_command, args| args.first == "skill" }
    end
  end

  def test_true_uninstall_stops_updater_and_removes_its_schedule
    write_plist(File.join(@root, "config.json"))
    File.write(@updater_plist, "existing updater")
    @harness.load_updater
    uninstall
    refute @harness.updater_loaded?
    refute File.exist?(@updater_plist)
    assert File.exist?(@plist)
    assert_equal [true], @harness.skill_update_locks
  end

  def test_skill_only_uninstall_cleans_managed_skills_without_a_service
    uninstall
    assert_equal %w[skill uninstall], @harness.calls.last[1]
    assert_equal [false], @harness.skill_update_locks
  end

  def test_skill_refresh_runs_while_the_updater_holds_its_lock
    lock_path = File.join(@root, "Library", "Caches", "repo-sync", "update.lock")
    FileUtils.mkdir_p(File.dirname(lock_path))
    File.open(lock_path, File::RDWR | File::CREAT, 0600) do |lock|
      assert lock.flock(File::LOCK_EX | File::LOCK_NB)
      install
      assert_equal [true], @harness.skill_update_locks
    end
  end

  def test_skill_cleanup_failure_aborts_uninstall_and_releases_lock
    write_plist(File.join(@root, "config.json"))
    @harness.fail_skill = true
    error = assert_raises(RuntimeError) { uninstall }
    assert_match "skill cleanup failure", error.message
    assert File.exist?(@plist)
    File.open(File.join(@root, "Library", "Caches", "repo-sync", "update.lock"), File::RDWR) do |lock|
      assert lock.flock(File::LOCK_EX | File::LOCK_NB)
    end
  end

  def test_skill_refresh_failure_is_reported
    @harness.fail_skill = true
    error = assert_raises(RuntimeError) { install }
    assert_match "skill cleanup failure", error.message
  end

  def test_successful_preflight_retains_lock_during_package_removal
    write_plist(File.join(@root, "config.json"))
    uninstall
    File.open(File.join(@root, "Library", "Caches", "repo-sync", "update.lock"), File::RDWR) do |lock|
      refute lock.flock(File::LOCK_EX | File::LOCK_NB), "updating must stay blocked after preflight"
    end
  end

  def test_direct_uninstall_refuses_to_interrupt_an_update
    write_plist(File.join(@root, "config.json"))
    File.write(@updater_plist, "existing updater")
    @harness.load_updater
    lock_path = File.join(@root, "Library", "Caches", "repo-sync", "update.lock")
    FileUtils.mkdir_p(File.dirname(lock_path))
    File.open(lock_path, File::RDWR | File::CREAT, 0600) do |lock|
      assert lock.flock(File::LOCK_EX | File::LOCK_NB)
      error = assert_raises(RuntimeError) { uninstall }
      assert_match "update is in progress", error.message
    end
    assert @harness.updater_loaded?
    assert File.exist?(@updater_plist)
    assert File.exist?(@plist)
    assert_empty @harness.calls
  end

  def test_upgrade_enrolls_existing_setup_with_custom_config
    config = File.join(@root, "custom config.json")
    write_plist(config)
    install
    assert @harness.updater_loaded?
    invocation = @harness.calls.find { |command, _args| command == File.join(@harness.staged_path, "repo-sync") }
    assert_equal ["install-updater", "--config", config], invocation[1]
  end

  def test_upgrade_enrolls_config_with_equals_argument
    config = File.join(@root, "custom config=notes.json")
    document = write_plist(config)
    document["ProgramArguments"] = ["/old/repo-sync", "run", "--config=#{config}"]
    File.write(@plist, JSON.generate(document))
    install
    invocation = @harness.calls.find { |command, _args| command == File.join(@harness.staged_path, "repo-sync") }
    assert_equal ["install-updater", "--config", config], invocation[1]
    assert_equal [File.join(@root, "bin", "repo-sync"), "run", "--config=#{config}"], read_plist["ProgramArguments"]
  end

  def test_homebrew_installs_required_tools
    assert_equal %w[gh git], CONFIG.fetch("dependencies").map { |dependency| dependency.fetch("formula") }.sort
  end
  if ENV["REPO_SYNC_HOMEBREW_TEST"] == "1"
    def test_actual_homebrew_loader_preserves_upgrade_flags
      script = File.join(@root, "homebrew-loader-test.rb")
      config_path = File.expand_path("../.github/.goreleaser.yaml", __dir__)
      File.write(script, "REPO_SYNC_CASK_CONFIG = #{config_path.inspect}\n" + <<~'RUBY')
        require "cask/cask_loader"
        require "yaml"
        require "tmpdir"
        require "fileutils"
        config = YAML.load_file(REPO_SYNC_CASK_CONFIG).fetch("homebrew_casks").first
        Dir.mktmpdir("repo-sync-cask-loader-") do |root|
          ENV["HOME"] = root
          path = File.join(root, "repo-sync.rb")
          File.write(path, <<~CASK)
            cask "repo-sync" do
              version "2.0.0"
              sha256 :no_check
              url "https://example.invalid/repo-sync.tar.gz"
              name "repo-sync"
              desc "Synchronizes repositories"
              homepage "https://github.com/vectal-labs/repo-sync"
              binary "repo-sync"
              #{config.fetch("custom_block")}
              postflight do
                #{config.fetch("hooks").fetch("post").fetch("install")}
              end
            end
          CASK
          calls = []
          cask = nil
          result = Struct.new(:exit_status, :stdout, :stderr)
          SystemCommand.singleton_class.send(:define_method, :run!) do |executable, **options|
            args = options.fetch(:args)
            calls << args
            if executable == "/bin/launchctl"
              result.new(113, "", "Could not find service")
            elsif executable == "/usr/bin/xattr"
              result.new(0, "", "")
            elsif executable == cask.staged_path.join("repo-sync").to_s && [%w[skill refresh], %w[skill uninstall]].include?(args)
              raise "skill hook timeout missing" unless options[:timeout] && options[:timeout] <= 30
              result.new(0, "", "")
            else
              raise "unexpected real command #{executable} #{args}"
            end
          end
          cask = Cask::CaskLoader::FromContentLoader.new(File.read(path)).load(config: nil)
          artifact = cask.artifacts.find { |item| item.is_a?(Cask::Artifact::PreflightBlock) }
          raise "scoped lifecycle extension missing" unless artifact.singleton_class.ancestors.any? { |ancestor| ancestor.name&.end_with?("RepoSyncCaskLifecycle::UninstallPhase") }
          postflight = cask.artifacts.find { |item| item.is_a?(Cask::Artifact::PostflightBlock) }
          postflight.install_phase
          raise "fresh install missed skill refresh" unless calls.include?(%w[skill refresh])
          raise "fresh install touched launchd" if calls.any? { |args| %w[print bootout bootstrap].include?(args.first) }
          updater_plist = File.join(root, "Library", "LaunchAgents", "com.vectal-labs.repo-sync.updates.plist")
          FileUtils.mkdir_p(File.dirname(updater_plist))
          File.write(updater_plist, "keep across upgrades")
          [{upgrade: true}, {reinstall: true}].each do |flags|
            calls.clear
            artifact.uninstall_phase(**flags)
            raise "upgrade touched updater" if calls.any? { |args| args.last.end_with?(".updates") }
            raise "upgrade removed skills" if calls.any? { |args| args.first == "skill" }
            raise "upgrade removed updater schedule" unless File.exist?(updater_plist)
          end
          calls.clear
          artifact.uninstall_phase(upgrade: false, reinstall: false)
          raise "uninstall missed updater" unless calls.any? { |args| args.last.end_with?(".updates") }
          raise "uninstall left updater schedule" if File.exist?(updater_plist)
          raise "uninstall missed skill cleanup" unless calls.include?(%w[skill uninstall])
          File.open(File.join(root, "Library", "Caches", "repo-sync", "update.lock"), File::RDWR) do |lock|
            raise "preflight released update lock before package removal" if lock.flock(File::LOCK_EX | File::LOCK_NB)
          end
          puts "Actual Homebrew loader and artifact flags: passed"
        end
      RUBY
      output, status = Open3.capture2e({
        "HOMEBREW_DEVELOPER" => "1",
        "HOMEBREW_NO_AUTO_UPDATE" => "1",
        "HOMEBREW_NO_ANALYTICS" => "1",
        "HOMEBREW_NO_INSTALL_FROM_API" => "1"
      }, "brew", "ruby", script)
      assert status.success?, output
      assert_includes output, "Actual Homebrew loader and artifact flags: passed"
    end
  end

end
