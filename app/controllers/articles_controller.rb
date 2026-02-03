require "will_paginate/array"
class ArticlesController < ApplicationController
  allow_unauthenticated_access only: %i[ index show ] # %i 是一种字面量符号数组的简写方式，表示[:index]
  before_action :set_article, only: %i[ show edit update destroy ]

  # GET /
  def index
    respond_to do |format|
      format.html {
        @page = params[:page].present? ? params[:page].to_i : 1
        @per_page = 10

        @articles = base_article_scope
                    .order(created_at: :desc)
                    .paginate(page: @page, per_page: @per_page)
      }

      format.rss {
        @articles = base_article_scope.order(created_at: :desc)
        headers["Content-Type"] = "application/xml; charset=utf-8"
        render layout: false
      }
    end
  end

  # GET /1 or /1.json
  def show
    if @article.nil? || (!%w[publish shared].include?(@article.status) && !authenticated?)
      # 404 页面设置较短的缓存时间
      headers["Cache-Control"] = "public, max-age=300" # 5 分钟
      render file: Rails.public_path.join("404.html"), status: :not_found, layout: false
      nil
    else
      # 已发布的文章设置较长的缓存时间
      if %w[publish shared].include?(@article.status)
        # 公开文章：缓存 1 小时
        headers["Cache-Control"] = "public, max-age=3600, s-maxage=3600"
      else
        # 需要认证的文章：不缓存或短缓存
        headers["Cache-Control"] = "private, no-cache" if authenticated?
      end
    end
  end

  # GET /articles/new
  def new
    @article = Article.new
  end

  # GET /1/edit
  def edit
    render "admin/articles/edit"
  end

  # POST / or /articles.json
  def create
    @article = Article.new(article_params)
    respond_to do |format|
      if @article.save
        format.html { redirect_to admin_articles_path, notice: "Created successfully." }
        format.json { render :show, status: :created, location: @article }
      else
        format.html { render :new, status: :unprocessable_entity }
        format.json { render json: @article.errors, status: :unprocessable_entity }
      end
    end
  end

  # PATCH/PUT /1 or /1.json
  def update
    respond_to do |format|
      if @article.update(article_params)
        format.html { redirect_to admin_articles_path, notice: "Updated successfully." }
        format.json { render :show, status: :ok, location: @article }
      else
        format.html { render :edit, status: :unprocessable_entity }
        format.json { render json: @article.errors, status: :unprocessable_entity }
      end
    end
  end

  # DELETE /1 or /1.json
  def destroy
    notice_message = if @article.status != "trash"
      @article.update(status: "trash")
      "Article was successfully moved to trash."
    else
      @article.destroy!
      "Article was successfully destroyed."
    end

    respond_to do |format|
      format.html { redirect_to admin_articles_path, status: :see_other, notice: notice_message }
      format.json { head :no_content }
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

  def article_params
    params.expect(article: [ :title,
                            :content,
                            :status,
                            :slug,
                            :description,
                            :meta_title,
                            :meta_description,
                            :meta_image,
                            :scheduled_at,
                            :crosspost_mastodon,
                            :crosspost_twitter,
                            :crosspost_bluesky,
                            :crosspost_xiaohongshu,
                            :send_newsletter,
                            :created_at,
                            social_media_posts_attributes: [ [ :id, :_destroy, :platform, :url ] ] ])
  end
end
