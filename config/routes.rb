# frozen_string_literal: true

Rails.application.routes.draw do
  # Root redirects to admin
  root to: redirect("/admin")

  # User authentication and management (required for admin login/setup)
  resources :users, only: %i[new create edit update]
  resource :session
  resource :setup, only: %i[show create], controller: "setup"

  # Newsletter subscriptions (public access)
  resources :subscriptions, only: %i[index create]
  get "/confirm", to: "subscriptions#confirm", as: :confirm_subscription
  get "/unsubscribe", to: "subscriptions#unsubscribe", as: :unsubscribe

  # Admin namespace
  namespace :admin do
    # Admin root points to articles index
    get "/", to: "articles#index", as: :root

    # Content management
    resources :articles, path: "posts" do
      collection do
        get :drafts
        get :scheduled
        post :batch_destroy
        post :batch_publish
        post :batch_unpublish
        post :batch_add_tags
        post :batch_crosspost
        post :batch_newsletter
      end
      member do
        patch :publish
        patch :unpublish
        post :fetch_comments
      end
    end

    resources :pages do
      collection do
        post :batch_destroy
        post :batch_publish
        post :batch_unpublish
      end
      member do
        patch :reorder
      end
    end

    resources :tags do
      collection do
        post :batch_destroy
      end
    end

    # Comment management
    resources :comments do
      collection do
        post :batch_destroy
        post :batch_approve
        post :batch_reject
      end
      member do
        patch :approve
        patch :reject
        post :reply
      end
    end

    # System management
    resource :setting, only: %i[edit update]
    resources :static_files, only: %i[index create destroy]
    resources :redirects

    resource :newsletter, only: %i[show update], controller: "newsletter" do
      collection do
        post :verify
      end
    end
    resources :subscribers, only: %i[index destroy] do
      collection do
        post :batch_create
        post :batch_confirm
        post :batch_destroy
      end
    end
    resources :migrates, only: %i[index create]

    # Export file downloads
    get "downloads/:filename", to: "downloads#show", as: :download, constraints: { filename: /[^\/]+/ }
    resources :crossposts, only: %i[index update] do
      member do
        post :verify
      end
    end
    resources :git_integrations, only: %i[index update] do
      member do
        post :verify
      end
    end

    # Jekyll integration
    resource :jekyll, only: %i[show update], controller: "jekyll" do
      post :sync
      post :sync_article
      post :verify
      get :preview
    end
    resources :jekyll_sync_records, only: [ :index ]

    # Activity logs
    resources :activities, only: [ :index ]

    # Source reference API
    post "sources/fetch_twitter", to: "sources#fetch_twitter"

    # TinyMCE editor image upload
    post "editor_images", to: "editor_images#create"

    # Jobs and system monitoring
    mount MissionControl::Jobs::Engine, at: "/jobs", as: :jobs
  end

  # Public comment submission
  resources :comments, only: [ :create ]

  # Static files public access
  get "/static/*filename", to: "static_files#show", as: :static_file, format: false

  # Health check
  get "up" => "rails/health#show", as: :rails_health_check
end
