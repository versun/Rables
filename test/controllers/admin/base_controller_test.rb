# frozen_string_literal: true

require "test_helper"

class Admin::DummyBaseController < Admin::BaseController
  allow_unauthenticated_access

  def index
    fetch_articles(Article.all)
    render plain: "ok"
  end

  private

  def model_class
    Article
  end

  def redirect_path_after_batch
    "/admin/articles"
  end
end

class Admin::BaseControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "batch actions and fetch_articles in base controller" do
    with_routing do |set|
      set.draw do
        namespace :admin do
          get "dummy_base" => "dummy_base#index"
          post "dummy_base/batch_destroy" => "dummy_base#batch_destroy"
          post "dummy_base/batch_publish" => "dummy_base#batch_publish"
          post "dummy_base/batch_unpublish" => "dummy_base#batch_unpublish"
        end
      end

      get "/admin/dummy_base", params: { status: "draft" }
      assert_response :success

      draft_article = create_published_article(
        status: :draft,
        title: "Batch Draft",
        slug: "batch-draft-#{Time.current.to_i}"
      )

      post "/admin/dummy_base/batch_publish", params: { ids: [ draft_article.slug ] }
      assert_redirected_to "/admin/articles"
      assert draft_article.reload.publish?

      post "/admin/dummy_base/batch_unpublish", params: { ids: [ draft_article.slug ] }
      assert_redirected_to "/admin/articles"
      assert draft_article.reload.draft?

      delete_article = create_published_article(
        title: "Batch Delete",
        slug: "batch-delete-#{Time.current.to_i}-#{rand(1000)}"
      )

      assert_difference "Article.count", -1 do
        post "/admin/dummy_base/batch_destroy", params: { ids: [ delete_article.slug ] }
      end
    end
  end
end
