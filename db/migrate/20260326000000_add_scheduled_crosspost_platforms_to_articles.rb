class AddScheduledCrosspostPlatformsToArticles < ActiveRecord::Migration[8.1]
  def change
    add_column :articles, :scheduled_crosspost_platforms, :text, default: "[]", null: false
  end
end
