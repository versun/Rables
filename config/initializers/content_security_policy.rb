# Be sure to restart your server when you modify this file.

# Define an application-wide content security policy.
# See the Securing Rails Applications Guide for more information:
# https://guides.rubyonrails.org/security.html#content-security-policy-header

Rails.application.configure do
  config.content_security_policy do |policy|
    policy.default_src :self
    # Font Awesome webfonts are served from cdnjs.cloudflare.com.
    policy.font_src    :self, "https://cdnjs.cloudflare.com"
    # Article/page HTML content may hotlink images from anywhere.
    policy.img_src     :self, :data, :https
    policy.object_src  :none
    # cdn.jsdelivr.net: prism, tinymce, highlight.js (see config/importmap.rb).
    # giscus.app: the admin-pasted giscus embed snippet.
    # :unsafe_inline: inline snippets injected via the head_code setting.
    policy.script_src  :self, :unsafe_inline, "https://cdn.jsdelivr.net", "https://giscus.app"
    # cdn.jsdelivr.net / cdnjs.cloudflare.com: prism/highlight.js/font-awesome CSS.
    policy.style_src   :self, :unsafe_inline, "https://cdn.jsdelivr.net", "https://cdnjs.cloudflare.com"
    # giscus comments iframe and third-party embeds in article HTML content.
    policy.frame_src   :https
    policy.connect_src :self
  end

  # Report-only for now: admins can inject arbitrary third-party code through
  # the head_code setting (analytics, etc.), which a static policy cannot
  # enumerate. Watch the reports, then switch to enforcing by removing this.
  config.content_security_policy_report_only = true
end
