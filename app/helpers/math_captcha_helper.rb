module MathCaptchaHelper
  # Must outlive the longest page cache (pages are cached 24h, articles 1h),
  # otherwise a browser-cached page serves a form whose token is already expired.
  MATH_CAPTCHA_TTL = 25.hours

  # Dedicated HMAC verifier so captcha challenges stay valid on cached pages
  # without relying on the session.
  def self.math_captcha_verifier
    Rails.application.message_verifier("math-captcha")
  end

  def self.sign_math_captcha(payload)
    math_captcha_verifier.generate(payload, expires_in: MATH_CAPTCHA_TTL)
  end

  # Returns the verified challenge payload, or nil when the token is
  # tampered with or expired.
  def self.verify_math_captcha(token)
    math_captcha_verifier.verified(token.to_s)
  rescue ActiveSupport::MessageVerifier::InvalidSignature
    nil
  end

  def math_captcha_challenge(max: 10, chooser: [ true, false ], rng: Kernel)
    max = max.to_i
    max = 10 if max <= 0

    if chooser.sample
      a = rng.rand(0..max)
      b = rng.rand(0..(max - a))
      op = "+"
    else
      a = rng.rand(0..max)
      b = rng.rand(0..a)
      op = "-"
    end

    question = "#{a} #{op} #{b} ="
    token = MathCaptchaHelper.sign_math_captcha({ a: a, b: b, op: op })

    { a:, b:, op:, question:, token: }
  end
end
