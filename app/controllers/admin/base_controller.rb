class Admin::BaseController < ApplicationController
  # 统一的Admin基类控制器
  # 所有后台管理控制器都应该继承此类

  layout "admin"

  def batch_destroy
    process_batch_action(action: :destroy)
  end

  def batch_publish
    process_batch_action(action: :publish)
  end

  def batch_unpublish
    process_batch_action(action: :unpublish)
  end

  private

  def fetch_articles(scope, sort_by: :created_at)
    @page = params[:page].present? ? params[:page].to_i : 1
    @per_page = 100
    @status = params[:status] || "all"

    filtered_posts = filter_by_status(scope)
    filtered_posts = apply_model_includes(filtered_posts)
    posts = filtered_posts.paginate(page: @page, per_page: @per_page)
                          .order(sort_by => :desc)
    @comment_counts = load_comment_counts(scope.model, posts)
    posts
  end

  def apply_model_includes(scope)
    model_class = scope.model
    includes = []
    includes << :tags if model_class.reflect_on_association(:tags)
    includes << :social_media_posts if model_class.reflect_on_association(:social_media_posts)
    # Preload rich text so article list rows calling plain_text_content don't trigger N+1 queries
    includes << :rich_text_content if model_class == Article
    scope.includes(includes)
  end

  # List rows only display a comment count, so fetch grouped counts in one
  # query instead of preloading full comment records (including content)
  def load_comment_counts(model_class, posts)
    return {} unless model_class == Article
    Comment.where(commentable_type: model_class.name, commentable_id: posts.map(&:id)).group(:commentable_id).count
  end

  def filter_by_status(posts)
    case @status
    when "publish", "schedule", "shared", "draft", "trash"
      posts.by_status(@status.to_sym)
    else
      posts
    end
  end

  # Batch Operation Helpers
  def process_batch_action(action:)
    ids = params[:ids] || []
    count = 0

    ids.each do |id|
      record = find_record_for_batch(id)
      next unless record

      success = case action
      when :destroy
        perform_destroy(record)
      when :publish
        perform_publish(record)
      when :unpublish
        perform_unpublish(record)
      end

      count += 1 if success
    end

    after_batch_action

    action_past_tense = case action
    when :destroy then "deleted"
    when :publish then "published"
    when :unpublish then "unpublished"
    end

    redirect_to redirect_path_after_batch, notice: "Successfully #{action_past_tense} #{count} #{model_class.model_name.human.downcase}(s)."
  rescue => e
    redirect_to redirect_path_after_batch, alert: "Error processing #{action} for #{model_class.model_name.human.pluralize.downcase}: #{e.message}"
  end

  def find_record_for_batch(id)
    # Default to finding by slug, override in controller if needed (e.g. Tags use id)
    model_class.find_by(slug: id)
  end

  def perform_destroy(record)
    record.destroy!
    true
  end

  def perform_publish(record)
    record.update(status: :publish)
  end

  def perform_unpublish(record)
    record.update(status: :draft)
  end

  def after_batch_action
    # Hook for subclasses to implement logic after batch action (e.g. refresh cache)
  end

  def redirect_path_after_batch
    # Infer the index path from the controller name
    # e.g. Admin::ArticlesController -> admin_articles_path
    send("admin_#{controller_name}_path")
  end

  def model_class
    # Infer the model class from the controller name
    # e.g. Admin::ArticlesController -> Article
    controller_name.classify.constantize
  end
end
