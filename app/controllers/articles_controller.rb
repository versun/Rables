class ArticlesController < ApplicationController
  allow_unauthenticated_access only: %i[ index show ] # %i 是一种字面量符号数组的简写方式，表示[:index]
  before_action :set_article, only: %i[ show ]

  # GET /
  def index
    respond_to do |format|
      format.html {
        @page = params[:page].present? ? params[:page].to_i : 1
        @per_page = 10

        @articles = base_article_scope
                    .order(created_at: :desc)
                    .paginate(page: @page, per_page: @per_page)

        # Set cache headers for the home page
        # Cache for 5 minutes, allow CDN/shared cache for 15 minutes
        headers["Cache-Control"] = "public, max-age=300, s-maxage=900"

        # Add fresh_when support based on the most recent article
        if @articles.any?
          fresh_when(last_modified: @articles.maximum(:updated_at), etag: @articles.map(&:cache_key_with_version).join)
        end
      }

      format.rss {
        @articles = base_article_scope.order(created_at: :desc).limit(50)
        headers["Content-Type"] = "application/xml; charset=utf-8"
        # RSS feed can be cached longer since content updates less frequently
        headers["Cache-Control"] = "public, max-age=600, s-maxage=1800"
        render layout: false
      }
    end
  end

  def show
    if @article.nil? || (!%w[publish shared].include?(@article.status) && !authenticated?)
      # 404 页面设置较短的缓存时间
      headers["Cache-Control"] = "public, max-age=300" # 5 分钟
      render file: Rails.public_path.join("404.html"), status: :not_found, layout: false
      nil
    else
      # 已发布的文章设置较长的缓存时间
      if %w[publish shared].include?(@article.status)
        # Published article: browser cache for 1 hour. Must stay private (no s-maxage)
        # because the page embeds a per-session CSRF token in the comment form;
        # a shared cache/CDN copy would make every visitor's comment submit fail with InvalidAuthenticityToken.
        headers["Cache-Control"] = "private, max-age=3600"
      else
        # 需要认证的文章：不缓存或短缓存
        headers["Cache-Control"] = "private, no-cache" if authenticated?
      end
    end
  end

  private

  def base_article_scope
    scope = params[:q].present? ? Article.search_content(params[:q]) : Article.all
    scope = scope.published.includes(:tags)

    if scope.respond_to?(:with_rich_text_content_and_embeds)
      scope.with_rich_text_content_and_embeds
    elsif scope.respond_to?(:with_rich_text_content)
      scope.with_rich_text_content.preload(rich_text_content: { embeds_attachments: :blob })
    else
      scope.preload(rich_text_content: { embeds_attachments: :blob })
    end
  end

  def set_article
    @article = Article.includes(
      :tags,
      :social_media_posts,
      comments: [ :replies ],
      rich_text_content: { embeds_attachments: :blob }
    ).find_by(slug: params[:slug])
  end
end
