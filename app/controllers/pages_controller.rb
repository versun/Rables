class PagesController < ApplicationController
  allow_unauthenticated_access only: %i[ show ]
  before_action :set_Page, only: %i[ show ]

  def show
    if @page.nil? || (!%w[publish shared].include?(@page.status) && !authenticated?)
      # 404 页面设置较短的缓存时间
      headers["Cache-Control"] = "public, max-age=300" # 5 分钟
      render file: Rails.public_path.join("404.html"), status: :not_found, layout: false
      nil
    else
      # 已发布的页面设置 1 天缓存
      if %w[publish shared].include?(@page.status)
        # Published page: browser cache for 1 day. Must stay private (no s-maxage)
        # because the page embeds a per-session CSRF token in the comment form;
        # a shared cache/CDN copy would make every visitor's comment submit fail with InvalidAuthenticityToken.
        headers["Cache-Control"] = "private, max-age=86400"
      else
        # 需要认证的页面：不缓存或短缓存
        headers["Cache-Control"] = "private, no-cache" if authenticated?
      end
    end
  end

  private

  def set_Page
    @page = Page.includes(comments: [ :replies ]).find_by(slug: params[:slug])
  end
end
