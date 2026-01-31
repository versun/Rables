# frozen_string_literal: true

require "test_helper"

class ApplicationControllerTest < ActionDispatch::IntegrationTest
  class DummyController < ApplicationController
    allow_unauthenticated_access

    def index
      render plain: "ok"
    end
  end

  test "redirects to setup when incomplete and sets time zone when complete" do
    Setting.first.update!(setup_completed: false)

    with_routing do |set|
      set.draw do
        get "/dummy" => "application_controller_test/dummy#index"
        get "/setup" => "setup#show", as: :setup
      end

      get "/dummy"
      assert_redirected_to setup_path
    end

    Setting.first.update!(setup_completed: true, time_zone: "Asia/Shanghai")
    CacheableSettings.refresh_site_info

    with_routing do |set|
      set.draw do
        get "/dummy" => "application_controller_test/dummy#index"
      end

      get "/dummy"
      assert_response :success
      assert_equal "Asia/Shanghai", Time.zone.name
    end
  ensure
    Time.zone = "UTC"
  end
end
