import { Controller } from "@hotwired/stimulus"

// UX-only validator for the server-rendered math challenge. The authoritative
// check happens server-side against the HMAC-signed captcha[token] field, so
// this controller never generates a challenge itself.
export default class extends Controller {
  static targets = ["container", "question", "a", "b", "op", "answer", "message"]
  static values = { max: { type: Number, default: 10 } }

  connect() {
    this.hide()
  }

  show() {
    this.containerTarget.style.display = "block"
    this.answerTarget.required = true
    this.updateSubmitDisabled(!this.isValid())
  }

  hide() {
    if (this.hasContainerTarget) this.containerTarget.style.display = "none"
    if (this.hasAnswerTarget) this.answerTarget.required = false
    this.clearMessage()
  }

  validate() {
    const valid = this.isValid()

    if (this.answerTarget.value.trim() === "") {
      this.clearMessage()
      this.updateSubmitDisabled(true)
      return
    }

    if (valid) {
      this.showMessage("答案正确。", "success")
      this.updateSubmitDisabled(false)
    } else {
      this.showMessage("答案不正确，请重试。", "error")
      this.updateSubmitDisabled(true)
    }
  }

  ensureValid(event) {
    this.show()
    this.validate()

    if (!this.isValid()) {
      event.preventDefault()
    }
  }

  isValid() {
    const a = parseInt(this.aTarget.value, 10)
    const b = parseInt(this.bTarget.value, 10)
    const op = this.opTarget.value
    const answerRaw = this.answerTarget.value.trim()
    const answer = parseInt(answerRaw, 10)

    if (Number.isNaN(a) || Number.isNaN(b) || !["+", "-"].includes(op)) return false
    if (answerRaw === "" || Number.isNaN(answer)) return false

    const expected = op === "+" ? (a + b) : (a - b)
    return answer === expected
  }

  showMessage(text, type) {
    if (!this.hasMessageTarget) return

    this.messageTarget.textContent = text
    this.messageTarget.style.display = "block"
    this.messageTarget.style.color = type === "success" ? "#0a7a22" : "#b00020"
  }

  clearMessage() {
    if (!this.hasMessageTarget) return

    this.messageTarget.textContent = ""
    this.messageTarget.style.display = "none"
    this.messageTarget.style.color = ""
  }

  updateSubmitDisabled(disabled) {
    const form = this.element.querySelector("form") || this.element.closest("form")
    if (!form) return

    form.querySelectorAll("input[type='submit'], button[type='submit']").forEach((el) => {
      el.disabled = !!disabled
    })
  }
}
