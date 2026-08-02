import { Controller } from "@hotwired/stimulus"

// Connects to data-controller="content-form"
// 从 admin/articles 与 admin/pages 表单内联 <script> 迁移而来：
// - 定时发布：status 切换为 schedule 时显示 scheduled_at 字段
// - content_type 切换 rich text / HTML 编辑器的显隐与 required 属性
// - 提交前同步 TinyMCE 内容并做非空验证
// data-content-form-param-value 参数化模型名（article / page），
// 用于定位 tinymce.get("<param>_content") 与 textarea name 选择器。
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

  // TinyMCE 空内容常是 "<p>&nbsp;</p>" 这类标记，剥离 HTML 标签和 &nbsp; 后再判空
  richTextContentBlank(content) {
    const text = (content || "").replace(/<[^>]*>/g, "").replace(/&nbsp;/gi, " ").trim()
    return text.length === 0
  }

  // 表单提交前处理
  submit(event) {
    const isHtml = this.contentTypeSelectTarget.value === "html"
    const form = this.element

    // 同步 TinyMCE 内容到 textarea（如果不是 HTML 模式）
    if (!isHtml && typeof tinymce !== "undefined") {
      tinymce.triggerSave()
    }

    // 自定义验证：检查内容是否为空
    if (!isHtml) {
      // Rich Text 模式：检查 TinyMCE 内容
      let content = ""
      if (typeof tinymce !== "undefined") {
        const editor = tinymce.get(`${this.paramValue}_content`)
        if (editor) {
          content = editor.getContent().trim()
        }
      }
      // 如果 TinyMCE 未加载，检查 textarea 的值
      if (!content) {
        const textarea = form.querySelector(`textarea[name="${this.paramValue}[content]"]`)
        if (textarea && textarea.value.trim()) {
          content = textarea.value.trim()
        }
      }
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
