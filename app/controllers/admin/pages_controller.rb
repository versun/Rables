class Admin::PagesController < Admin::BaseController
  before_action :set_page, only: [ :show, :edit, :update, :destroy ]

  def index
    @scope = Page.all
    @pages = fetch_articles(@scope, sort_by: :page_order)
    @path = admin_pages_path
  end

  def show
  end

  def new
    @page = Page.new(comment: true)
  end

  def edit
  end

  def create
    @page = Page.new(page_params)

    respond_to do |format|
      if @page.save
        ActivityLog.log!(
          action: :created,
          target: :page,
          level: :info,
          title: @page.title,
          slug: @page.slug
        )
        refresh_pages
        format.html { redirect_to admin_pages_path, notice: "Page was successfully created." }
      else
        ActivityLog.log!(
          action: :failed,
          target: :page,
          level: :error,
          title: @page.title,
          slug: @page.slug,
          errors: @page.errors.full_messages.join(", ")
        )
        format.html { render :new, status: :unprocessable_entity }
        format.json { render json: @page.errors, status: :unprocessable_entity }
      end
    end
  end

  def update
    respond_to do |format|
      if @page.update(page_params)
        ActivityLog.log!(
          action: :updated,
          target: :page,
          level: :info,
          title: @page.title,
          slug: @page.slug
        )
        refresh_pages
        format.html { redirect_to admin_pages_path, notice: "Page was successfully updated." }
      else
        ActivityLog.log!(
          action: :failed,
          target: :page,
          level: :error,
          title: @page.title,
          slug: @page.slug,
          errors: @page.errors.full_messages.join(", ")
        )
        format.html { render :edit, status: :unprocessable_entity }
        format.json { render json: @page.errors, status: :unprocessable_entity }
      end
    end
  end

  def destroy
    page_title = @page.title
    @page.destroy!
    ActivityLog.log!(
      action: :deleted,
      target: :page,
      level: :info,
      title: page_title,
      slug: @page.slug
    )
    refresh_pages

    respond_to do |format|
      format.html { redirect_to admin_pages_path, status: :see_other, notice: "Page was successfully deleted." }
      format.json { head :no_content }
    end
  end

  private

  def after_batch_action
    refresh_pages
  end

  private

  def set_page
    @page = Page.find_by!(slug: params[:id])
  end

  def page_params
    params.require(:page).permit(:title, :content, :html_content, :content_type, :slug, :page_order, :meta_description, :redirect_url, :status, :comment, :scheduled_at)
  end
end
