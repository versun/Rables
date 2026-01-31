# frozen_string_literal: true

require "test_helper"

class TagTest < ActiveSupport::TestCase
  def setup
    @tag = Tag.new(name: "Test Tag")
  end

  test "should be valid with valid attributes" do
    assert @tag.valid?
  end

  test "should require name" do
    @tag.name = nil
    assert_not @tag.valid?
    assert_includes @tag.errors[:name], "can't be blank"
  end

  test "should require unique name" do
    existing_tag = tags(:ruby)
    @tag.name = existing_tag.name
    assert_not @tag.valid?
    assert_includes @tag.errors[:name], "has already been taken"
  end

  test "should be case insensitive for uniqueness" do
    existing_tag = tags(:ruby)
    @tag.name = existing_tag.name.upcase
    assert_not @tag.valid?
  end

  test "should generate slug from name" do
    @tag.name = "My Test Tag"
    @tag.valid?
    assert_equal "My Test Tag", @tag.slug
  end

  test "should generate unique slug when duplicate exists" do
    Tag.create!(name: "Test Tag")
    @tag.name = "Test Tag"
    @tag.valid?
    assert @tag.slug.start_with?("Test Tag")
  end

  test "should require unique slug" do
    existing_tag = tags(:ruby)
    @tag.slug = existing_tag.slug
    @tag.name = "Different Name"
    assert_not @tag.valid?
    assert_includes @tag.errors[:slug], "has already been taken"
  end

  test "alphabetical scope should order by name" do
    tag1 = Tag.create!(name: "Zebra")
    tag2 = Tag.create!(name: "Apple")

    alphabetical = Tag.alphabetical
    assert_equal tag2, alphabetical.first
    assert_equal tag1, alphabetical.last
  end

  test "find_or_create_by_names should create tags from comma-separated string" do
    # Use names that don't exist in fixtures
    tags = Tag.find_or_create_by_names("python, golang, typescript")

    assert_equal 3, tags.count
    assert tags.map(&:name).include?("python")
    assert tags.map(&:name).include?("golang")
    assert tags.map(&:name).include?("typescript")
  end

  test "find_or_create_by_names should reuse existing tags" do
    existing_tag = tags(:ruby)
    tags = Tag.find_or_create_by_names("ruby, new-tag")

    assert_equal 2, tags.count
    assert_includes tags, existing_tag
  end

  test "find_or_create_by_names should handle blank string" do
    tags = Tag.find_or_create_by_names("")
    assert_equal 0, tags.count
  end

  test "find_or_create_by_names should handle nil" do
    tags = Tag.find_or_create_by_names(nil)
    assert_equal 0, tags.count
  end

  test "find_or_create_by_names should strip whitespace" do
    tags = Tag.find_or_create_by_names("  python  ,  golang  ")
    assert_equal 2, tags.count
    assert tags.map(&:name).include?("python")
    assert tags.map(&:name).include?("golang")
  end

  test "find_or_create_by_names should remove duplicates" do
    tags = Tag.find_or_create_by_names("ruby, rails, ruby")
    assert_equal 2, tags.count
  end

  test "articles_count should return count of articles" do
    tag = tags(:ruby)
    article1 = create_published_article
    article2 = create_published_article

    article1.tags << tag
    article2.tags << tag

    assert_equal 2, tag.articles_count
  end

  test "should have many articles through article_tags" do
    tag = tags(:ruby)
    assert_respond_to tag, :articles
  end

  test "should destroy associated article_tags when destroyed" do
    tag = tags(:ruby)
    article = create_published_article
    article.tags << tag

    assert_difference "ArticleTag.count", -1 do
      tag.destroy
    end
  end
end
