import { Controller } from "@hotwired/stimulus"

// Connects to data-controller="crosspost-form"
export default class extends Controller {
    static targets = ["main", "verify"]

    // 同步主表单的值到 verify 表单
    sync() {
        const formData = new FormData(this.mainTarget)
        formData.forEach((value, key) => {
            if (key.startsWith('crosspost[')) {
                const fieldName = key.replace('crosspost[', '').replace(']', '')
                const verifyField = this.verifyTarget.querySelector(`[name="crosspost[${fieldName}]"]`)
                if (verifyField) {
                    verifyField.value = value
                }
            }
        })
    }
}
