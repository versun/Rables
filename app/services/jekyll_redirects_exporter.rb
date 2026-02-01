# frozen_string_literal: true

class JekyllRedirectsExporter
  attr_reader :setting, :redirects, :stats

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
    @redirects = Redirect.all
    @stats = { exported: 0, errors: 0 }
  end

  # Export all redirects to file and track stats
  def export_all
    return false unless @setting.jekyll_path_valid?

    filepath = export_to_file
    @stats[:exported] = @redirects.count if filepath

    true
  rescue => e
    @stats[:errors] += 1
    Rails.event.notify("jekyll_redirects_exporter.export_failed",
      component: "JekyllRedirectsExporter",
      error: e.message,
      level: "error")
    false
  end

  def export
    case @setting.redirect_export_format
    when "netlify"
      export_netlify
    when "vercel"
      export_vercel
    when "htaccess"
      export_htaccess
    when "nginx"
      export_nginx
    when "jekyll-plugin"
      export_jekyll_plugin
    else
      export_netlify
    end
  end

  def export_to_file
    return unless @setting.jekyll_path_valid?

    content = export
    filepath = output_filepath

    FileUtils.mkdir_p(File.dirname(filepath))
    File.write(filepath, content)

    filepath
  end

  private

  def output_filepath
    case @setting.redirect_export_format
    when "netlify"
      File.join(@setting.jekyll_path, "_redirects")
    when "vercel"
      File.join(@setting.jekyll_path, "vercel.json")
    when "htaccess"
      File.join(@setting.jekyll_path, ".htaccess")
    when "nginx"
      File.join(@setting.jekyll_path, "nginx_redirects.conf")
    when "jekyll-plugin"
      File.join(@setting.jekyll_path, "_data", "redirects.yml")
    else
      File.join(@setting.jekyll_path, "_redirects")
    end
  end

  def export_netlify
    lines = @redirects.map do |redirect|
      status = redirect.permanent? ? "301" : "302"
      "#{redirect.regex}  #{redirect.replacement}  #{status}"
    end

    lines.join("\n")
  end

  def export_vercel
    redirects_array = @redirects.map do |redirect|
      {
        source: redirect.regex,
        destination: redirect.replacement,
        permanent: redirect.permanent?
      }
    end

    JSON.pretty_generate({ redirects: redirects_array })
  end

  def export_htaccess
    lines = [ "RewriteEngine On", "" ]

    @redirects.each do |redirect|
      flag = redirect.permanent? ? "[R=301,L]" : "[R=302,L]"
      # The regex field already contains a regex pattern
      source = redirect.regex
      target = redirect.replacement
      lines << "RewriteRule ^#{source.sub(%r{^/}, '')}$ #{target} #{flag}"
    end

    lines.join("\n")
  end

  def export_nginx
    lines = []

    @redirects.each do |redirect|
      return_code = redirect.permanent? ? "301" : "302"
      lines << "location ~ #{redirect.regex} {"
      lines << "    return #{return_code} #{redirect.replacement};"
      lines << "}"
      lines << ""
    end

    lines.join("\n")
  end

  def export_jekyll_plugin
    # For jekyll-redirect-from plugin format
    redirects_array = @redirects.map do |redirect|
      {
        "from" => redirect.regex,
        "to" => redirect.replacement
      }
    end

    redirects_array.to_yaml
  end
end
