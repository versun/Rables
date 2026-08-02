import { Controller } from "@hotwired/stimulus"

export default class extends Controller {
  static targets = ["providerSelect", "nativeSettings", "listmonkSettings", "verifyBtn", "verifyStatus", "listSelect", "templateSelect", "sesVerifyBtn", "sesVerifyStatus"]
  static values = { verifyUrl: String, activeTab: String }

  connect() {
    this.updateSettingsVisibility()
  }

  providerChanged() {
    this.updateSettingsVisibility()
  }

  async verify() {
    const verifyBtn = this.verifyBtnTarget
    const verifyStatus = this.verifyStatusTarget
    const listSelect = this.listSelectTarget
    const templateSelect = this.templateSelectTarget

    // 更新状态
    verifyStatus.textContent = 'Verifying...'
    verifyStatus.style.color = 'gray'
    verifyBtn.disabled = true

    // 获取表单数据（带上当前选中的列表与模板，后端据此回显选中项）
    const formData = {
      url: document.getElementById('listmonk_url').value,
      username: document.getElementById('listmonk_username').value,
      api_key: document.getElementById('listmonk_api_key').value,
      list_id: listSelect.value,
      template_id: templateSelect.value
    }

    try {
      const response = await fetch(this.verifyUrlValue, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': document.querySelector('meta[name="csrf-token"]').content
        },
        body: JSON.stringify(formData)
      })

      const data = await response.json()

      if (data.success) {
        verifyStatus.textContent = '✓ Verification successful!'
        verifyStatus.style.color = 'green'
        
        // 更新列表选择器
        listSelect.innerHTML = '<option value="">Select a List</option>'
        data.lists.forEach(list => {
          const option = document.createElement('option')
          option.value = list.id
          option.textContent = list.name
          // option.value 与 current_list_id 类型不一致（数字 vs 字符串），统一转字符串比较
          if (String(list.id) === String(data.current_list_id)) {
            option.selected = true
          }
          listSelect.appendChild(option)
        })

        // 更新模板选择器
        templateSelect.innerHTML = '<option value="">Select a Template</option>'
        data.templates.forEach(template => {
          const option = document.createElement('option')
          option.value = template.id
          option.textContent = template.name
          if (String(template.id) === String(data.current_template_id)) {
            option.selected = true
          }
          templateSelect.appendChild(option)
        })
      } else {
        verifyStatus.textContent = '✗ ' + (data.error || 'Verification failed')
        verifyStatus.style.color = 'red'
      }
    } catch (error) {
      verifyStatus.textContent = '✗ Error: ' + error.message
      verifyStatus.style.color = 'red'
    } finally {
      verifyBtn.disabled = false
    }
  }

  async verifySes() {
    const verifyBtn = this.sesVerifyBtnTarget
    const verifyStatus = this.sesVerifyStatusTarget

    // 更新状态
    verifyStatus.textContent = 'Verifying...'
    verifyStatus.style.color = 'gray'
    verifyBtn.disabled = true

    // 获取表单数据
    const formData = {
      smtp_address: document.getElementById('newsletter_setting_smtp_address').value,
      smtp_port: document.getElementById('newsletter_setting_smtp_port').value,
      smtp_user_name: document.getElementById('newsletter_setting_smtp_user_name').value,
      smtp_password: document.getElementById('newsletter_setting_smtp_password').value,
      smtp_domain: document.getElementById('newsletter_setting_smtp_domain').value,
      smtp_authentication: document.getElementById('newsletter_setting_smtp_authentication').value,
      smtp_enable_starttls: document.getElementById('newsletter_setting_smtp_enable_starttls').checked ? '1' : '0',
      from_email: document.getElementById('newsletter_setting_from_email').value
    }

    try {
      const response = await fetch(this.verifyUrlValue, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': document.querySelector('meta[name="csrf-token"]').content
        },
        body: JSON.stringify(formData)
      })

      const data = await response.json()

      if (data.success) {
        verifyStatus.textContent = '✓ ' + (data.message || 'Verification successful!')
        verifyStatus.style.color = 'green'
      } else {
        verifyStatus.textContent = '✗ ' + (data.error || 'Verification failed')
        verifyStatus.style.color = 'red'
      }
    } catch (error) {
      verifyStatus.textContent = '✗ Error: ' + error.message
      verifyStatus.style.color = 'red'
    } finally {
      verifyBtn.disabled = false
    }
  }

  updateSettingsVisibility() {
    if (this.hasActiveTabValue && this.activeTabValue) {
      if (this.hasNativeSettingsTarget) {
        this.nativeSettingsTarget.style.display = this.activeTabValue === 'native' ? 'block' : 'none'
      }
      if (this.hasListmonkSettingsTarget) {
        this.listmonkSettingsTarget.style.display = this.activeTabValue === 'listmonk' ? 'block' : 'none'
      }
      return
    }

    if (!this.hasProviderSelectTarget) return
    const provider = this.providerSelectTarget.value

    if (this.hasNativeSettingsTarget) {
      this.nativeSettingsTarget.style.display = provider === 'native' ? 'block' : 'none'
    }
    if (this.hasListmonkSettingsTarget) {
      this.listmonkSettingsTarget.style.display = provider === 'native' ? 'none' : 'block'
    }
  }
}
