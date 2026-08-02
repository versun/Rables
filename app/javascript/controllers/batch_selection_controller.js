import { Controller } from "@hotwired/stimulus"

// Connects to data-controller="batch-selection"
export default class extends Controller {
    static targets = ["checkbox", "selectAll", "actions", "count"]

    connect() {
        this.updateUI()
    }

    toggleAll(event) {
        const checked = event.target.checked
        this.checkboxTargets.forEach(checkbox => {
            checkbox.checked = checked
        })
        this.updateUI()
    }

    toggle() {
        this.updateUI()
    }

    updateUI() {
        const checkedBoxes = this.checkboxTargets.filter(cb => cb.checked)
        const hasSelection = checkedBoxes.length > 0

        // Show/hide batch actions
        if (this.hasActionsTarget) {
            this.actionsTarget.style.display = hasSelection ? 'flex' : 'none'
        }

        // Update select all checkbox state
        if (this.hasSelectAllTarget) {
            const allChecked = this.checkboxTargets.length > 0 &&
                checkedBoxes.length === this.checkboxTargets.length
            this.selectAllTarget.checked = allChecked
            this.selectAllTarget.indeterminate = checkedBoxes.length > 0 && !allChecked
        }

        // Update count display
        if (this.hasCountTarget) {
            this.countTarget.textContent = checkedBoxes.length
        }
    }

    getSelectedIds() {
        return this.checkboxTargets
            .filter(cb => cb.checked)
            .map(cb => cb.value)
    }

    submitBatchAction(event) {
        const form = event.params.formId
            ? document.getElementById(event.params.formId)
            : event.currentTarget.closest('form')

        if (!form) {
            event.preventDefault()
            return false
        }

        const selectedIds = this.getSelectedIds()

        if (selectedIds.length === 0) {
            event.preventDefault()
            alert('Please select at least one item.')
            return false
        }

        this.appendSelectedIds(form, selectedIds)

        // Confirmation for destructive actions
        const action = event.params.action
        if (action && (action.includes('delete') || action.includes('destroy'))) {
            if (!confirm(`Are you sure you want to delete ${selectedIds.length} item(s)?`)) {
                event.preventDefault()
                return false
            }
        }

        return true
    }

    openModal(event) {
        if (this.getSelectedIds().length === 0) {
            alert('请至少选择一个文章。')
            return
        }

        const modal = document.getElementById(event.params.modalId)
        // Reset checkboxes (e.g. crosspost platform selection)
        modal.querySelectorAll('input[type="checkbox"]').forEach(cb => cb.checked = false)
        modal.style.display = 'block'
    }

    closeModal(event) {
        this.hideModal(document.getElementById(event.params.modalId))
    }

    closeOnBackdropClick(event) {
        if (event.target === event.currentTarget) {
            this.hideModal(event.currentTarget)
        }
    }

    hideModal(modal) {
        modal.style.display = 'none'
        modal.querySelectorAll('input[type="text"]').forEach(input => input.value = '')
        modal.querySelectorAll('input[type="checkbox"]').forEach(cb => cb.checked = false)
    }

    appendSelectedIds(form, selectedIds) {
        form.querySelectorAll('input[name="ids[]"]').forEach(input => input.remove())

        selectedIds.forEach(id => {
            const input = document.createElement('input')
            input.type = 'hidden'
            input.name = 'ids[]'
            input.value = id
            form.appendChild(input)
        })
    }
}
