class AddExcerptToArticles < ActiveRecord::Migration[8.1]
  EXCERPT_LENGTH = 200

  def up
    add_column :articles, :excerpt, :text

    say_with_time "Backfilling article excerpts" do
      Article.reset_column_information
      Article.find_each do |article|
        excerpt = build_excerpt(article)
        article.update_columns(excerpt: excerpt)
      end
    end
  end

  def down
    remove_column :articles, :excerpt
  end

  private

  def build_excerpt(article)
    source = article.description.presence || article.plain_text_content
    text = source.to_s.squish
    return nil if text.blank?

    text.truncate(EXCERPT_LENGTH, separator: /\s/)
  end
end
