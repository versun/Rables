xml.instruct! :xml, version: "1.0"
xml.rss version: "2.0",
        "xmlns:content" => "http://purl.org/rss/1.0/modules/content/" do
  raw_site_url = site_settings[:url].to_s.strip
  site_url = raw_site_url.presence&.chomp("/")
  site_url = "https://#{site_url}" if site_url.present? && !site_url.match?(%r{^https?://})

  xml.channel do
    xml.title "Articles tagged with #{@tag.name} | #{site_settings[:title]}"
    xml.description "Latest articles tagged with #{@tag.name} from #{site_settings[:title]}"
    xml.link(site_url.present? ? "#{site_url}/tags/#{@tag.slug}" : tag_url(@tag.slug))
    xml.author site_settings[:author]

    @articles.each do |article|
      xml.item do
        xml.title article.title.presence || article.plain_text_content.to_s.squish[0, 20].presence || article.created_at.strftime("%Y-%m-%d")
        xml.description article.description
        if article.html?
          xml.tag!("content:encoded") { xml.cdata! (article.html_content || "") }
        else
          xml.tag!("content:encoded") { xml.cdata! article.content.to_s }
        end
        xml.pubDate article.created_at.rfc822
        article_path = [ Rails.application.config.x.article_route_prefix, article.slug ].reject(&:blank?).join("/")
        article_path = "/#{article_path}" unless article_path.start_with?("/")
        article_url = site_url.present? ? "#{site_url}#{article_path}" : article_path
        xml.link article_url
        xml.guid article_url
        xml.author site_settings[:author]
      end
    end
  end
end
