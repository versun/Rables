import { Controller } from "@hotwired/stimulus"

export default class extends Controller {
  static targets = ["menu"]
  static values = { url: String }

  connect() {
    this.boundCloseOnOutsideClick = this.closeOnOutsideClick.bind(this)
  }

  disconnect() {
    document.removeEventListener('click', this.boundCloseOnOutsideClick)
  }

  toggle(event) {
    event.stopPropagation()

    const isVisible = this.menuTarget.style.display === 'block'

    // 关闭所有其他分享菜单
    document.querySelectorAll('[data-share-target="menu"]').forEach(menu => {
      if (menu !== this.menuTarget) {
        menu.style.display = 'none'
      }
    })

    this.menuTarget.style.display = isVisible ? 'none' : 'block'

    if (!isVisible) {
      setTimeout(() => {
        document.addEventListener('click', this.boundCloseOnOutsideClick)
      }, 0)
    } else {
      document.removeEventListener('click', this.boundCloseOnOutsideClick)
    }
  }

  close(event) {
    this.closeMenu()
  }

  closeOnOutsideClick(event) {
    if (!this.element.contains(event.target)) {
      this.closeMenu()
    }
  }

  copy(event) {
    event.preventDefault()
    event.stopPropagation()

    const url = this.urlValue
    const target = event.currentTarget

    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(url).then(() => {
        this.showCopySuccess(target)
      }).catch(err => {
        console.error('复制失败:', err)
        this.fallbackCopy(url, target)
      })
    } else {
      this.fallbackCopy(url, target)
    }
  }

  fallbackCopy(text, target) {
    const textArea = document.createElement("textarea")
    textArea.value = text
    textArea.style.position = "fixed"
    textArea.style.left = "-999999px"
    document.body.appendChild(textArea)
    textArea.focus()
    textArea.select()

    try {
      const successful = document.execCommand('copy')
      if (successful) {
        this.showCopySuccess(target)
      } else {
        alert('复制失败，请手动复制: ' + text)
        this.closeMenu()
      }
    } catch (err) {
      console.error('Fallback: 复制失败', err)
      alert('复制失败，请手动复制: ' + text)
      this.closeMenu()
    }

    document.body.removeChild(textArea)
  }

  closeMenu() {
    this.menuTarget.style.display = 'none'
    document.removeEventListener('click', this.boundCloseOnOutsideClick)
  }

  showCopySuccess(target) {
    if (!target) return
    // 成功反馈期间忽略重复点击，避免把"已复制"当作 originalHTML 恢复
    if (target.dataset.copying === "true") return

    target.dataset.copying = "true"
    const originalHTML = target.innerHTML
    target.innerHTML = '<i class="fas fa-check"></i> 已复制'
    target.style.color = '#4caf50'

    setTimeout(() => {
      target.innerHTML = originalHTML
      target.style.color = ''
      delete target.dataset.copying
      // 延迟关闭菜单，让用户看到复制成功的反馈
      this.closeMenu()
    }, 800)
  }
}
