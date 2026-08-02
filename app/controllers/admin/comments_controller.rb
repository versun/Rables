class Admin::CommentsController < Admin::BaseController
  before_action :set_comment, only: [ :show, :edit, :update, :destroy, :approve, :reject, :reply ]

  def index
    @comments = Comment.includes(:commentable, :article, parent: :commentable)

    # Filter by status
    # Filter by status
    if params[:status].present? && Comment.statuses.key?(params[:status])
      @comments = @comments.where(status: params[:status])
    end

    @comments = @comments.reorder(Arel.sql("COALESCE(published_at, created_at) DESC"))
                         .paginate(page: params[:page], per_page: 30)
  end

  def show
  end

  def edit
  end

  def update
    new_status = comment_params[:status]
    if new_status.present? && !Comment.statuses.key?(new_status)
      flash.now[:alert] = "Invalid status."
      render :edit, status: :unprocessable_entity
      return
    end

    if @comment.update(comment_params)
      ActivityLog.log!(
        action: :updated,
        target: :comment,
        level: :info,
        id: @comment.id,
        commentable: "#{@comment.commentable_type}##{@comment.commentable_id}"
      )
      redirect_to admin_comments_path, notice: "Comment updated successfully."
    else
      ActivityLog.log!(
        action: :failed,
        target: :comment,
        level: :error,
        id: @comment.id,
        commentable: "#{@comment.commentable_type}##{@comment.commentable_id}",
        errors: @comment.errors.full_messages.join(", ")
      )
      render :edit, status: :unprocessable_entity
    end
  end

  def destroy
    commentable_info = "#{@comment.commentable_type}##{@comment.commentable_id}"
    if @comment.destroy
      ActivityLog.log!(
        action: :deleted,
        target: :comment,
        level: :info,
        id: @comment.id,
        commentable: commentable_info
      )
      redirect_to admin_comments_path, notice: "Comment deleted successfully."
    else
      ActivityLog.log!(
        action: :failed,
        target: :comment,
        level: :error,
        id: @comment.id,
        commentable: commentable_info
      )
      redirect_to admin_comments_path, alert: "Failed to delete comment."
    end
  end

  def approve
    if @comment.approved!
      ActivityLog.log!(
        action: :approved,
        target: :comment,
        level: :info,
        id: @comment.id,
        commentable: "#{@comment.commentable_type}##{@comment.commentable_id}"
      )
      redirect_to admin_comments_path, notice: "Comment approved successfully."
    else
      ActivityLog.log!(
        action: :failed,
        target: :comment,
        level: :error,
        id: @comment.id,
        commentable: "#{@comment.commentable_type}##{@comment.commentable_id}"
      )
      redirect_to admin_comments_path, alert: "Failed to approve comment."
    end
  end

  def reject
    if @comment.rejected!
      ActivityLog.log!(
        action: :rejected,
        target: :comment,
        level: :info,
        id: @comment.id,
        commentable: "#{@comment.commentable_type}##{@comment.commentable_id}"
      )
      redirect_to admin_comments_path, notice: "Comment rejected."
    else
      ActivityLog.log!(
        action: :failed,
        target: :comment,
        level: :error,
        id: @comment.id,
        commentable: "#{@comment.commentable_type}##{@comment.commentable_id}"
      )
      redirect_to admin_comments_path, alert: "Failed to reject comment."
    end
  end

  def reply
    if @comment.platform.present?
      redirect_to admin_comments_path, alert: "Cannot reply to external comments."
      return
    end

    if @comment.rejected?
      redirect_to admin_comments_path, alert: "Cannot reply to rejected comments."
      return
    end

    commentable = @comment.commentable || @comment.display_commentable
    unless commentable
      redirect_to admin_comments_path, alert: "Commentable not found."
      return
    end

    author_name = reply_author_name
    if author_name.blank?
      redirect_to admin_comments_path, alert: "Please set the site author name in Settings before replying."
      return
    end

    reply_comment = commentable.comments.build(
      parent: @comment,
      author_name: author_name,
      author_url: reply_author_url,
      content: reply_params[:content],
      status: :approved,
      published_at: Time.current
    )

    if reply_comment.save
      ActivityLog.log!(
        action: :replied,
        target: :comment,
        level: :info,
        commentable: "#{commentable.class.name}##{commentable.id}",
        author: author_name
      )
      redirect_to admin_comments_path, notice: "Reply posted successfully."
    else
      ActivityLog.log!(
        action: :failed,
        target: :comment,
        level: :error,
        commentable: "#{commentable.class.name}##{commentable.id}",
        errors: reply_comment.errors.full_messages.join(", ")
      )
      redirect_to admin_comments_path, alert: "Failed to reply: #{reply_comment.errors.full_messages.join(', ')}"
    end
  end

  def batch_destroy
    ids = params[:ids] || []
    count = 0

    ids.each do |id|
      comment = Comment.find_by(id: id)
      if comment
        comment.destroy
        count += 1
      end
    end

    ActivityLog.log!(
      action: :deleted,
      target: :comment,
      level: :info,
      count: count
    )
    redirect_to admin_comments_path, notice: "Successfully deleted #{count} comment(s)."
  rescue => e
    ActivityLog.log!(
      action: :failed,
      target: :comment,
      level: :error,
      error: e.message
    )
    redirect_to admin_comments_path, alert: "Error deleting comments: #{e.message}"
  end

  def batch_approve
    ids = params[:ids] || []
    count = 0

    ids.each do |id|
      comment = Comment.find_by(id: id)
      if comment && comment.approved!
        count += 1
      end
    end

    ActivityLog.log!(
      action: :approved,
      target: :comment,
      level: :info,
      count: count
    )
    redirect_to admin_comments_path, notice: "Successfully approved #{count} comment(s)."
  rescue => e
    ActivityLog.log!(
      action: :failed,
      target: :comment,
      level: :error,
      error: e.message
    )
    redirect_to admin_comments_path, alert: "Error approving comments: #{e.message}"
  end

  def batch_reject
    ids = params[:ids] || []
    count = 0

    ids.each do |id|
      comment = Comment.find_by(id: id)
      if comment && comment.rejected!
        count += 1
      end
    end

    ActivityLog.log!(
      action: :rejected,
      target: :comment,
      level: :info,
      count: count
    )
    redirect_to admin_comments_path, notice: "Successfully rejected #{count} comment(s)."
  rescue => e
    ActivityLog.log!(
      action: :failed,
      target: :comment,
      level: :error,
      error: e.message
    )
    redirect_to admin_comments_path, alert: "Error rejecting comments: #{e.message}"
  end

  private

  def set_comment
    @comment = Comment.find(params[:id])
  end

  def comment_params
    params.require(:comment).permit(:author_name, :author_email, :author_url, :content, :status)
  end

  def reply_params
    params.require(:comment).permit(:content)
  end

  def reply_author_name
    CacheableSettings.site_info[:author].to_s.strip
  end

  def reply_author_url
    raw_url = CacheableSettings.site_info[:url].to_s.strip
    return nil if raw_url.blank?

    normalized_url = raw_url.chomp("/")
    normalized_url = "https://#{normalized_url}" unless normalized_url.match?(%r{^https?://})
    normalized_url
  end
end
