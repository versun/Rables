Rails.application.routes.draw do
  # Root redirects to admin
  root to: redirect("/admin")

  # User authentication and management (needed for admin login)
  resources :users
  resource :session
  resource :setup, only: [ :show, :create ], controller: "setup"

  # Newsletter subscriptions (public API)
  resources :subscriptions, only: [ :index, :create ]
  get "/confirm", to: "subscriptions#confirm", as: :confirm_subscription
  get "/unsubscribe", to: "subscriptions#unsubscribe", as: :unsubscribe

  # Admin namespace - 统一所有后台管理功能
  namespace :admin do
    # Admin root now points to articles index
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
    resource :setting, only: [ :edit, :update ]
    resources :static_files, only: [ :index, :create, :destroy ]
    resources :redirects

    resource :newsletter, only: [ :show, :update ], controller: "newsletter" do
      collection do
        post :verify
      end
    end
    resources :subscribers, only: [ :index, :destroy ] do
      collection do
        post :batch_create
        post :batch_confirm
        post :batch_destroy
      end
    end
    resources :migrates, only: [ :index, :create ]

    # 导出文件下载
    get "downloads/:filename", to: "downloads#show", as: :download, constraints: { filename: /[^\/]+/ }
    resources :crossposts, only: [ :index, :update ] do
      member do
        post :verify
      end
    end
    resources :git_integrations, only: [ :index, :update ] do
      member do
        post :verify
      end
    end

    # Activity logs
    resources :activities, only: [ :index ]

    # Jekyll integration
    resource :jekyll, only: [ :show, :update ], controller: "jekyll" do
      post :sync
      post :sync_article
      post :verify
      get :preview
    end
    resources :jekyll_sync_records, only: [ :index ]

    # Source reference API
    post "sources/fetch_twitter", to: "sources#fetch_twitter"

    # TinyMCE editor image upload
    post "editor_images", to: "editor_images#create"

    # Jobs and system monitoring
    mount MissionControl::Jobs::Engine, at: "/jobs", as: :jobs
  end

  # Public comment submission (API only)
  resources :comments, only: [ :create ]

  # Static files public access
  get "/static/*filename", to: "static_files#show", as: :static_file, format: false

  # Health check
  get "up" => "rails/health#show", as: :rails_health_check
end
