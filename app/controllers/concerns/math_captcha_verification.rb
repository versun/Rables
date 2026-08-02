module MathCaptchaVerification
  private

  def math_captcha_expected(max: 10)
    token = params.dig(:captcha, :token).to_s
    return nil if token.blank?

    payload = MathCaptchaHelper.verify_math_captcha(token)
    return nil unless payload.is_a?(Hash)

    a = payload["a"]
    b = payload["b"]
    op = payload["op"].to_s

    return nil unless a.is_a?(Integer) && b.is_a?(Integer)
    return nil unless (0..max).cover?(a) && (0..max).cover?(b)

    expected =
      case op
      when "+"
        a + b
      when "-"
        a - b
      end

    return nil unless expected && (0..max).cover?(expected)

    expected
  end

  def math_captcha_valid?(max: 10)
    expected = math_captcha_expected(max:)
    return false if expected.nil?

    answer = params.dig(:captcha, :answer).to_s.strip
    return false if answer.blank?

    Integer(answer, 10) == expected
  rescue ArgumentError, TypeError
    false
  end

  # Message for a failed captcha check. A missing, tampered, or expired
  # challenge token means the page is stale (e.g. cached before the token
  # existed, or open past the token TTL), so the remedy is reloading for a
  # fresh challenge; anything else means the answer itself was wrong.
  def math_captcha_error_message(max: 10)
    if math_captcha_expected(max:).nil?
      "验证已过期：请刷新页面后重新回答数学题。"
    else
      "验证失败：请回答数学题。"
    end
  end
end
