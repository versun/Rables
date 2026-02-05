# frozen_string_literal: true

require "test_helper"
require "rake"

class ExportDataTaskTest < ActiveSupport::TestCase
  setup do
    Rake.application = Rake::Application.new
    Rake.application.rake_require("tasks/export_data", [ Rails.root.join("lib").to_s ])
    Rake::Task.define_task(:environment)
  end

  test "export:all writes csv files and processes attachments" do
    article = create_published_article(
      content_type: :html,
      html_content: <<~HTML
        <figure data-trix-attachment='{"contentType":"image/png","url":"http://example.com/image.png","filename":"image.png"}'>
          <img src="http://example.com/image.png">
        </figure>
      HTML
    )
    SocialMediaPost.create!(article: article, platform: "mastodon", url: "https://example.com/post/1")
    Listmonk.create!(url: "http://listmonk.local", username: "user", api_key: "key", list_id: "1", template_id: "2")

    fixed_time = Time.zone.parse("2026-02-05 09:00:00")
    export_dir = Rails.root.join("export", "export_#{fixed_time.strftime('%Y%m%d_%H%M%S')}")

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "image-data")

    assert_output(/Starting data export/) do
      Time.stub(:current, fixed_time) do
        Net::HTTP.stub(:get_response, response) do
          with_task_reenabled("export:all", &:invoke)
        end
      end
    end

    assert File.exist?(File.join(export_dir, "articles.csv"))
    assert File.exist?(File.join(export_dir, "crossposts.csv"))
    assert File.exist?(File.join(export_dir, "listmonks.csv"))
    assert File.exist?(File.join(export_dir, "pages.csv"))
    assert File.exist?(File.join(export_dir, "settings.csv"))
    assert File.exist?(File.join(export_dir, "social_media_posts.csv"))
    assert File.exist?(File.join(export_dir, "users.csv"))
  ensure
    FileUtils.rm_rf(export_dir) if export_dir
  end

  private

  def with_task_reenabled(task_name)
    task = Rake::Task[task_name]
    task.reenable
    yield task
  end
end
