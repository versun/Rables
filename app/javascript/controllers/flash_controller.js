import { Controller } from "@hotwired/stimulus"

export default class extends Controller {
  connect() {
    this.fadeTimeout = setTimeout(() => {
      this.element.style.transition = "opacity 0.5s"
      this.element.style.opacity = "0"
      this.removeTimeout = setTimeout(() => this.element.remove(), 500)
    }, 5000)
  }

  disconnect() {
    clearTimeout(this.fadeTimeout)
    clearTimeout(this.removeTimeout)
  }
}
