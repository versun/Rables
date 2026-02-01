# frozen_string_literal: true

require "digest"

class JekyllCommentsExporter
  attr_reader :setting, :stats

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
    @stats = { articles: 0, comments: 0 }
  end

  def export_all
    return unless @setting.jekyll_path_valid?
    return unless @setting.export_comments?

    FileUtils.mkdir_p(@setting.comments_data_path)

    Article.where(status: :publish).find_each do |article|
      export_for_article(article)
      @stats[:articles] += 1
    end

    @stats
  end

  def export_for_article(article)
    return unless @setting.jekyll_path_valid?
    return unless @setting.export_comments?

    comments = fetch_comments_for(article)
    return if comments.empty?

    tree = build_comment_tree(comments)

    filename = "#{article.slug}.#{@setting.comments_format}"
    filepath = File.join(@setting.comments_data_path, filename)

    # Ensure the directory exists
    FileUtils.mkdir_p(File.dirname(filepath))

    content = @setting.comments_format == "json" ? format_as_json(tree) : format_as_yaml(tree)
    File.write(filepath, content)

    @stats[:comments] += comments.count
  end

  private

  def fetch_comments_for(article)
    scope = article.comments

    # Filter by approval status
    scope = scope.where(status: :approved) unless @setting.include_pending_comments?

    # Filter social comments - local comments have nil platform
    unless @setting.include_social_comments?
      scope = scope.where(platform: nil)
    end

    scope.reorder(:created_at)
  end

  def build_comment_tree(comments)
    # Separate root comments and replies
    root_comments = comments.select { |c| c.parent_id.nil? }
    replies_by_parent = comments.select { |c| c.parent_id.present? }.group_by(&:parent_id)

    root_comments.map { |comment| build_comment_node(comment, replies_by_parent) }
  end

  def build_comment_node(comment, replies_by_parent)
    node = format_comment(comment)

    replies = replies_by_parent[comment.id]
    if replies.present?
      node["replies"] = replies.map { |reply| build_comment_node(reply, replies_by_parent) }
    end

    node
  end

  def format_comment(comment)
    # Determine comment type based on platform
    comment_type = comment.platform.present? ? comment.platform : "local"

    data = {
      "id" => comment.id,
      "type" => comment_type,
      "author" => format_author(comment),
      "content" => comment.content,
      "date" => comment.published_at&.iso8601 || comment.created_at.iso8601
    }

    # Add social media specific fields
    if comment.platform.present?
      data["platform"] = comment.platform
      data["url"] = comment.url if comment.url.present?
    end

    data
  end

  def format_author(comment)
    author = {
      "name" => comment.author_name
    }

    # Add email hash for Gravatar (don't expose actual email)
    if comment.author_email.present?
      author["email_hash"] = Digest::MD5.hexdigest(comment.author_email.downcase.strip)
    end

    author["url"] = comment.author_url if comment.author_url.present?

    # Social media author info
    if comment.platform.present?
      author["username"] = comment.author_username if comment.author_username.present?
      author["avatar"] = comment.author_avatar_url if comment.author_avatar_url.present?
    end

    author
  end

  def format_as_yaml(tree)
    tree.to_yaml
  end

  def format_as_json(tree)
    JSON.pretty_generate(tree)
  end
end
