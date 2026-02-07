require "net/http"
require "json"
class Listmonk < ApplicationRecord
  validates :api_key, presence: true
  validates :username, presence: true
  validates :url, presence: true, format: { with: URI.regexp, message: "格式无效" }

  # 检查是否已配置完成所有必要字段
  def configured?
    api_key.present? && username.present? && url.present?
  end

  # 获取所有列表
  def fetch_lists
    begin
      uri = URI("#{url}/api/lists")
      request = Net::HTTP::Get.new(uri)
      request.basic_auth(username, api_key)

      response = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == "https") do |http|
        http.request(request)
      end

      if response.is_a?(Net::HTTPSuccess)
        JSON.parse(response.body)["data"]["results"]
      else
        raise "Fetch Lists failed! #{response.code} - #{response.body}"
      end
    rescue => e
      ActivityLog.log!(
        action: :failed,
        target: :newsletter,
        level: :error,
        operation: "fetch_lists",
        error: e.message
      )
      []
    end
  end

  def fetch_templates
    begin
      uri = URI("#{url}/api/templates")
      request = Net::HTTP::Get.new(uri)
      request.basic_auth(username, api_key)

      response = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == "https") do |http|
        http.request(request)
      end

      if response.is_a?(Net::HTTPSuccess)
        JSON.parse(response.body)["data"]
      else
        raise "Fetch Template failed! #{response.code} - #{response.body}"
      end
    rescue => e
      ActivityLog.log!(
        action: :failed,
        target: :newsletter,
        level: :error,
        operation: "fetch_templates",
        error: e.message
      )
      []
    end
  end

  def create_campaigns(article, site_title)
    begin
      uri = URI("#{url}/api/campaigns")
      request = Net::HTTP::Post.new(uri)
      request.basic_auth(username, api_key)
      request["Content-Type"] = "application/json"
      request.body = {
        name: article.title,
        subject: "#{article.title} | #{site_title}",
        lists: [ list_id ],
        type: "regular",
        content_type: "html",
        messenger: "email",
        body: campaign_body(article),
        template_id: template_id,
        send_later: false
      }.to_json

      response = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == "https") do |http|
        http.request(request)
      end

      campaign_id = JSON.parse(response.body)["data"]["id"] if response.is_a?(Net::HTTPSuccess)

      if campaign_id
        ActivityLog.log!(
          action: :created,
          target: :newsletter,
          level: :info,
          operation: "campaign",
          title: article.title,
          campaign_id: campaign_id
        )
      else
        raise "Create Campaign failed! Title:#{article.title},Code:#{response.code} - #{response.body}"
      end

      campaign_id
    rescue => e
      ActivityLog.log!(
        action: :failed,
        target: :newsletter,
        level: :error,
        operation: "campaign",
        title: article.title,
        error: e.message
      )
      nil
    end
  end

  def send_newsletter(article, site_title = "")
    begin
      campaign_id = create_campaigns(article, site_title)
      return false unless campaign_id.present?

      uri = URI("#{url}/api/campaigns/#{campaign_id}/status")
      request = Net::HTTP::Put.new(uri)
      request.basic_auth(username, api_key)
      request["Content-Type"] = "application/json"
      request.body = {
        status: "running"
      }.to_json
      # running the campaign
      response = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == "https") do |http|
        http.request(request)
      end

      if response.is_a?(Net::HTTPSuccess)
        ActivityLog.log!(
          action: :sent,
          target: :newsletter,
          level: :info,
          operation: "campaign",
          title: article.title,
          campaign_id: campaign_id
        )
      else
        raise "Send Campaign failed! #{response.code} - #{response.body}"
      end

      true
    rescue => e
      ActivityLog.log!(
        action: :failed,
        target: :newsletter,
        level: :error,
        operation: "campaign_send",
        title: article.title,
        campaign_id: campaign_id,
        error: e.message
      )
      nil
    end
  end

  private

  def campaign_body(article)
    base_body = article.html? ? (article.html_content || "") : article.content.to_s
    return base_body unless article.has_source?

    reference_html = ApplicationController.renderer.render(
      partial: "articles/source_reference",
      locals: { article: article }
    )

    "#{reference_html}\n#{base_body}"
  end
end
