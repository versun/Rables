require "csv"
require "fileutils"
require "nokogiri"

namespace :export do
  desc "Export all data to CSV files and attachments"
  task all: :environment do
    puts "Starting data export..."

    # 创建导出目录
    export_dir = Rails.root.join("export", "export_#{Time.current.strftime('%Y%m%d_%H%M%S')}")
    FileUtils.mkdir_p(export_dir)
    attachments_dir = File.join(export_dir, "attachments")
    FileUtils.mkdir_p(attachments_dir)

    puts "Export directory: #{export_dir}"

    # 导出各个模型数据
    # export_activity_logs(export_dir)
    export_articles(export_dir, attachments_dir)
    export_crossposts(export_dir)
    export_listmonks(export_dir)
    export_pages(export_dir)
    export_settings(export_dir)
    export_social_media_posts(export_dir)
    export_users(export_dir)

    puts "Export completed! Files saved to: #{export_dir}"
  end

  def export_activity_logs(export_dir)
    puts "Exporting activity_logs..."

    CSV.open(File.join(export_dir, "activity_logs.csv"), "w", write_headers: true, headers: %w[id action target level description created_at updated_at]) do |csv|
      ActivityLog.find_each do |log|
        csv << [
          log.id,
          log.action,
          log.target,
          log.level,
          log.description,
          log.created_at,
          log.updated_at
        ]
      end
    end

    puts "Exported #{ActivityLog.count} activity_logs"
  end

  def export_articles(export_dir, attachments_dir)
    puts "Exporting articles and attachments..."

    CSV.open(File.join(export_dir, "articles.csv"), "w", write_headers: true, headers: %w[id title slug description content status scheduled_at crosspost_mastodon crosspost_twitter crosspost_bluesky send_newsletter created_at updated_at]) do |csv|
      Article.find_each do |article|
        content = if article.html?
          article.html_content || ""
        else
          article.content&.to_trix_html || ""
        end

        # 处理附件
        processed_content = process_article_content(content, article.id, attachments_dir)

        csv << [
          article.id,
          article.title,
          article.slug,
          article.description,
          processed_content,
          article.status,
          article.scheduled_at,
          article.crosspost_mastodon,
          article.crosspost_twitter,
          article.crosspost_bluesky,
          article.send_newsletter,
          article.created_at,
          article.updated_at
        ]
      end
    end

    puts "Exported #{Article.count} articles"
  end

  def process_article_content(content, article_id, attachments_dir)
    return content if content.blank?

    doc = Nokogiri::HTML.fragment(content)

    # 查找所有的附件标签
    doc.css("figure[data-trix-attachment]").each do |figure|
      attachment_data = JSON.parse(figure["data-trix-attachment"])

      if attachment_data["contentType"]&.start_with?("image/")
        # 获取原始URL和文件名
        original_url = attachment_data["url"]
        filename = attachment_data["filename"] || File.basename(original_url)

        # 创建文章特定的附件目录
        article_attachments_dir = File.join(attachments_dir, "article_#{article_id}")
        FileUtils.mkdir_p(article_attachments_dir)

        # 下载附件
        new_filename = "#{SecureRandom.hex(8)}_#{filename}"
        local_path = File.join(article_attachments_dir, new_filename)

        begin
          if original_url.present?
            # 如果是相对URL，添加主机前缀
            full_url = original_url.start_with?("http") ? original_url : "#{Rails.application.config.action_controller.asset_host}#{original_url}"

            # 下载文件
            uri = URI(full_url)
            response = Net::HTTP.get_response(uri)

            if response.is_a?(Net::HTTPSuccess)
              File.open(local_path, "wb") { |f| f.write(response.body) }

              # 更新content中的URL为新的相对路径
              new_url = "attachments/article_#{article_id}/#{new_filename}"
              attachment_data["url"] = new_url
              figure["data-trix-attachment"] = attachment_data.to_json

              # 更新img标签的src
              img = figure.at_css("img")
              img["src"] = new_url if img
            end
          end
        rescue => e
          puts "Error downloading attachment #{original_url}: #{e.message}"
        end
      end
    end

    doc.to_html
  end

  def export_crossposts(export_dir)
    puts "Exporting crossposts..."

    CSV.open(File.join(export_dir, "crossposts.csv"), "w", write_headers: true, headers: %w[id platform client_key client_secret access_token access_token_secret api_key api_key_secret username app_password enabled created_at updated_at]) do |csv|
      Crosspost.find_each do |crosspost|
        csv << [
          crosspost.id,
          crosspost.platform,
          crosspost.client_key,
          crosspost.client_secret,
          crosspost.access_token,
          crosspost.access_token_secret,
          crosspost.api_key,
          crosspost.api_key_secret,
          crosspost.username,
          crosspost.app_password,
          crosspost.enabled,
          crosspost.created_at,
          crosspost.updated_at
        ]
      end
    end

    puts "Exported #{Crosspost.count} crossposts"
  end

  def export_listmonks(export_dir)
    puts "Exporting listmonks..."

    CSV.open(File.join(export_dir, "listmonks.csv"), "w", write_headers: true, headers: %w[id url username api_key list_id enabled created_at updated_at]) do |csv|
      Listmonk.find_each do |listmonk|
        csv << [
          listmonk.id,
          listmonk.url,
          listmonk.username,
          listmonk.api_key,
          listmonk.list_id,
          listmonk.enabled,
          listmonk.created_at,
          listmonk.updated_at
        ]
      end
    end

    puts "Exported #{Listmonk.count} listmonks"
  end

  def export_pages(export_dir)
    puts "Exporting pages..."

    CSV.open(File.join(export_dir, "pages.csv"), "w", write_headers: true, headers: %w[id title slug content status redirect_url created_at updated_at]) do |csv|
      Page.find_each do |page|
        csv << [
          page.id,
          page.title,
          page.slug,
          page.content&.to_trix_html,
          page.status,
          page.redirect_url,
          page.created_at,
          page.updated_at
        ]
      end
    end

    puts "Exported #{Page.count} pages"
  end

  def export_settings(export_dir)
    puts "Exporting settings..."

    headers = %w[id title description author social_links footer head_code tool_code custom_css url time_zone created_at updated_at]
    CSV.open(File.join(export_dir, "settings.csv"), "w", write_headers: true, headers: headers) do |csv|
      Setting.find_each do |setting|
        csv << [
          setting.id,
          setting.title,
          setting.description,
          setting.author,
          setting.social_links&.to_json,
          setting.footer&.to_trix_html,
          setting.head_code,
          setting.tool_code,
          setting.custom_css,
          setting.url,
          setting.time_zone,
          setting.created_at,
          setting.updated_at
        ]
      end
    end

    puts "Exported #{Setting.count} settings"
  end

  def export_social_media_posts(export_dir)
    puts "Exporting social_media_posts..."

    CSV.open(File.join(export_dir, "social_media_posts.csv"), "w", write_headers: true, headers: %w[id article_id platform url created_at updated_at]) do |csv|
      SocialMediaPost.find_each do |post|
        csv << [
          post.id,
          post.article_id,
          post.platform,
          post.url,
          post.created_at,
          post.updated_at
        ]
      end
    end

    puts "Exported #{SocialMediaPost.count} social_media_posts"
  end

  def export_users(export_dir)
    puts "Exporting users..."

    headers = %w[id user_name created_at updated_at]
    CSV.open(File.join(export_dir, "users.csv"), "w", write_headers: true, headers: headers) do |csv|
      User.find_each do |user|
        csv << [
          user.id,
          user.user_name,
          user.created_at,
          user.updated_at
        ]
      end
    end

    puts "Exported #{User.count} users"
  end
end
