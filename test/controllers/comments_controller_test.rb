# frozen_string_literal: true

require "test_helper"

class CommentsControllerTest < ActionDispatch::IntegrationTest
  setup do
    # Comments must be enabled on the fixture article for most tests
    articles(:published_article).update_column(:comment, true)
  end

  def captcha_params(a: 3, b: 4, op: "+", answer: nil)
    expected = op == "+" ? (a + b) : (a - b)
    token = MathCaptchaHelper.sign_math_captcha({ a: a, b: b, op: op })
    { captcha: { a:, b:, op:, token:, answer: (answer || expected).to_s } }
  end

  # Simulate the rate limit counter already exceeding the threshold
  def with_rate_limit_count(count)
    cache = Rails.cache
    cache.define_singleton_method(:increment) { |*| count }
    yield
  ensure
    cache.singleton_class.send(:remove_method, :increment)
  end

  test "should reject comment without captcha" do
    article = articles(:published_article)

    assert_no_difference "Comment.count" do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Spammer",
          content: "Buy now!"
        }
      }, as: :json
    end

    assert_response :unprocessable_entity
    assert_equal false, response.parsed_body["success"]
  end

  test "should create comment with valid captcha" do
    article = articles(:published_article)

    assert_difference "Comment.count", 1 do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Alice",
          content: "Nice post!"
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :created
    assert_equal true, response.parsed_body["success"]
  end

  test "should create comment with author_email" do
    article = articles(:published_article)

    assert_difference "Comment.count", 1 do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Alice",
          author_email: "alice@example.com",
          content: "Nice post!"
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :created
    assert_equal "alice@example.com", Comment.order(:created_at).last.author_email
  end

  test "shows success message after turbo submit" do
    article = articles(:published_article)
    article.update!(comment: true)

    assert_difference "Comment.count", 1 do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Alice",
          content: "Nice post!"
        }
      }.merge(captcha_params), as: :turbo_stream
    end

    assert_response :success
    assert_includes response.body, "comment-form-container"
    assert_includes response.body, "Your comment will be reviewed"
  end

  test "shows success message after turbo reply submit" do
    article = articles(:published_article)
    article.update!(comment: true)
    parent = comments(:approved_comment)

    assert_difference "Comment.count", 1 do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Alice",
          content: "Replying here",
          parent_id: parent.id
        }
      }.merge(captcha_params), as: :turbo_stream
    end

    assert_response :success
    assert_includes response.body, "comment-form-#{parent.id}"
    assert_includes response.body, "Your comment will be reviewed"
  end

  test "turbo submit with invalid captcha preserves input and shows error" do
    article = articles(:published_article)
    article.update!(comment: true)

    post comments_path(article_id: article.slug), params: {
      comment: {
        author_name: "Alice",
        content: "Nice post!"
      },
      captcha: { a: "1", b: "1", op: "+", answer: "" }
    }, as: :turbo_stream

    assert_response :unprocessable_entity
    assert_includes response.body, "验证失败"
    assert_includes response.body, "value=\"Alice\""
    assert_includes response.body, "Nice post!"
  end

  test "turbo submit with validation error preserves input and shows error" do
    article = articles(:published_article)
    article.update!(comment: true)

    post comments_path(article_id: article.slug), params: {
      comment: {
        author_name: "Alice",
        content: ""
      }
    }.merge(captcha_params), as: :turbo_stream

    assert_response :unprocessable_entity
    assert_includes response.body, "提交评论时出错"
    assert_includes response.body, "value=\"Alice\""
  end

  test "html submit with invalid captcha redirects with alert" do
    article = articles(:published_article)

    assert_no_difference "Comment.count" do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Alice",
          content: "Nice post!"
        },
        captcha: { a: "1", b: "1", op: "+", answer: "" }
      }
    end

    assert_redirected_to article_path(article)
    assert_match "验证失败", flash[:alert]
  end

  test "creates comment for page with valid captcha" do
    page = pages(:published_page)

    assert_difference "Comment.count", 1 do
      post comments_path(page_id: page.slug), params: {
        comment: {
          author_name: "Page User",
          content: "Nice page!"
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :created
  end

  test "invalid comment returns unprocessable json" do
    article = articles(:published_article)

    assert_no_difference "Comment.count" do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "",
          content: ""
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :unprocessable_entity
    assert_equal false, response.parsed_body["success"]
  end

  test "xhr html request returns json on success" do
    article = articles(:published_article)

    assert_difference "Comment.count", 1 do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "XHR User",
          content: "XHR comment"
        }
      }.merge(captcha_params), headers: { "X-Requested-With" => "XMLHttpRequest" }
    end

    assert_response :created
    assert_includes response.body, "评论已提交"
  end

  test "unexpected error returns json error" do
    article = articles(:published_article)

    Comment.class_eval do
      alias_method :original_save, :save
      def save(*)
        raise "boom"
      end
    end

    post comments_path(article_id: article.slug), params: {
      comment: {
        author_name: "Crash",
        content: "Crash"
      }
    }.merge(captcha_params), as: :json

    assert_response :internal_server_error
    assert_includes response.body, "提交评论时发生错误"
  ensure
    Comment.class_eval do
      alias_method :save, :original_save
      remove_method :original_save
    end
  end

  test "xhr html request returns json on captcha failure" do
    article = articles(:published_article)

    post comments_path(article_id: article.slug), params: {
      comment: {
        author_name: "XHR User",
        content: "XHR comment"
      },
      captcha: { a: "1", b: "1", op: "+", answer: "" }
    }, headers: { "X-Requested-With" => "XMLHttpRequest" }

    assert_response :unprocessable_entity
    assert_includes response.body, "验证失败"
  end

  test "html invalid comment redirects with alert" do
    article = articles(:published_article)

    assert_no_difference "Comment.count" do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "",
          content: ""
        }
      }.merge(captcha_params)
    end

    assert_redirected_to article_path(article)
    assert_match "提交评论时出错", flash[:alert]
  end

  test "html comment on page redirects to page" do
    page = pages(:published_page)

    assert_difference "Comment.count", 1 do
      post comments_path(page_id: page.slug), params: {
        comment: {
          author_name: "Page Html",
          content: "Nice page!"
        }
      }.merge(captcha_params)
    end

    assert_redirected_to page_path(page)
  end

  test "xhr html request returns json on validation error" do
    article = articles(:published_article)

    assert_no_difference "Comment.count" do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "",
          content: ""
        }
      }.merge(captcha_params), headers: { "X-Requested-With" => "XMLHttpRequest" }
    end

    assert_response :unprocessable_entity
    assert_includes response.body, "提交评论时出错"
  end

  test "rescues record not found during build" do
    article = articles(:published_article)

    original_comments = Article.instance_method(:comments)
    Article.define_method(:comments) { raise ActiveRecord::RecordNotFound }

    post comments_path(article_id: article.slug), params: {
      comment: {
        author_name: "Ghost",
        content: "Missing"
      }
    }.merge(captcha_params), as: :json

    assert_response :not_found
    assert_includes response.body, "文章或页面未找到"
  ensure
    Article.define_method(:comments, original_comments)
  end

  test "turbo stream record not found targets reply form" do
    article = articles(:published_article)
    parent = comments(:approved_comment)

    original_comments = Article.instance_method(:comments)
    Article.define_method(:comments) { raise ActiveRecord::RecordNotFound }

    post comments_path(article_id: article.slug), params: {
      comment: {
        author_name: "Ghost",
        content: "Missing",
        parent_id: parent.id
      }
    }.merge(captcha_params), as: :turbo_stream

    assert_response :not_found
    assert_includes response.body, "comment-form-#{parent.id}"
    assert_includes response.body, "文章或页面未找到"
  ensure
    Article.define_method(:comments, original_comments)
  end

  test "xhr html record not found returns json" do
    article = articles(:published_article)
    original_comments = Article.instance_method(:comments)
    Article.define_method(:comments) { raise ActiveRecord::RecordNotFound }

    post comments_path(article_id: article.slug), params: {
      comment: {
        author_name: "Ghost",
        content: "Missing"
      }
    }.merge(captcha_params), headers: { "X-Requested-With" => "XMLHttpRequest" }

    assert_response :not_found
    assert_includes response.body, "文章或页面未找到"
  ensure
    Article.define_method(:comments, original_comments)
  end

  test "html record not found redirects to root" do
    article = articles(:published_article)
    original_comments = Article.instance_method(:comments)
    Article.define_method(:comments) { raise ActiveRecord::RecordNotFound }

    post comments_path(article_id: article.slug), params: {
      comment: {
        author_name: "Ghost",
        content: "Missing"
      }
    }.merge(captcha_params)

    assert_redirected_to root_path
  ensure
    Article.define_method(:comments, original_comments)
  end

  test "rejects comment with tampered captcha token" do
    article = articles(:published_article)
    tampered = captcha_params
    tampered[:captcha][:token] = "#{tampered[:captcha][:token]}tampered"

    assert_no_difference "Comment.count" do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Mallory",
          content: "Forged captcha"
        }
      }.merge(tampered), as: :json
    end

    assert_response :unprocessable_entity
    assert_equal false, response.parsed_body["success"]
  end

  test "rejects comment with expired captcha token" do
    article = articles(:published_article)
    expired = captcha_params

    travel(MathCaptchaHelper::MATH_CAPTCHA_TTL + 1.minute) do
      assert_no_difference "Comment.count" do
        post comments_path(article_id: article.slug), params: {
          comment: {
            author_name: "Late",
            content: "Too late"
          }
        }.merge(expired), as: :json
      end
    end

    assert_response :unprocessable_entity
    assert_equal false, response.parsed_body["success"]
  end

  test "rejects comment on draft article" do
    article = articles(:draft_article)
    article.update_column(:comment, true)

    assert_no_difference "Comment.count" do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Alice",
          content: "Nice post!"
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :not_found
  end

  test "rejects comment when comments are disabled" do
    article = articles(:published_article)
    article.update_column(:comment, false)

    assert_no_difference "Comment.count" do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Alice",
          content: "Nice post!"
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :not_found
  end

  test "creates comment on shared article with comments enabled" do
    article = articles(:shared_article)
    article.update_column(:comment, true)

    assert_difference "Comment.count", 1 do
      post comments_path(article_id: article.slug), params: {
        comment: {
          author_name: "Alice",
          content: "Nice share!"
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :created
  end

  test "rejects comment on draft page" do
    page = pages(:draft_page)
    page.update_column(:comment, true)

    assert_no_difference "Comment.count" do
      post comments_path(page_id: page.slug), params: {
        comment: {
          author_name: "Page User",
          content: "Nice page!"
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :not_found
  end

  test "rate limits comment creation per ip" do
    article = articles(:published_article)

    assert_no_difference "Comment.count" do
      with_rate_limit_count(6) do
        post comments_path(article_id: article.slug), params: {
          comment: {
            author_name: "Spam Bot",
            content: "Spam"
          }
        }.merge(captcha_params)
      end
    end

    assert_redirected_to article_path(article)
    assert_match "请稍后再试", flash[:alert]
  end
end
