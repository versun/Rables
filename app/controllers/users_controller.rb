class UsersController < ApplicationController
  layout "admin"

  allow_unauthenticated_access only: %i[ new create ]
  def new
    if User.exists?
      redirect_to root_path, notice: "Admin user already exists."
    else
      @user = User.new
    end
  end

  def edit
    @user = Current.user
  end

  def update
    @user = Current.user

    # Changing the password requires proving ownership of the current one
    if password_change_attempted? && !@user.authenticate(params[:user][:current_password].to_s)
      @user.errors.add(:current_password, "is incorrect")
      render :edit, status: :unprocessable_entity
      return
    end

    if @user.update(user_params)
      redirect_to admin_articles_path, notice: "Account was successfully updated."
    else
      render :edit, status: :unprocessable_entity
    end
  end

  def create
    @user = User.new(user_params)

    # Check-and-create inside one transaction to shrink the TOCTOU window
    result = User.transaction do
      if User.exists?
        :exists
      else
        @user.save ? :created : :invalid
      end
    end

    case result
    when :exists
      redirect_to root_path, notice: "Admin user already exists."
    when :created
      redirect_to new_session_path, notice: "Admin user created successfully."
    else
      render :new, status: :unprocessable_entity
    end
  end

  private
  def password_change_attempted?
    params.dig(:user, :password).present?
  end

  def user_params
    params.expect(user: [ :user_name, :password, :password_confirmation ])
  end
end
