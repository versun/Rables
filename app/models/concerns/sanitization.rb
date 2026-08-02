module Sanitization
  # Shared safelist of HTML tags/attributes for content rendering.
  # Used by ApplicationHelper#safe_html_content, Article#sanitize_html and Page#sanitize_html.
  ALLOWED_HTML_TAGS = %w[
    p br div span
    h1 h2 h3 h4 h5 h6
    a img
    ul ol li dl dt dd
    table thead tbody tfoot tr th td caption colgroup col
    strong b em i u s strike del ins mark small
    blockquote q cite pre code kbd samp var
    hr
    figure figcaption
    article section aside header footer nav main
    details summary
    abbr address time
    sub sup
    ruby rt rp
    iframe video audio source
  ].freeze

  ALLOWED_HTML_ATTRIBUTES = %w[
    href src alt title class id style
    target rel
    width height
    colspan rowspan
    data-controller data-action data-target
    loading
    controls autoplay loop muted
    frameborder allow allowfullscreen
    name content
  ].freeze

  private

  # Add loading="lazy" to all images for better performance
  def add_lazy_loading_to_images(html)
    return html if html.blank?

    doc = Nokogiri::HTML5.fragment(html)
    doc.css("img").each do |img|
      img.set_attribute("loading", "lazy") unless img["loading"].present?
    end
    doc.to_html.html_safe
  end
end
