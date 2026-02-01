# frozen_string_literal: true

require "test_helper"

class JekyllRedirectsExporterTest < ActiveSupport::TestCase
  test "exports netlify redirects file" do
    dir = Dir.mktmpdir
    JekyllSetting.create!(jekyll_path: dir)
    Redirect.create!(regex: "/old", replacement: "/new", permanent: true, enabled: true)

    exporter = JekyllRedirectsExporter.new
    exporter.export_to_netlify

    content = File.read(File.join(dir, "_redirects"))
    assert_includes content, "/old /new 301"
  ensure
    FileUtils.remove_entry(dir) if dir
  end
end
