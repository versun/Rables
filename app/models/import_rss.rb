class ImportRss
  require "feedjira"
  require "cgi"
  require "open-uri"
  require "resolv"
  require "ipaddr"

  # Private, loopback and link-local ranges blocked to prevent SSRF
  BLOCKED_IP_RANGES = [
    IPAddr.new("0.0.0.0/8"),
    IPAddr.new("10.0.0.0/8"),
    IPAddr.new("100.64.0.0/10"),
    IPAddr.new("127.0.0.0/8"),
    IPAddr.new("169.254.0.0/16"),
    IPAddr.new("172.16.0.0/12"),
    IPAddr.new("192.0.0.0/24"),
    IPAddr.new("192.168.0.0/16"),
    IPAddr.new("198.18.0.0/15"),
    IPAddr.new("224.0.0.0/4"),
    IPAddr.new("240.0.0.0/4"),
    IPAddr.new("::1/128"),
    IPAddr.new("fc00::/7"),
    IPAddr.new("fe80::/10")
  ].freeze

  attr_reader :error_message, :imported_count, :failed_count

  def initialize(url, import_images = false)
    @url = url
    @import_images = import_images ? true : false
    @error_message = nil
    @imported_count = 0
    @failed_count = 0
  end

  def import_data
    ActivityLog.log!(
      action: :started,
      target: :import,
      level: :info,
      source: "rss",
      url: @url,
      import_images: @import_images
    )

    raise "Unsafe feed URL: #{@url}" unless safe_remote_url?(@url)

    feed = Feedjira.parse(URI.open(@url).read)

    feed.entries.each do |item|
      next if item.url.nil?

      begin
        import_entry(item)
        @imported_count += 1
      rescue StandardError => e
        @failed_count += 1
        Rails.event.notify("import_rss.entry_failed", component: "ImportRss", url: item.url, error: e.message, level: "error")
      end
    end

    ActivityLog.log!(
      action: :completed,
      target: :import,
      level: :info,
      source: "rss",
      url: @url,
      import_images: @import_images,
      imported_count: @imported_count,
      failed_count: @failed_count
    )
    true
  rescue StandardError => e
    @error_message = e.message
    ActivityLog.log!(
      action: :failed,
      target: :import,
      level: :error,
      source: "rss",
      url: @url,
      import_images: @import_images,
      imported_count: @imported_count,
      failed_count: @failed_count,
      error: e.message
    )
    false
  end

  def import_images(doc, title)
    doc.css("img").each do |img|
      src = img["src"]
      next unless src

      unless safe_remote_url?(src)
        Rails.event.notify("import_rss.image_skipped", component: "ImportRss", url: src, reason: "unsafe_url", level: "warn")
        next
      end

      begin
        URI.open(src) do |io|
          blob = ActiveStorage::Blob.create_and_upload!(
            io: io,
            filename: "#{title.parameterize}-#{SecureRandom.hex(4)}.#{io.content_type.split("/").last}",
            content_type: io.content_type
          )

          # Update image URL in content
          attachment = ActionText::Attachment.from_attachable(blob)
          relative_url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)
          attachment.node["url"] = relative_url

          # 确保SGID也是正确的
          correct_sgid = blob.to_sgid.to_s
          attachment.node["sgid"] = correct_sgid

          img.replace(attachment.node.to_html)
        end
      rescue StandardError => e
        # Skip the failing image instead of aborting the whole feed import
        Rails.event.notify("import_rss.image_download_failed", component: "ImportRss", url: src, error: e.message, level: "error")
      end
    end
    doc
  end

  private

  def import_entry(item)
    decoded_link = CGI.unescape(item.url)
    title = (item.title || item.published).to_s
    slug = decoded_link.split("/").last
    content = ActionText::Content.new(item.content)
    # Parse as a fragment so no doctype/html/body wrapper leaks into the article body
    doc = Nokogiri::HTML.fragment(content.to_s)

    doc = import_images(doc, title) if @import_images

    Article.create!(status: :publish,
                    title: title,
                    content: doc.to_html,
                    created_at: item.published,
                    slug: slug,
                    description: item.summary,
                    )
  end

  # SSRF guard: only http(s) URLs whose host resolves to public IPs are allowed.
  # DNS resolution failures are treated as unsafe.
  def safe_remote_url?(url)
    uri = URI.parse(url.to_s)
    return false unless %w[http https].include?(uri.scheme)

    host = uri.hostname
    return false if host.blank?

    addresses = Resolv.getaddresses(host)
    return false if addresses.empty?

    addresses.none? do |address|
      ip = begin
        IPAddr.new(address)
      rescue IPAddr::InvalidAddressError
        nil
      end
      ip.nil? || BLOCKED_IP_RANGES.any? { |range| range.include?(ip) }
    end
  rescue StandardError
    false
  end
end
