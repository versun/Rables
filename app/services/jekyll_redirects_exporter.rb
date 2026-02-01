class JekyllRedirectsExporter
  attr_reader :setting

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
  end

  # Export redirects to various formats
  def export
    case setting.redirect_export_format
    when "netlify"
      export_to_netlify
    when "vercel"
      export_to_vercel
    when "htaccess"
      export_to_htaccess
    when "nginx"
      export_to_nginx
    when "jekyll-plugin"
      export_to_jekyll_plugin
    else
      export_to_jekyll_plugin
    end
  end

  # Write redirects to Jekyll directory
  def write_to_jekyll
    return unless setting.jekyll_path_valid?

    content = export
    return if content.blank?

    case setting.redirect_export_format
    when "netlify"
      write_file("_redirects", content)
    when "vercel"
      write_file("vercel.json", content)
    when "htaccess"
      write_file(".htaccess", content)
    when "nginx"
      write_file("nginx-redirects.conf", content)
    end
  end

  # Netlify _redirects format
  def export_to_netlify
    redirects = Redirect.enabled.map do |redirect|
      status_code = redirect.permanent? ? "301" : "302"
      "#{redirect.regex} #{redirect.replacement} #{status_code}"
    end

    redirects.join("\n")
  end

  # Vercel vercel.json format
  def export_to_vercel
    redirects_array = Redirect.enabled.map do |redirect|
      status_code = redirect.permanent? ? 308 : 307
      {
        "source" => redirect.regex,
        "destination" => redirect.replacement,
        "permanent" => redirect.permanent?
      }
    end

    JSON.pretty_generate({ "redirects" => redirects_array })
  end

  # Apache .htaccess format
  def export_to_htaccess
    lines = [ "RewriteEngine On" ]

    Redirect.enabled.each do |redirect|
      flag = redirect.permanent? ? "R=301,L" : "R=302,L"
      lines << "RewriteRule #{redirect.regex} #{redirect.replacement} [#{flag}]"
    end

    lines.join("\n")
  end

  # Nginx config format
  def export_to_nginx
    Redirect.enabled.map do |redirect|
      status_code = redirect.permanent? ? "301" : "302"
      "rewrite #{redirect.regex} #{redirect.replacement} #{status_code};"
    end.join("\n")
  end

  # Jekyll redirect_from plugin format (returns a hash for front matter)
  def export_to_jekyll_plugin
    # This format is used in front matter, not a separate file
    # Returns a hash that can be merged into article/page front matter
    {}
  end

  # Get redirect_from data for a specific article/page
  def redirects_for(item)
    return {} unless setting.redirect_export_format == "jekyll-plugin"

    # Find redirects that match this item's slug
    slug_pattern = "/#{item.slug}"
    matching_redirects = Redirect.enabled.select do |redirect|
      redirect.replacement.include?(slug_pattern)
    end

    return {} if matching_redirects.empty?

    { "redirect_from" => matching_redirects.map(&:regex) }
  end

  private

  def write_file(filename, content)
    file_path = File.join(setting.jekyll_path, filename)
    File.write(file_path, content)
  end
end
