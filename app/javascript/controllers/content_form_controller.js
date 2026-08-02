import { Controller } from "@hotwired/stimulus"

// Connects to data-controller="content-form"
// 从 admin/articles 与 admin/pages 表单内联 <script> 迁移而来：
// - 定时发布：status 切换为 schedule 时显示 scheduled_at 字段
// - content_type 切换 rich text / HTML 编辑器的显隐与 required 属性
// - 提交前做内容非空验证
// data-content-form-param-value 参数化模型名（article / page），
// 用于定位 lexxy-editor 元素与 html_content textarea。
export default class extends Controller {
  static targets = [
    "scheduledAt",
    "scheduledAtHint",
    "contentTypeSelect",
    "richTextField",
    "htmlContentField"
  ]
  static values = { param: String }

  connect() {
    this.toggleContentType()
  }

  toggleScheduledAt(event) {
    const isSchedule = event.target.value === "schedule"
    this.scheduledAtTarget.style.display = isSchedule ? "block" : "none"
    if (this.hasScheduledAtHintTarget) {
      this.scheduledAtHintTarget.style.display = isSchedule ? "block" : "none"
    }
  }

  toggleContentType() {
    const isHtml = this.contentTypeSelectTarget.value === "html"
    this.richTextFieldTarget.style.display = isHtml ? "none" : "block"
    this.htmlContentFieldTarget.style.display = isHtml ? "block" : "none"

    // 更新HTML textarea的required属性
    const htmlTextArea = this.htmlContentFieldTarget.querySelector("textarea")
    if (htmlTextArea) {
      if (isHtml) {
        htmlTextArea.setAttribute("required", "required")
      } else {
        htmlTextArea.removeAttribute("required")
      }
    }
  }

  // Lexxy 空内容常是 "<p><br></p>" 这类标记，剥离 HTML 标签和 &nbsp; 后再判空
  richTextContentBlank(content) {
    const text = (content || "").replace(/<[^>]*>/g, "").replace(/&nbsp;/gi, " ").trim()
    return text.length === 0
  }

  // 表单提交前处理
  submit(event) {
    const isHtml = this.contentTypeSelectTarget.value === "html"
    const form = this.element

    if (!isHtml) {
      // Rich Text 模式：lexxy-editor 是 form-associated 自定义元素，
      // 其 value 属性即当前 HTML 内容（未升级的元素回退到初始 value attribute）
      const editor = form.querySelector(`lexxy-editor[name="${this.paramValue}[content]"]`)
      const content = editor ? (editor.value ?? editor.getAttribute("value") ?? "") : ""
      if (this.richTextContentBlank(content)) {
        event.preventDefault()
        alert("Content cannot be blank")
        return false
      }
    } else {
      // HTML 模式：检查 html_content
      const htmlTextarea = form.querySelector(`textarea[name="${this.paramValue}[html_content]"]`)
      if (!htmlTextarea || !htmlTextarea.value.trim()) {
        event.preventDefault()
        alert("HTML content cannot be blank")
        return false
      }
    }
  }
}
