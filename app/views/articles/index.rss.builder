xml.instruct! :xml, version: "1.0"
xml.rss version: "2.0",
        "xmlns:content" => "http://purl.org/rss/1.0/modules/content/" do
  raw_site_url = site_settings[:url].to_s.strip
  site_url = raw_site_url.presence&.chomp("/")
  site_url = "https://#{site_url}" if site_url.present? && !site_url.match?(%r{^https?://})

  xml.channel do
    xml.title site_settings[:title]
    xml.description site_settings[:description]
    xml.link(site_url.presence || site_settings[:url])
    xml.author site_settings[:author]

    @articles.each do |article|
      xml.item do
        fallback_title = article.plain_text_content.to_s.squish[0, 20]
        xml.title article.title.presence || fallback_title.presence || article.created_at.strftime("%Y-%m-%d")
        xml.description article.description.presence || article.excerpt

        # Build content with source reference if available
        content_html = if article.html?
          article.html_content || ""
        else
          article.content.to_s
        end

        # Prepend source reference if article has source
        if article.has_source?
          source_ref_html = render(partial: "articles/source_reference", locals: { article: article }, formats: [ :html ])
          content_html = source_ref_html.to_s + content_html.to_s
        end

        xml.tag!("content:encoded") { xml.cdata! content_html }

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
