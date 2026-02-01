class JekyllCommentsExporter
  require "digest/md5"
  require "fileutils"
  require "json"
  require "yaml"

  def initialize(setting: JekyllSetting.instance)
    @setting = setting
    @jekyll_path = Pathname.new(@setting.jekyll_path.to_s)
  end

  def export_all
    comments = base_scope.includes(:commentable).to_a
    grouped = comments.group_by(&:commentable)

    grouped.each do |commentable, comment_list|
      export_for_commentable(commentable, comment_list)
    end
  end

  def export_for_article(article)
    export_for_commentable(article, base_scope.where(commentable: article))
  end

  def export_for_page(page)
    export_for_commentable(page, base_scope.where(commentable: page))
  end

  def build_comment_tree(comments)
    by_parent = comments.group_by(&:parent_id)
    build_nodes(by_parent, nil)
  end

  def format_for_yaml(comments)
    comments.to_yaml(line_width: -1)
  end

  def format_for_json(comments)
    JSON.pretty_generate(comments)
  end

  private

  def base_scope
    scope = Comment.all
    scope = scope.where(status: :approved) unless @setting.include_pending_comments
    scope = scope.where(platform: nil) unless @setting.include_social_comments
    scope
  end

  def export_for_commentable(commentable, comments)
    return if commentable.nil?

    tree = build_comment_tree(Array(comments))
    payload = tree.map { |node| serialize_comment(node) }

    file_path = comments_file_path(commentable)
    FileUtils.mkdir_p(File.dirname(file_path))

    content = @setting.comments_format == "json" ? format_for_json(payload) : format_for_yaml(payload)
    File.write(file_path, content)
  end

  def comments_file_path(commentable)
    slug = commentable.try(:slug).presence || commentable.id
    extension = @setting.comments_format == "json" ? "json" : "yml"
    @jekyll_path.join("_data", "comments", "#{slug}.#{extension}")
  end

  def build_nodes(by_parent, parent_id)
    Array(by_parent[parent_id]).map do |comment|
      {
        comment: comment,
        replies: build_nodes(by_parent, comment.id)
      }
    end
  end

  def serialize_comment(node)
    comment = node[:comment]
    data = {
      id: comment.id,
      type: comment.platform.presence || "local",
      author: author_payload(comment),
      content: comment.content.to_s,
      date: comment.published_at&.iso8601 || comment.created_at&.iso8601
    }

    data[:url] = comment.url if comment.url.present?
    data[:platform] = comment.platform if comment.platform.present?
    data[:replies] = node[:replies].map { |child| serialize_comment(child) } if node[:replies].any?
    data
  end

  def author_payload(comment)
    payload = { name: comment.author_name.to_s }
    if comment.author_email.present?
      payload[:email_hash] = Digest::MD5.hexdigest(comment.author_email.downcase)
    end
    payload[:url] = comment.author_url if comment.author_url.present?
    payload[:username] = comment.author_username if comment.author_username.present?
    payload[:avatar] = comment.author_avatar_url if comment.author_avatar_url.present?
    payload
  end
end
