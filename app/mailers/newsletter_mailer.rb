require "uri"

class NewsletterMailer < ApplicationMailer
  def default_url_options
    base = super || {}
    options = active_storage_url_options(@site_url)
    return base if options.blank?

    base.merge(options)
  end

  def article_email(article, subscriber, site_info)
    @article = article
    @subscriber = subscriber
    @site_info = site_info
    @newsletter_setting = NewsletterSetting.instance
    @footer = @newsletter_setting.footer
    @site_url = normalized_site_url
    @article_url = article_full_url(@article, @site_url)

    # Build unsubscribe URL using rails_api_url
    api_uri = URI.parse(ApplicationController.helpers.rails_api_url)
    port = api_uri.port
    port = nil if (api_uri.scheme == "http" && port == 80) || (api_uri.scheme == "https" && port == 443)

    script_name = api_uri.path.presence
    script_name = nil if script_name == "/"

    url_options = { token: subscriber.unsubscribe_token, host: api_uri.host, protocol: api_uri.scheme }
    url_options[:port] = port if port
    url_options[:script_name] = script_name if script_name

    @unsubscribe_url = Rails.application.routes.url_helpers.unsubscribe_url(**url_options)

    from_email = @newsletter_setting.from_email
    if from_email.blank?
      Rails.event.notify "newsletter.mailer.missing_from_email",
        level: "error",
        component: "newsletter_mailer",
        fallback_email: "noreply@example.com"
      from_email = "noreply@example.com"
    end

    Rails.event.notify "newsletter.mailer.creating_email",
      level: "info",
      component: "newsletter_mailer",
      recipient: @subscriber.email,
      from_email: from_email,
      article_id: @article.id

    mail_obj = nil
    with_active_storage_url_options(@site_url) do
      mail_obj = mail(
        to: @subscriber.email,
        from: from_email,
        subject: "#{@article.title} | #{@site_info[:title]}"
      )
    end

    # 验证邮件对象设置
    Rails.event.notify "newsletter.mailer.email_created",
      level: "info",
      component: "newsletter_mailer",
      to: mail_obj.to,
      from: mail_obj.from

    mail_obj
  end

  def confirmation_email(subscriber, site_info)
    @subscriber = subscriber
    @site_info = site_info
    api_uri = URI.parse(ApplicationController.helpers.rails_api_url)
    port = api_uri.port
    port = nil if (api_uri.scheme == "http" && port == 80) || (api_uri.scheme == "https" && port == 443)

    script_name = api_uri.path.presence
    script_name = nil if script_name == "/"

    url_options = { token: subscriber.confirmation_token, host: api_uri.host, protocol: api_uri.scheme }
    url_options[:port] = port if port
    url_options[:script_name] = script_name if script_name

    @confirmation_url = Rails.application.routes.url_helpers.confirm_subscription_url(**url_options)

    newsletter_setting = NewsletterSetting.instance
    from_email = newsletter_setting.from_email || "noreply@example.com"

    mail(
      to: @subscriber.email,
      from: from_email,
      subject: "请确认您的订阅 | #{@site_info[:title]}"
    )
  end

  private

  def normalized_site_url
    return "" unless Setting.respond_to?(:table_exists?) && Setting.table_exists?

    raw_url = Setting.first&.url.to_s.strip
    return "" if raw_url.blank?

    site_url = raw_url.chomp("/")
    site_url = "https://#{site_url}" unless site_url.match?(%r{^https?://})
    site_url
  end

  def article_full_url(article, site_url)
    return "" if site_url.blank?

    # CMS mode: Article URLs point to the static Jekyll site
    # The URL format follows Jekyll's post structure: /YYYY/MM/DD/slug/
    # Use created_at as publication date (articles don't have published_at field)
    published_date = article.created_at
    jekyll_path = "/#{published_date.strftime('%Y/%m/%d')}/#{article.slug}/"
    "#{site_url}#{jekyll_path}"
  end

  def active_storage_url_options(site_url)
    return {} if site_url.blank?

    uri = URI.parse(site_url)
    options = { host: uri.host, protocol: uri.scheme }

    if uri.port && !((uri.scheme == "http" && uri.port == 80) || (uri.scheme == "https" && uri.port == 443))
      options[:port] = uri.port
    end

    if uri.path.present? && uri.path != "/"
      options[:script_name] = uri.path
    end

    options
  end

  def with_active_storage_url_options(site_url)
    previous = ActiveStorage::Current.url_options
    options = active_storage_url_options(site_url)
    ActiveStorage::Current.url_options = options if options.present?
    yield
  ensure
    ActiveStorage::Current.url_options = previous
  end
end
