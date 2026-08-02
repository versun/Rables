class TagsController < ApplicationController
  allow_unauthenticated_access only: %i[ index show ]

  def index
    @tags = Tag.alphabetical.all
    @published_article_counts = Article.published.joins(:article_tags).group("article_tags.tag_id").count
  end

  def show
    @tag = Tag.find_by!(slug: params[:slug])
    @articles = @tag.articles.published.includes(:rich_text_content, :tags).order(created_at: :desc).paginate(page: params[:page], per_page: 20)
    @newsletter_setting = NewsletterSetting.instance

    respond_to do |format|
      format.html
      format.rss {
        @articles = @tag.articles.published.includes(:rich_text_content, :tags).order(created_at: :desc)
        headers["Content-Type"] = "application/xml; charset=utf-8"
        render layout: false
      }
    end
  end
end
