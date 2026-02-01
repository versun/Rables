class JekyllRedirectsExporter
  require "json"
  require "yaml"

  def initialize(setting: JekyllSetting.instance)
    @setting = setting
    @jekyll_path = Pathname.new(@setting.jekyll_path.to_s)
  end

  def export_to_netlify(path = nil)
    content = redirects.map { |r| "#{r.regex} #{r.replacement} #{status_code(r)}" }.join("\n")
    write_file(path || @jekyll_path.join("_redirects"), content)
  end

  def export_to_vercel(path = nil)
    body = {
      redirects: redirects.map do |r|
        { source: r.regex, destination: r.replacement, permanent: r.permanent? }
      end
    }
    write_file(path || @jekyll_path.join("vercel.json"), JSON.pretty_generate(body))
  end

  def export_to_htaccess(path = nil)
    content = redirects.map do |r|
      "RedirectMatch #{status_code(r)} #{r.regex} #{r.replacement}"
    end.join("\n")
    write_file(path || @jekyll_path.join(".htaccess"), content)
  end

  def export_to_nginx(path = nil)
    content = redirects.map do |r|
      code = r.permanent? ? "permanent" : "redirect"
      "rewrite #{r.regex} #{r.replacement} #{code};"
    end.join("\n")
    write_file(path || @jekyll_path.join("redirects.conf"), content)
  end

  def export_to_jekyll_plugin(path = nil)
    payload = redirects.map { |r| { "from" => r.regex, "to" => r.replacement, "permanent" => r.permanent? } }
    write_file(path || @jekyll_path.join("_data", "redirects.yml"), payload.to_yaml(line_width: -1))
  end

  private

  def redirects
    Redirect.enabled.order(:id)
  end

  def status_code(redirect)
    redirect.permanent? ? 301 : 302
  end

  def write_file(path, content)
    FileUtils.mkdir_p(File.dirname(path))
    File.write(path, content.to_s)
  end
end
