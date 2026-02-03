# frozen_string_literal: true

class AddStatusCreatedAtIndexToArticles < ActiveRecord::Migration[8.1]
  def change
    add_index :articles, [ :status, :created_at ]
  end
end
