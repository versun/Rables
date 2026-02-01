# frozen_string_literal: true

require "test_helper"

class JekyllCommentsExporterTest < ActiveSupport::TestCase
  setup do
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      export_comments: true,
      comments_format: "yaml",
      include_pending_comments: false,
      include_social_comments: true,
      redirect_export_format: "netlify"
    )
    @article = Article.create!(
      title: "Test Article",
      slug: "test-article-#{Time.current.to_i}",
      description: "Test description",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )
    @exporter = JekyllCommentsExporter.new(@setting)
  end

  teardown do
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  test "initializes with default setting" do
    exporter = JekyllCommentsExporter.new
    assert_equal JekyllSetting.instance, exporter.setting
  end

  test "initializes with custom setting" do
    exporter = JekyllCommentsExporter.new(@setting)
    assert_equal @setting, exporter.setting
  end

  test "export_all creates comments directory" do
    @exporter.export_all
    assert File.directory?(@setting.comments_data_path)
  end

  test "export_all skips when path invalid" do
    @setting.jekyll_path = "/nonexistent"
    result = @exporter.export_all
    assert_nil result
  end

  test "export_all skips when export_comments disabled" do
    @setting.update!(export_comments: false)
    result = @exporter.export_all
    assert_nil result
  end

  test "export_for_article creates yaml file" do
    comment = Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Test Author",
      author_email: "test@example.com",
      content: "Great article!",
      status: :approved,
      platform: nil
    )

    @exporter.export_for_article(@article)

    filepath = File.join(@setting.comments_data_path, "#{@article.slug}.yaml")
    assert File.exist?(filepath)

    content = YAML.load_file(filepath)
    assert_equal 1, content.length
    assert_equal comment.id, content.first["id"]
    assert_equal "Test Author", content.first["author"]["name"]
  end

  test "export_for_article creates json file when format is json" do
    @setting.update!(comments_format: "json")
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Test Author",
      content: "Great article!",
      status: :approved,
      platform: nil
    )

    @exporter.export_for_article(@article)

    filepath = File.join(@setting.comments_data_path, "#{@article.slug}.json")
    assert File.exist?(filepath)

    content = JSON.parse(File.read(filepath))
    assert_equal 1, content.length
  end

  test "export_for_article excludes pending comments by default" do
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Approved",
      content: "Approved comment",
      status: :approved,
      platform: nil
    )
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Pending",
      content: "Pending comment",
      status: :pending,
      platform: nil
    )

    @exporter.export_for_article(@article)

    filepath = File.join(@setting.comments_data_path, "#{@article.slug}.yaml")
    content = YAML.load_file(filepath)
    assert_equal 1, content.length
    assert_equal "Approved", content.first["author"]["name"]
  end

  test "export_for_article includes pending comments when enabled" do
    @setting.update!(include_pending_comments: true)
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Approved",
      content: "Approved comment",
      status: :approved,
      platform: nil
    )
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Pending",
      content: "Pending comment",
      status: :pending,
      platform: nil
    )

    @exporter.export_for_article(@article)

    filepath = File.join(@setting.comments_data_path, "#{@article.slug}.yaml")
    content = YAML.load_file(filepath)
    assert_equal 2, content.length
  end

  test "export_for_article builds nested comment tree" do
    parent = Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Parent",
      content: "Parent comment",
      status: :approved,
      platform: nil
    )
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Reply",
      content: "Reply comment",
      status: :approved,
      platform: nil,
      parent_id: parent.id
    )

    @exporter.export_for_article(@article)

    filepath = File.join(@setting.comments_data_path, "#{@article.slug}.yaml")
    content = YAML.load_file(filepath)

    assert_equal 1, content.length
    assert_equal "Parent", content.first["author"]["name"]
    assert_equal 1, content.first["replies"].length
    assert_equal "Reply", content.first["replies"].first["author"]["name"]
  end

  test "export_for_article includes email hash for gravatar" do
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Test",
      author_email: "Test@Example.com",
      content: "Comment",
      status: :approved,
      platform: nil
    )

    @exporter.export_for_article(@article)

    filepath = File.join(@setting.comments_data_path, "#{@article.slug}.yaml")
    content = YAML.load_file(filepath)

    expected_hash = Digest::MD5.hexdigest("test@example.com")
    assert_equal expected_hash, content.first["author"]["email_hash"]
  end

  test "export_for_article skips when no comments" do
    @exporter.export_for_article(@article)

    filepath = File.join(@setting.comments_data_path, "#{@article.slug}.yaml")
    assert_not File.exist?(filepath)
  end

  test "stats tracks exported articles and comments" do
    # Create comments for our test article
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Test",
      content: "Comment 1",
      status: :approved,
      platform: nil
    )
    Comment.create!(
      commentable: @article,
      commentable_type: "Article",
      commentable_id: @article.id,
      article: @article,
      author_name: "Test",
      content: "Comment 2",
      status: :approved,
      platform: nil
    )

    @exporter.export_all

    # Stats should include at least our article and comments
    assert @exporter.stats[:articles] >= 1
    assert @exporter.stats[:comments] >= 2
  end
end
