class JekyllCommentsExporter
  attr_reader :setting

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
  end

  # Export all comments
  def export_all
    return {} unless setting.export_comments?

    comments_by_post = {}

    # Get all commentable items with approved comments
    Article.where(id: approved_comments.select(:commentable_id).where(commentable_type: "Article")).find_each do |article|
      comments_by_post[article.slug] = export_for_article(article)
    end

    Page.where(id: approved_comments.select(:commentable_id).where(commentable_type: "Page")).find_each do |page|
      comments_by_post[page.slug] = export_for_page(page)
    end

    comments_by_post
  end

  # Export comments for a specific article
  def export_for_article(article)
    return [] unless setting.export_comments?

    comments = fetch_comments(article)
    build_comment_tree(comments)
  end

  # Export comments for a specific page
  def export_for_page(page)
    return [] unless setting.export_comments?

    comments = fetch_comments(page)
    build_comment_tree(comments)
  end

  # Format comments for YAML export
  def format_for_yaml(comments)
    comments.map { |c| format_comment(c) }
  end

  # Format comments for JSON export
  def format_for_json(comments)
    comments.map { |c| format_comment(c) }
  end

  # Write comments to Jekyll data directory
  def write_to_jekyll
    return unless setting.jekyll_path_valid?
    return unless setting.export_comments?

    comments_data = export_all
    comments_dir = File.join(setting.jekyll_path, "_data", "comments")
    FileUtils.mkdir_p(comments_dir)

    comments_data.each do |slug, comments|
      next if comments.empty?

      file_path = File.join(comments_dir, "#{slug}.#{setting.comments_format}")
      content = format_content(comments)
      File.write(file_path, content)
    end
  end

  private

  def approved_comments
    base_query = Comment.approved
    base_query = base_query.where.not(platform: %w[mastodon bluesky twitter]) unless setting.include_social_comments?
    base_query
  end

  def fetch_comments(commentable)
    comments = commentable.comments.where(id: approved_comments.select(:id))

    # Filter by platform if needed
    unless setting.include_social_comments?
      comments = comments.where(platform: nil)
    end

    comments.order(:created_at)
  end

  def build_comment_tree(comments)
    return [] if comments.empty?

    # Group comments by parent
    comment_map = {}
    root_comments = []

    comments.each do |comment|
      formatted = format_comment(comment)
      formatted["replies"] = []
      comment_map[comment.id] = formatted
    end

    comments.each do |comment|
      if comment.parent_id && comment_map[comment.parent_id]
        comment_map[comment.parent_id]["replies"] << comment_map[comment.id]
      else
        root_comments << comment_map[comment.id]
      end
    end

    root_comments
  end

  def format_comment(comment)
    data = {
      "id" => comment.id,
      "type" => comment.platform || "local",
      "author" => {
        "name" => comment.author_name
      },
      "content" => comment.content.to_s,
      "date" => comment.published_at&.iso8601 || comment.created_at.iso8601
    }

    # Add email hash for Gravatar (local comments only)
    if comment.platform.nil? && comment.author_email.present?
      data["author"]["email_hash"] = Digest::MD5.hexdigest(comment.author_email.downcase.strip)
    end

    # Add author URL if present
    if comment.author_url.present?
      data["author"]["url"] = comment.author_url
    end

    # Add avatar for social comments
    if comment.platform.present? && comment.author_avatar_url.present?
      data["author"]["avatar"] = comment.author_avatar_url
    end

    # Add platform-specific data
    if comment.platform.present?
      data["platform"] = comment.platform
      data["username"] = comment.author_username if comment.author_username.present?
      data["url"] = comment.platform_url if comment.platform_url.present?
    end

    data
  end

  def format_content(comments)
    case setting.comments_format
    when "json"
      JSON.pretty_generate(format_for_json(comments))
    else # yaml
      format_for_yaml(comments).to_yaml(line_width: -1)
    end
  end
end
