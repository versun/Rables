class SubscriptionsController < ApplicationController
  include CacheableSettings
  include MathCaptchaVerification
  # Allow unauthenticated users to subscribe/confirm/unsubscribe from public pages
  allow_unauthenticated_access only: [ :index, :create, :confirm, :unsubscribe ]

  def index
    @subscriber = Subscriber.new
    render :index
  end

  def create
    email = params.dig(:subscription, :email) || params[:email]

    if email.blank?
      respond_to do |format|
        format.html { redirect_to root_path, alert: "请输入有效的邮箱地址。" }
        format.json { render json: { success: false, message: "请输入有效的邮箱地址。" }, status: :unprocessable_entity }
      end
      return
    end

    unless math_captcha_valid?(max: 10)
      respond_to do |format|
        format.html { redirect_to root_path, alert: "验证失败：请回答数学题。" }
        format.json { render json: { success: false, message: "验证失败：请回答数学题。" }, status: :unprocessable_entity }
      end
      return
    end

    @subscriber = Subscriber.find_or_initialize_by(email: email)

    if already_subscribed?(@subscriber)
      respond_to do |format|
        format.html do
          flash[:notice] = "您已经订阅了我们的邮件列表。"
          redirect_to root_path
        end
        format.json { render json: { success: true, message: "您已经订阅了我们的邮件列表。" } }
      end
      return
    end

    if @subscriber.persisted? && @subscriber.unsubscribed?
      @subscriber.confirmed_at = nil
      @subscriber.unsubscribed_at = nil
      @subscriber.confirmation_token = SecureRandom.urlsafe_base64(32)
    end

    # 处理订阅的tags
    tag_ids = params.dig(:subscription, :tag_ids) || []
    tag_ids = tag_ids.reject(&:blank?) if tag_ids.is_a?(Array)

    if @subscriber.save
      # 保存订阅的tags
      if tag_ids.present?
        tags = Tag.where(id: tag_ids)
        @subscriber.tags = tags
      else
        # 如果没有选择tag，则订阅所有内容（tags为空）
        @subscriber.tags = []
      end

      NewsletterConfirmationJob.perform_later(@subscriber.id)

      ActivityLog.log!(
        action: :created,
        target: :subscription,
        level: :info,
        email: email,
        tags: tag_ids
      )

      respond_to do |format|
        format.html do
          flash[:notice] = "订阅成功！请检查您的邮箱并点击确认链接。"
          redirect_to root_path
        end
        format.json { render json: { success: true, message: "订阅成功！请检查您的邮箱并点击确认链接。" } }
      end
    else
      ActivityLog.log!(
        action: :failed,
        target: :subscription,
        level: :error,
        email: email,
        errors: @subscriber.errors.full_messages.join(", ")
      )
      respond_to do |format|
        format.html do
          flash[:alert] = @subscriber.errors.full_messages.join(", ")
          redirect_to root_path
        end
        format.json { render json: { success: false, message: @subscriber.errors.full_messages.join(", ") }, status: :unprocessable_entity }
      end
    end
  end

  def confirm
    @subscriber = Subscriber.find_by(confirmation_token: params[:token])
    @success = false

    if @subscriber
      if @subscriber.confirmed?
        @success = true
        @message = "您的邮箱已经确认过了。"
      else
        @subscriber.confirm!
        ActivityLog.log!(
          action: :confirmed,
          target: :subscription,
          level: :info,
          email: @subscriber.email
        )
        @success = true
        @message = "订阅确认成功！"
      end
    end

    render :confirm
  end

  def unsubscribe
    @subscriber = Subscriber.find_by(unsubscribe_token: params[:token])
    @success = false

    if @subscriber
      email = @subscriber.email
      @subscriber.unsubscribe!
      ActivityLog.log!(
        action: :unsubscribed,
        target: :subscription,
        level: :info,
        email: email
      )
      @success = true
    end

    render :unsubscribe
  end

  private

  def already_subscribed?(subscriber)
    subscriber.persisted? && subscriber.confirmed? && !subscriber.unsubscribed?
  end
end
