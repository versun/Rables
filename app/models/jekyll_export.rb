class JekyllExport
  include Exports::HtmlAttachmentProcessing

  attr_reader :setting

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
  end

  # Generate full export
  def generate(articles: Article.publish, pages: Page.publish)
    {
      articles: articles.map { |article| export_article(article) },
      pages: pages.map { |page| export_page(page) }
    }
  end

  # Export single article
  def export_article(article)
    return nil unless article

    {
      filename: article_filename(article),
      content: article_to_markdown(article),
      front_matter: build_article_front_matter(article),
      attachments: export_attachments(article)
    }
  end

  # Export single page
  def export_page(page)
    return nil unless page

    {
      filename: page_filename(page),
      content: page_to_markdown(page),
      front_matter: build_page_front_matter(page),
      attachments: export_attachments(page)
    }
  end

  # Build Jekyll Front Matter for article
  def build_article_front_matter(article)
    published_date = article.created_at

    fm = {
      "layout" => "post",
      "title" => article.title,
      "date" => published_date.strftime("%Y-%m-%d %H:%M:%S %z"),
      "slug" => article.slug
    }

    # Add categories/tags
    if article.tags.any?
      tag_names = article.tags.pluck(:name)
      fm["categories"] = tag_names
      fm["tags"] = tag_names
    end

    # Add description
    fm["description"] = article.description if article.description.present?

    # Add meta fields
    fm["meta_title"] = article.meta_title if article.meta_title.present?
    fm["meta_description"] = article.meta_description if article.meta_description.present?

    # Add source reference
    if article.has_source?
      fm["source_author"] = article.source_author if article.source_author.present?
      fm["source_url"] = article.source_url if article.source_url.present?
    end

    # Add custom front matter mapping
    setting.front_matter_mapping_hash.each do |key, value|
      fm[key] = value
    end

    fm
  end

  # Build Jekyll Front Matter for page
  def build_page_front_matter(page)
    fm = {
      "layout" => "page",
      "title" => page.title,
      "slug" => page.slug
    }

    # Add order if present
    fm["order"] = page.page_order if page.page_order.present?

    # Add description (Page doesn't have description field)

    # Add meta fields (Page may not have these)
    fm["meta_title"] = page.meta_title if page.respond_to?(:meta_title) && page.meta_title.present?
    fm["meta_description"] = page.meta_description if page.respond_to?(:meta_description) && page.meta_description.present?

    # Add custom front matter mapping
    setting.front_matter_mapping_hash.each do |key, value|
      fm[key] = value
    end

    fm
  end

  # Convert article to Markdown
  def article_to_markdown(article)
    front_matter = build_article_front_matter(article)
    content = process_content(article)

    "---\n" +
      front_matter.to_yaml(line_width: -1) +
      "---\n\n" +
      content
  end

  # Convert page to Markdown
  def page_to_markdown(page)
    front_matter = build_page_front_matter(page)
    content = process_content(page)

    "---\n" +
      front_matter.to_yaml(line_width: -1) +
      "---\n\n" +
      content
  end

  # Process content (convert HTML to Markdown, handle images)
  def process_content(item)
    return "" unless item

    content = if item.html?
                item.html_content.to_s
    else
                item.content.to_s
    end

    # Process images and attachments
    process_html_content(content, item)
  end

  # Generate filename for article
  def article_filename(article)
    published_date = article.created_at
    date_prefix = published_date.strftime("%Y-%m-%d")
    "#{date_prefix}-#{article.slug}.md"
  end

  # Generate filename for page
  def page_filename(page)
    "#{page.slug}.md"
  end

  # Get list of attachments to export
  def export_attachments(item)
    return [] unless item.respond_to?(:content)

    # Extract attachment references from content
    attachments = []

    # Parse HTML content for attachments
    doc = Nokogiri::HTML::DocumentFragment.parse(item.content.to_s)

    # Find all image attachments
    doc.css("img").each do |img|
      src = img["src"]
      next unless src.present?

      # Handle ActiveStorage attachments
      if src.include?("/rails/active_storage/")
        # Extract blob ID or filename
        blob_id = src.match(/\/rails\/active_storage\/blobs\/[^\/]+\/([^\/]+)/)&.captures&.first
        if blob_id
          attachments << {
            type: :image,
            original_src: src,
            filename: img["alt"] || blob_id
          }
        end
      end
    end

    attachments
  end

  private

  # Process HTML content for Jekyll
  def process_html_content(html, item)
    return "" if html.blank?

    doc = Nokogiri::HTML::DocumentFragment.parse(html)

    # Process images
    doc.css("img").each do |img|
      process_image_tag(img, item)
    end

    # Convert to Markdown if needed, otherwise return HTML
    # For now, return HTML as Jekyll supports it
    doc.to_html
  end

  # Process image tag for Jekyll
  def process_image_tag(img, item)
    src = img["src"]
    return unless src.present?

    # Update src to Jekyll assets path
    if src.include?("/rails/active_storage/")
      # Generate Jekyll-compatible path
      jekyll_path = "/#{setting.images_directory}/#{item.slug}/#{File.basename(src)}"
      img["src"] = jekyll_path
    end

    # Ensure alt text
    img["alt"] ||= item.title
  end
end
