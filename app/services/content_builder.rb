module ContentBuilder
  # 构建社交媒体发布内容
  # @param slug [String] 文章slug（如果提供了article则忽略此参数）
  # @param title [String] 文章标题（如果提供了article则忽略此参数）
  # @param content_text [String] 文章内容（如果提供了article则忽略此参数）
  # @param description_text [String, nil] 文章描述（可选，如果提供了article则忽略此参数）
  # @param article [Article] 文章对象，用于检查source reference和获取文章属性
  # @param max_length [Integer] 最大字符长度限制
  # @param always_add_link [Boolean] 是否总是添加链接（默认false，即根据内容长度判断）
  # @param count_non_ascii_double [Boolean] 是否将非ASCII字符计为2个字符（用于Twitter等平台）
  # @return [String] 构建好的内容
  def build_content(slug = nil, title = nil, content_text = nil, description_text = nil, article: nil, max_length: 300, always_add_link: false, count_non_ascii_double: false)
    # 如果提供了 article 对象，优先使用 article 的属性
    if article
      slug = article.slug
      title = article.title
      content_text = article.plain_text_content
      description_text = article.description
    end

    title = title.to_s
    content_text = (description_text.presence || content_text).to_s

    # 如果有source reference，确保始终放在最后（在Read more链接之后）
    source_url_text = if article&.has_source? && article.source_url.present?
      "\n#{article.source_url}"
    else
      ""
    end

    title_length = count_chars(title, count_non_ascii_double)
    content_length = count_chars(content_text, count_non_ascii_double)
    source_url_length = count_chars(source_url_text, count_non_ascii_double)

    # 检查是否需要添加链接
    body_length = if title.present?
      title_length + content_length + 1 # +1 for newline
    else
      content_length
    end
    total_length = body_length + source_url_length
    needs_link = always_add_link || total_length >= max_length || description_text.present?

    # 如果不需要链接，返回完整内容
    unless needs_link
      return "#{title}\n#{content_text}#{source_url_text}" if title.present?
      return "#{content_text}#{source_url_text}"
    end

    # 构建URL和链接文本
    post_url = build_post_url(slug, article)
    link_text = count_non_ascii_double ? "\nRead more:#{post_url}" : "\nRead more: #{post_url}"
    link_length = count_non_ascii_double ? 34 : (count_chars(link_text, false))

    suffix_text = "#{link_text}#{source_url_text}"
    suffix_length = link_length + source_url_length

    # 计算可用于标题和内容的长度
    available_length = max_length - suffix_length
    return suffix_text.lstrip if available_length <= 0

    if title.present?
      # 如果标题过长，截断标题
      if title_length >= available_length - 3
        truncated_title = if available_length > 3
          "#{truncate_text(title, available_length - 3, count_non_ascii_double)}..."
        else
          truncate_text(title, available_length, count_non_ascii_double)
        end
        return "#{truncated_title}#{suffix_text}"
      end

      # 计算内容可用长度
      remaining_length = available_length - title_length - 1 # -1 for newline after title

      # 如果没有足够空间放内容，只显示标题和链接
      return "#{title}#{suffix_text}" if remaining_length <= 4

      # 内容未超出则完整显示
      return "#{title}\n#{content_text}#{suffix_text}" if content_length <= remaining_length

      # 截断内容并添加省略号
      return "#{title}\n#{truncate_text(content_text, remaining_length - 3, count_non_ascii_double)}...#{suffix_text}"
    end

    # 没有标题时，仅处理内容
    return "#{content_text}#{suffix_text}" if content_length <= available_length
    return suffix_text.lstrip if available_length <= 4

    "#{truncate_text(content_text, available_length - 3, count_non_ascii_double)}...#{suffix_text}"
  end

  # 计算字符数（可选择非ASCII字符计为2）
  # @param str [String] 要计算的字符串
  # @param count_non_ascii_double [Boolean] 是否将非ASCII字符计为2个字符
  # @return [Integer] 字符数
  def count_chars(str, count_non_ascii_double = false)
    str = str.to_s
    return str.length unless count_non_ascii_double

    str.each_char.map { |c| c.ascii_only? ? 1 : 2 }.sum
  end

  # 截断文本到指定长度（支持非ASCII字符计数）
  # @param str [String] 要截断的字符串
  # @param max_length [Integer] 最大长度
  # @param count_non_ascii_double [Boolean] 是否将非ASCII字符计为2个字符
  # @return [String] 截断后的字符串
  def truncate_text(str, max_length, count_non_ascii_double = false)
    str = str.to_s
    return str[0...max_length] unless count_non_ascii_double

    current_length = 0
    chars = []

    str.each_char do |c|
      char_length = c.ascii_only? ? 1 : 2
      break if current_length + char_length > max_length
      current_length += char_length
      chars << c
    end

    chars.join("")
  end

  # 构建文章URL
  # @param slug [String] 文章slug
  # @param article [Article] 文章对象，用于获取发布日期
  # @return [String] 文章完整URL
  def build_post_url(slug, article = nil)
    # 确保获取完整URL，带scheme
    site_url = Setting.first&.url.presence || "http://localhost:3000"

    # 移除末尾的斜杠
    site_url = site_url.chomp("/")

    # 确保URL有scheme
    site_url = "https://#{site_url}" unless site_url.match?(%r{^https?://})

    # CMS mode: Article URLs follow Jekyll format: /YYYY/MM/DD/slug/
    if article
      # Use created_at as publication date (articles don't have published_at field)
      published_date = article.created_at
      "#{site_url}/#{published_date.strftime('%Y/%m/%d')}/#{slug}/"
    else
      "#{site_url}/#{slug}/"
    end
  end
end
