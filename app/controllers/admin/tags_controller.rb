class Admin::TagsController < Admin::BaseController
  before_action :set_tag, only: [ :edit, :update, :destroy ]

  def index
    @tags = Tag.alphabetical
      .left_joins(:articles)
      .select("tags.*, COUNT(articles.id) AS articles_total")
      .group("tags.id")
  end

  def new
    @tag = Tag.new
  end

  def create
    @tag = Tag.new(tag_params)

    if @tag.save
      ActivityLog.log!(
        action: :created,
        target: :tag,
        level: :info,
        name: @tag.name,
        slug: @tag.slug
      )
      redirect_to admin_tags_path, notice: "Tag was successfully created."
    else
      ActivityLog.log!(
        action: :failed,
        target: :tag,
        level: :error,
        name: @tag.name,
        errors: @tag.errors.full_messages.join(", ")
      )
      render :new, status: :unprocessable_entity
    end
  end

  def edit
  end

  def update
    if @tag.update(tag_params)
      ActivityLog.log!(
        action: :updated,
        target: :tag,
        level: :info,
        name: @tag.name,
        slug: @tag.slug
      )
      redirect_to admin_tags_path, notice: "Tag was successfully updated."
    else
      ActivityLog.log!(
        action: :failed,
        target: :tag,
        level: :error,
        name: @tag.name,
        errors: @tag.errors.full_messages.join(", ")
      )
      render :edit, status: :unprocessable_entity
    end
  end

  def destroy
    tag_name = @tag.name
    # article_tags join rows are cleaned up by has_many :article_tags, dependent: :destroy
    @tag.destroy
    ActivityLog.log!(
      action: :deleted,
      target: :tag,
      level: :info,
      name: tag_name,
      slug: @tag.slug
    )
    redirect_to admin_tags_path, status: :see_other, notice: "Tag was successfully deleted."
  end

  private

  def find_record_for_batch(id)
    Tag.find_by(id: id)
  end

  private

  def set_tag
    @tag = Tag.find(params[:id])
  end

  def tag_params
    params.require(:tag).permit(:name)
  end
end
