module CommentsHelper
  EXTERNAL_COMMENT_PLATFORMS = %w[mastodon bluesky twitter].freeze

  # Builds the display list of top-level comments for a commentable:
  # approved local comments first, then external platform comments grouped by platform.
  # Filters preloaded comments in Ruby to avoid N+1 queries.
  def grouped_comment_items(commentable)
    preloaded_comments = commentable.comments.to_a

    items = preloaded_comments.select { |c| c.platform.nil? && c.approved? && c.parent_id.nil? }
                              .map { |c| { type: "local", data: c } }

    EXTERNAL_COMMENT_PLATFORMS.each do |platform|
      items += preloaded_comments.select { |c| c.platform == platform && c.parent_id.nil? }
                                 .map { |c| { type: platform, data: c } }
    end

    items
  end

  # Total number of visible comments (approved local + all external), including replies.
  def visible_comments_count(commentable)
    commentable.comments.to_a.count { |c| (c.platform.nil? && c.approved?) || c.platform.present? }
  end
end
