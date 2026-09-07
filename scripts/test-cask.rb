# frozen_string_literal: true

require "json"
require "yaml"
require "minitest/autorun"
require "tmpdir"
require "fileutils"
require "open3"

# Execute the actual cask hooks, using native plutil against temporary files.
# Service control and quarantine changes are recorded, never executed.
class CaskLifecycleHarness
  Result = Struct.new(:exit_status, :stdout, :stderr)
  attr_reader :calls, :staged_path
  attr_accessor :fail_bootstrap, :never_stops, :inspect_error
  attr_reader :exit_probes

  def initialize(root)
    @calls = []
    @root = root
    @staged_path = File.join(root, "Caskroom", "repo-sync", "2.0.0")
    @loaded = false
    @old_process_alive = false
    @exit_probes = 0
  end

  def load_service
    @loaded = true
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

      case args.first
      when "print"
        return Result.new(1, "", "permission denied") if inspect_error
        return Result.new(113, "", "Could not find service com.vectal-labs.repo-sync") unless @loaded

        Result.new(0, "  pid = 424242\n", "")
      when "bootout"
        @loaded = false
        Result.new(0, "", "")
      when "bootstrap"
        raise "new daemon would overlap old daemon" if @old_process_alive
        raise "simulated service startup failure" if fail_bootstrap

        @loaded = true
        Result.new(0, "", "")
      else
        raise "unexpected launchctl action"
      end
    when "/usr/bin/xattr"
      Result.new(0, "", "")
    else
      raise "unexpected command: #{command}"
    end
  end
end

class CaskLifecycleTest < Minitest::Test
  CONFIG = YAML.load_file(File.expand_path("../.github/.goreleaser.yaml", __dir__))
               .fetch("homebrew_casks").first
  Object.class_eval(CONFIG.fetch("custom_block"))

  def setup
    @root = Dir.mktmpdir("repo-sync cask '")
    @previous_home = ENV["HOME"]
    ENV["HOME"] = @root
    @plist = File.join(@root, "Library", "LaunchAgents", "com.vectal-labs.repo-sync.plist")
    @harness = CaskLifecycleHarness.new(@root)
    @elapsed = 0
  end

  def teardown
    ENV["HOME"] = @previous_home
    FileUtils.remove_entry(@root)
  end

  def run_hook(phase, action)
    # Advance a fake monotonic clock so stuck-process tests do not really wait.
    Process.stub(:clock_gettime, ->(_clock) { @elapsed }) do
      Process.stub(:kill, ->(_signal, pid) { @harness.probe_exit(pid) }) do
        Kernel.stub(:sleep, ->(delay) { @elapsed += delay }) do
          @harness.instance_eval(CONFIG.fetch("hooks").fetch(phase).fetch(action))
        end
      end
    end
  end

  def install
    run_hook("post", "install")
  end

  def uninstall
    run_hook("pre", "uninstall")
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
  end

  def test_upgrade_preserves_custom_config_and_all_other_settings
    expected = write_plist(File.join(@root, "custom config.json"))
    uninstall
    assert File.exist?(@plist)
    install

    expected["ProgramArguments"][0] = File.join(@harness.staged_path, "repo-sync")
    assert_equal expected, read_plist
    service_actions = @harness.calls.select { |command, _args| command == "/bin/launchctl" }
    assert_equal %w[print bootout print bootstrap print], service_actions.map { |_command, args| args.first }
  end

  def test_uninstall_stops_the_service_without_removing_user_files
    write_plist(File.join(@root, "config.json"))
    before = File.read(@plist)
    uninstall
    assert_equal before, File.read(@plist)
    assert_equal ["bootout", "gui/#{Process.uid}/com.vectal-labs.repo-sync"], @harness.calls.last[1]
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
      "~/Library/Application Support/repo-sync",
      "~/Library/Logs/repo-sync",
      "~/Library/Caches/repo-sync"
    ], CONFIG.fetch("zap").fetch("trash")
  end

  def test_homebrew_installs_required_tools
    assert_equal %w[gh git], CONFIG.fetch("dependencies").map { |dependency| dependency.fetch("formula") }.sort
  end
end
