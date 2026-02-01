class CommentsController < ApplicationController
  include MathCaptchaVerification
  # Allow unauthenticated users to submit comments
  allow_unauthenticated_access only: [ :create ]
  before_action :set_commentable, only: [ :create ]

  def create
    commentable_ref = commentable_reference
    unless math_captcha_valid?(max: 10)
      ActivityLog.log!(
        action: :failed,
        target: :comment,
        level: :error,
        commentable: commentable_ref,
        reason: "captcha_invalid"
      )

      respond_to do |format|
        format.turbo_stream do
          @form_dom_id = comment_form_dom_id
          @form_parent_id = comment_parent_id
          @form_comment = build_comment_from_params
          @form_error_message = "验证失败：请回答数学题。"
          render :create, status: :unprocessable_entity
        end
        format.html do
          if request.xhr? || request.headers["X-Requested-With"] == "XMLHttpRequest"
            render json: { success: false, message: "验证失败：请回答数学题。" }, status: :unprocessable_entity
          else
            redirect_to_commentable alert: "验证失败：请回答数学题。"
          end
        end
        format.json { render json: { success: false, message: "验证失败：请回答数学题。" }, status: :unprocessable_entity }
      end
      return
    end

    @comment = @commentable.comments.build(comment_params)
    @comment.published_at = Time.current
    @comment.status = :pending # Require manual approval

    respond_to do |format|
      if @comment.save
        ActivityLog.log!(
          action: :created,
          target: :comment,
          level: :info,
          commentable: commentable_ref,
          author: @comment.author_name
        )
        format.turbo_stream do
          flash.now[:comment_submitted] = true
          @form_dom_id = comment_form_dom_id
          @form_parent_id = comment_parent_id
          flash.now[:comment_parent_id] = @form_parent_id
          @form_comment = Comment.new
          render :create
        end
        format.html do
          # For AJAX requests, return success response
          if request.xhr? || request.headers["X-Requested-With"] == "XMLHttpRequest"
            render json: { success: true, message: "评论已提交，等待审核后显示。" }, status: :created
          else
            # For regular form submissions, redirect
            flash[:comment_submitted] = true
            flash[:comment_parent_id] = comment_parent_id
            redirect_to_commentable
          end
        end
        format.json { render json: { success: true, message: "评论已提交，等待审核后显示。" }, status: :created }
      else
        ActivityLog.log!(
          action: :failed,
          target: :comment,
          level: :error,
          commentable: commentable_ref,
          errors: @comment.errors.full_messages.join(", ")
        )
        format.turbo_stream do
          @form_dom_id = comment_form_dom_id
          @form_parent_id = comment_parent_id
          @form_comment = @comment
          @form_error_message = "提交评论时出错：#{@comment.errors.full_messages.join('，')}"
          render :create, status: :unprocessable_entity
        end
        format.html do
          if request.xhr? || request.headers["X-Requested-With"] == "XMLHttpRequest"
            render json: { success: false, message: "提交评论时出错：#{@comment.errors.full_messages.join('，')}" }, status: :unprocessable_entity
          else
            redirect_to_commentable alert: "提交评论时出错：#{@comment.errors.full_messages.join('，')}"
          end
        end
        format.json { render json: { success: false, message: "提交评论时出错：#{@comment.errors.full_messages.join('，')}" }, status: :unprocessable_entity }
      end
    end
  rescue ActiveRecord::RecordNotFound => e
    respond_to do |format|
      format.turbo_stream do
        @form_dom_id = comment_form_dom_id
        @form_parent_id = comment_parent_id
        @form_comment = Comment.new
        @form_error_message = "文章或页面未找到。"
        render :create, status: :not_found
      end
      format.html do
        if request.xhr? || request.headers["X-Requested-With"] == "XMLHttpRequest"
          render json: { success: false, message: "文章或页面未找到。" }, status: :not_found
        else
          redirect_to_commentable alert: "Article or page not found."
        end
      end
      format.json { render json: { success: false, message: "文章或页面未找到。" }, status: :not_found }
    end
  rescue => e
    Rails.event.notify(
      "comments_controller.comment_creation_error",
      level: "error",
      component: "CommentsController",
      message: e.message,
      backtrace: e.backtrace
    )
    ActivityLog.log!(
      action: :failed,
      target: :comment,
      level: :error,
      commentable: commentable_reference,
      error: e.message
    )
    respond_to do |format|
      format.turbo_stream do
        @form_dom_id = comment_form_dom_id
        @form_parent_id = comment_parent_id
        @form_comment = Comment.new
        @form_error_message = "提交评论时发生错误，请稍后重试。"
        render :create, status: :internal_server_error
      end
      format.html do
        if request.xhr? || request.headers["X-Requested-With"] == "XMLHttpRequest"
          render json: { success: false, message: "提交评论时发生错误，请稍后重试。" }, status: :internal_server_error
        else
          redirect_to_commentable alert: "An error occurred while submitting your comment. Please try again later."
        end
      end
      format.json { render json: { success: false, message: "提交评论时发生错误，请稍后重试。" }, status: :internal_server_error }
    end
  end

  private

  def set_commentable
    if params[:article_id].present?
      @commentable = Article.find_by!(slug: params[:article_id])
      @article = @commentable # Keep @article for backward compatibility
    elsif params[:page_id].present?
      @commentable = Page.find_by!(slug: params[:page_id])
      @page = @commentable # Set @page for page views
    else
      raise ActiveRecord::RecordNotFound, "No article_id or page_id provided"
    end
  end

  def comment_params
    params.require(:comment).permit(:author_name, :author_email, :author_url, :content, :parent_id)
  end

  def determine_redirect_path
    referer = request.referer.to_s
    return root_path if referer.blank?

    site_url = ApplicationController.helpers.normalized_site_url
    return referer if site_url.present? && referer.start_with?(site_url)
    return referer if referer.start_with?(request.base_url)

    root_path
  end

  def redirect_to_commentable(options = {})
    target = determine_redirect_path
    if target == root_path
      redirect_to target, options
    else
      redirect_to target, options.merge(allow_other_host: true)
    end
  end

  def comment_parent_id
    params.dig(:comment, :parent_id).presence
  end

  def comment_form_dom_id
    parent_id = comment_parent_id
    parent_id.present? ? "comment-form-#{parent_id}" : "comment-form-container"
  end

  def build_comment_from_params
    return Comment.new unless params[:comment].present?

    Comment.new(comment_params)
  end

  def commentable_reference
    return nil unless @commentable

    identifier = @commentable.try(:slug).presence || @commentable.id
    "#{@commentable.class.name}##{identifier}"
  end
end
