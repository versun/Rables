class RedirectMiddleware
  def initialize(app)
    @app = app
  end

  def call(env)
    request = ActionDispatch::Request.new(env)
    path = request.path

    # Skip redirects for admin pages, assets, and API endpoints
    return @app.call(env) if path.start_with?("/admin", "/assets", "/rails", "/up")

    # Only redirect GET/HEAD requests; redirecting POST/PUT/etc. would drop
    # the request body (e.g. form submissions).
    return @app.call(env) unless request.get? || request.head?

    # Check all redirects
    redirect = find_matching_redirect(path)
    if redirect
      target_url = redirect.apply_to(path)
      if target_url
        Rails.event.notify "middleware.redirect.applied",
          level: "info",
          component: "redirect_middleware",
          from_path: path,
          to_url: target_url,
          status_code: redirect.permanent? ? 301 : 302,
          permanent: redirect.permanent?
        status = redirect.permanent? ? 301 : 302
        return [ status, { "Location" => target_url, "Content-Type" => "text/html" }, [] ]
      end
    end

    @app.call(env)
  end

  private

  def find_matching_redirect(path)
    # Cache enabled redirects to avoid hitting database on every request
    enabled_redirects = Rails.cache.fetch("redirect_middleware/enabled_redirects", expires_in: 5.minutes) do
      Redirect.all.to_a.select(&:enabled?)
    end

    enabled_redirects.find { |redirect| redirect.match?(path) }
  end
end
