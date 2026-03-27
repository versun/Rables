class AddScheduledSendNewsletterToArticles < ActiveRecord::Migration[8.1]
  def change
    add_column :articles, :scheduled_send_newsletter, :boolean, default: false, null: false
  end
end
