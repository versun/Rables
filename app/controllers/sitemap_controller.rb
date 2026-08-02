class SitemapController < ApplicationController
  allow_unauthenticated_access

  def index
    respond_to do |format|
      format.xml do
        @articles = Article.published.order(created_at: :desc).select(:slug, :updated_at)
        @pages = Page.published.order(created_at: :desc).select(:slug, :updated_at)
        headers["Content-Type"] = "application/xml; charset=utf-8"
        render layout: false
      end
    end
  end
end
