import { Controller } from "@hotwired/stimulus"

export default class extends Controller {
  static values = { commentId: Number }

  show() {
    const formContainer = this.formContainer()
    if (formContainer) {
      formContainer.style.display = 'block'
      formContainer.scrollIntoView({ behavior: 'smooth', block: 'nearest' })
      const textarea = formContainer.querySelector('textarea')
      if (textarea) {
        setTimeout(() => textarea.focus(), 100)
      }
    }
  }

  hide() {
    const formContainer = this.formContainer()
    if (formContainer) {
      formContainer.style.display = 'none'
      const form = formContainer.querySelector('form')
      if (form) {
        form.reset()
      }
    }
  }

  formContainer() {
    return document.getElementById('reply-form-' + this.commentIdValue)
  }
}
