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
end
