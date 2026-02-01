class GitOperationsService
  require "open3"

  def initialize(repository_path)
    @repository_path = repository_path
  end

  def clone_repository(repo_url, path)
    run_command([ "git", "clone", repo_url, path ])
  end

  def pull_latest(branch: nil)
    command = [ "git", "-C", @repository_path, "pull", "--ff-only" ]
    command.concat([ "origin", branch ]) if branch.present?
    run_command(command)
  end

  def commit_and_push(message, branch: nil)
    run_command([ "git", "-C", @repository_path, "add", "-A" ])
    status = run_command([ "git", "-C", @repository_path, "status", "--porcelain" ])
    return nil if status.strip.empty?

    run_command([ "git", "-C", @repository_path, "commit", "-m", message ])
    push(branch: branch)
    run_command([ "git", "-C", @repository_path, "rev-parse", "HEAD" ]).strip
  end

  def push(branch: nil)
    command = [ "git", "-C", @repository_path, "push" ]
    command.concat([ "origin", branch ]) if branch.present?
    run_command(command)
  end

  def current_remote_url(remote = "origin")
    run_command([ "git", "-C", @repository_path, "remote", "get-url", remote ]).strip
  rescue
    nil
  end

  def set_remote_url(remote, url)
    run_command([ "git", "-C", @repository_path, "remote", "set-url", remote, url ])
  end

  private

  def run_command(command)
    stdout, stderr, status = Open3.capture3(*command)
    return stdout if status.success?

    raise "Git command failed: #{stderr.presence || stdout}"
  end
end
