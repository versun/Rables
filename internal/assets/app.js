/* Rables frontend — dependency-free vanilla JS port of the 14 Rails Stimulus
 * controllers (plan §9, sources: app/javascript/controllers/*.js).
 *
 * The Go templates carry the same data-controller / data-action /
 * data-<id>-target / data-<id>-<name>-value hooks as the Rails views, so this
 * file ships a tiny Stimulus-compatible subset runtime plus one controller
 * class per Rails counterpart. No MutationObserver: pages are server-rendered
 * MPAs and controllers attach once at DOMContentLoaded; DOM inserted later by
 * this file itself (lexxy editor) needs no controllers.
 */
(() => {
  "use strict";

  // --- tiny Stimulus-subset runtime --------------------------------------

  const registry = new Map(); // identifier -> Controller subclass
  const instances = [];       // live controller instances

  const camelize = (s) => s.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
  const dasherize = (s) => s.replace(/([A-Z])/g, (_, c) => "-" + c.toLowerCase());
  const capitalize = (s) => s.charAt(0).toUpperCase() + s.slice(1);

  class Controller {
    constructor(element, identifier) {
      this.element = element;
      this.identifier = identifier;
      this.application = app;
    }

    // Targets scoped like Stimulus: a [data-<id>-target] element belongs to
    // the closest ancestor-or-self controller with the same identifier.
    targetsAll(name) {
      const sel = `[data-${this.identifier}-target~="${name}"]`;
      const scopeSel = `[data-controller~="${this.identifier}"]`;
      const found = [];
      if (this.element.matches(sel)) found.push(this.element);
      this.element.querySelectorAll(sel).forEach((el) => {
        if (el.closest(scopeSel) === this.element) found.push(el);
      });
      return found;
    }

    target(name) {
      return this.targetsAll(name)[0] || null;
    }

    hasTarget(name) {
      return this.targetsAll(name).length > 0;
    }

    valueNames() {
      return Object.keys(this.constructor.values || {});
    }

    readValue(name, spec) {
      const attr = `data-${this.identifier}-${dasherize(name)}-value`;
      const has = this.element.hasAttribute(attr);
      const raw = this.element.getAttribute(attr);
      const type = typeof spec === "object" ? spec.type : spec || String;
      const fallback = typeof spec === "object" && "default" in spec ? spec.default : undefined;
      if (!has) return [fallback, false];
      if (type === Number) return [Number(raw), true];
      if (type === Boolean) return [raw === "true" || raw === "1", true];
      return [raw, true];
    }
  }

  function installAccessors(instance) {
    const klass = instance.constructor;
    (klass.targets || []).forEach((name) => {
      const cap = capitalize(name);
      Object.defineProperty(instance, `${name}Target`, {
        get() {
          const el = this.target(name);
          if (!el) throw new Error(`Missing target "${name}" for ${this.identifier}`);
          return el;
        },
      });
      Object.defineProperty(instance, `${name}Targets`, { get() { return this.targetsAll(name); } });
      Object.defineProperty(instance, `has${cap}Target`, { get() { return this.hasTarget(name); } });
    });
    instance.valueNames().forEach((name) => {
      const spec = klass.values[name];
      const cap = capitalize(name);
      Object.defineProperty(instance, `${name}Value`, {
        get() { return this.readValue(name, spec)[0]; },
      });
      Object.defineProperty(instance, `has${cap}Value`, {
        get() { return this.readValue(name, spec)[1]; },
      });
    });
  }

  // event.params from data-<id>-<name>-param attributes on the action element.
  function actionParams(element, identifier) {
    const prefix = camelize(identifier);
    const params = {};
    for (const key of Object.keys(element.dataset)) {
      if (key.startsWith(prefix) && key.endsWith("Param")) {
        const middle = key.slice(prefix.length, -"Param".length);
        if (middle) params[middle.charAt(0).toLowerCase() + middle.slice(1)] = element.dataset[key];
      }
    }
    return params;
  }

  function bindActions() {
    document.querySelectorAll("[data-action]").forEach((el) => {
      el.getAttribute("data-action").split(/\s+/).forEach((descriptor) => {
        const m = descriptor.match(/^([a-z]+)->([\w-]+)#(\w+)$/);
        if (!m || !registry.has(m[2])) return;
        const [, eventName, identifier, method] = m;
        const scope = el.closest(`[data-controller~="${identifier}"]`);
        if (!scope) return;
        const instance = instances.find((i) => i.element === scope && i.identifier === identifier);
        if (!instance || typeof instance[method] !== "function") return;
        el.addEventListener(eventName, (event) => {
          event.params = actionParams(el, identifier);
          instance[method](event);
        });
      });
    });
  }

  const app = {
    getControllerForElementAndIdentifier(element, identifier) {
      return instances.find((i) => i.element === element && i.identifier === identifier) || null;
    },
  };

  function boot() {
    document.querySelectorAll("[data-controller]").forEach((el) => {
      el.getAttribute("data-controller").split(/\s+/).forEach((identifier) => {
        const klass = registry.get(identifier);
        if (!klass) return;
        instances.push(new klass(el, identifier));
      });
    });
    instances.forEach((instance) => installAccessors(instance));
    bindActions();
    instances.forEach((instance) => instance.connect && instance.connect());
  }

  const csrfToken = () => document.querySelector('meta[name="csrf-token"]')?.content || "";

  // --- flash (fade out after 5s) ------------------------------------------
  // Mirrors flash_controller.js. Applied to every .flash element directly:
  // the Go layout renders class-only hooks (see plan 决策记录 2026-08-03).

  function flashFade(element) {
    setTimeout(() => {
      element.style.transition = "opacity 0.5s";
      element.style.opacity = "0";
      setTimeout(() => element.remove(), 500);
    }, 5000);
  }

  // --- batch_selection -----------------------------------------------------

  class BatchSelectionController extends Controller {
    static targets = ["checkbox", "selectAll", "actions", "count"];

    connect() {
      this.updateUI();
    }

    toggleAll(event) {
      const checked = event.target.checked;
      this.checkboxTargets.forEach((checkbox) => {
        checkbox.checked = checked;
      });
      this.updateUI();
    }

    toggle() {
      this.updateUI();
    }

    updateUI() {
      const checkedBoxes = this.checkboxTargets.filter((cb) => cb.checked);
      const hasSelection = checkedBoxes.length > 0;

      if (this.hasActionsTarget) {
        this.actionsTarget.style.display = hasSelection ? "flex" : "none";
      }

      if (this.hasSelectAllTarget) {
        const allChecked = this.checkboxTargets.length > 0 &&
          checkedBoxes.length === this.checkboxTargets.length;
        this.selectAllTarget.checked = allChecked;
        this.selectAllTarget.indeterminate = checkedBoxes.length > 0 && !allChecked;
      }

      if (this.hasCountTarget) {
        this.countTarget.textContent = checkedBoxes.length;
      }
    }

    getSelectedIds() {
      return this.checkboxTargets.filter((cb) => cb.checked).map((cb) => cb.value);
    }

    submitBatchAction(event) {
      const form = event.params.formId
        ? document.getElementById(event.params.formId)
        : event.currentTarget.closest("form");

      if (!form) {
        event.preventDefault();
        return false;
      }

      const selectedIds = this.getSelectedIds();

      if (selectedIds.length === 0) {
        event.preventDefault();
        alert("Please select at least one item.");
        return false;
      }

      this.appendSelectedIds(form, selectedIds);

      const action = event.params.action;
      if (action && (action.includes("delete") || action.includes("destroy"))) {
        if (!confirm(`Are you sure you want to delete ${selectedIds.length} item(s)?`)) {
          event.preventDefault();
          return false;
        }
      }

      return true;
    }

    openModal(event) {
      if (this.getSelectedIds().length === 0) {
        alert("请至少选择一个文章。");
        return;
      }

      const modal = document.getElementById(event.params.modalId);
      modal.querySelectorAll('input[type="checkbox"]').forEach((cb) => (cb.checked = false));
      modal.style.display = "block";
    }

    closeModal(event) {
      this.hideModal(document.getElementById(event.params.modalId));
    }

    closeOnBackdropClick(event) {
      if (event.target === event.currentTarget) {
        this.hideModal(event.currentTarget);
      }
    }

    hideModal(modal) {
      modal.style.display = "none";
      modal.querySelectorAll('input[type="text"]').forEach((input) => (input.value = ""));
      modal.querySelectorAll('input[type="checkbox"]').forEach((cb) => (cb.checked = false));
    }

    appendSelectedIds(form, selectedIds) {
      form.querySelectorAll('input[name="ids[]"]').forEach((input) => input.remove());

      selectedIds.forEach((id) => {
        const input = document.createElement("input");
        input.type = "hidden";
        input.name = "ids[]";
        input.value = id;
        form.appendChild(input);
      });
    }
  }

  // --- content_form ---------------------------------------------------------
  // Mirrors content_form_controller.js (schedule toggle, editor-mode toggle,
  // non-blank validation). The Go form fields are flat-named (content /
  // markdown_content / html_content), so lookups go through the targets
  // instead of a model param.
  // Rich-text mode progressively upgrades the plain textarea to the vendored
  // <lexxy-editor> (same form-associated custom element Rails renders); if the
  // module fails to load the textarea stays and both modes still work.
  // Markdown mode likewise upgrades its textarea to the vendored EasyMDE;
  // forceSync keeps the textarea current so the plain form POST is unchanged.
  // The pages form shares this controller but has no markdown target, so every
  // markdown path is guarded by hasMarkdownContentFieldTarget.

  let lexxyLoading = null;

  function ensureLexxyCSS() {
    if (document.querySelector('link[href="/assets/lexxy.css"]')) return;
    const link = document.createElement("link");
    link.rel = "stylesheet";
    link.href = "/assets/lexxy.css";
    document.head.appendChild(link);
  }

  function loadLexxy() {
    if (customElements.get("lexxy-editor")) return Promise.resolve(true);
    if (!lexxyLoading) {
      lexxyLoading = import("/assets/lexxy.min.js")
        .then(() => customElements.get("lexxy-editor") != null)
        .catch(() => false);
    }
    return lexxyLoading;
  }

  let easymdeLoading = null;

  function ensureEasyMDECSS() {
    if (document.querySelector('link[href="/assets/easymde.min.css"]')) return;
    const link = document.createElement("link");
    link.rel = "stylesheet";
    link.href = "/assets/easymde.min.css";
    document.head.appendChild(link);
  }

  function loadEasyMDE() {
    if (window.EasyMDE) return Promise.resolve(true);
    if (!easymdeLoading) {
      easymdeLoading = import("/assets/easymde.min.js")
        .then(() => window.EasyMDE != null)
        .catch(() => false);
    }
    return easymdeLoading;
  }

  class ContentFormController extends Controller {
    static targets = [
      "scheduledAt",
      "scheduledAtHint",
      "contentTypeSelect",
      "richTextField",
      "markdownContentField",
      "htmlContentField",
    ];

    connect() {
      this.toggleContentType();
      this.upgradeRichText();
      this.upgradeMarkdown();
    }

    toggleScheduledAt(event) {
      const isSchedule = event.target.value === "schedule";
      this.scheduledAtTarget.style.display = isSchedule ? "block" : "none";
      if (this.hasScheduledAtHintTarget) {
        this.scheduledAtHintTarget.style.display = isSchedule ? "block" : "none";
      }
    }

    toggleContentType() {
      const mode = this.contentTypeSelectTarget.value;
      this.richTextFieldTarget.style.display = mode === "rich_text" ? "block" : "none";
      this.htmlContentFieldTarget.style.display = mode === "html" ? "block" : "none";
      if (this.hasMarkdownContentFieldTarget) {
        this.markdownContentFieldTarget.style.display = mode === "markdown" ? "block" : "none";
        if (mode === "markdown") this.upgradeMarkdown();
      }

      const htmlTextArea = this.htmlContentFieldTarget.querySelector("textarea");
      if (htmlTextArea) {
        if (mode === "html") {
          htmlTextArea.setAttribute("required", "required");
        } else {
          htmlTextArea.removeAttribute("required");
        }
      }
    }

    upgradeRichText() {
      const field = this.richTextFieldTarget;
      const textarea = field.querySelector("textarea");
      if (!textarea) return;
      loadLexxy().then((ok) => {
        if (!ok || !field.isConnected || !field.contains(textarea)) return;
        ensureLexxyCSS();
        const editor = document.createElement("lexxy-editor");
        editor.setAttribute("name", textarea.getAttribute("name") || "content");
        editor.setAttribute("value", textarea.value);
        editor.className = "lexxy-content";
        textarea.replaceWith(editor);
      });
    }

    upgradeMarkdown() {
      if (!this.hasMarkdownContentFieldTarget || this.easymde) return;
      const textarea = this.markdownContentFieldTarget.querySelector("textarea");
      if (!textarea) return;
      loadEasyMDE().then((ok) => {
        if (!ok || this.easymde || !textarea.isConnected) return;
        ensureEasyMDECSS();
        this.easymde = new window.EasyMDE({
          element: textarea,
          forceSync: true, // keep the textarea current for the plain form POST
          spellChecker: false,
        });
      });
    }

    // Lexxy empty content is markup like "<p><br></p>": strip tags and &nbsp;
    richTextContentBlank(content) {
      const text = (content || "").replace(/<[^>]*>/g, "").replace(/&nbsp;/gi, " ").trim();
      return text.length === 0;
    }

    submit(event) {
      const mode = this.contentTypeSelectTarget.value;

      if (mode === "markdown") {
        const textarea = this.hasMarkdownContentFieldTarget
          ? this.markdownContentFieldTarget.querySelector("textarea")
          : null;
        const content = this.easymde ? this.easymde.value() : textarea ? textarea.value : "";
        if (!content.trim()) {
          event.preventDefault();
          alert("Content cannot be blank");
          return false;
        }
      } else if (mode !== "html") {
        const editor = this.richTextFieldTarget.querySelector('lexxy-editor, textarea');
        const content = editor ? (editor.value ?? editor.getAttribute("value") ?? "") : "";
        if (this.richTextContentBlank(content)) {
          event.preventDefault();
          alert("Content cannot be blank");
          return false;
        }
      } else {
        const htmlTextarea = this.htmlContentFieldTarget.querySelector("textarea");
        if (!htmlTextarea || !htmlTextarea.value.trim()) {
          event.preventDefault();
          alert("HTML content cannot be blank");
          return false;
        }
      }
    }
  }

  // --- crosspost_form ---------------------------------------------------------

  class CrosspostFormController extends Controller {
    static targets = ["main", "verify"];

    // Sync the main form's values into the verify form.
    sync() {
      const formData = new FormData(this.mainTarget);
      formData.forEach((value, key) => {
        if (key.startsWith("crosspost[")) {
          const fieldName = key.replace("crosspost[", "").replace("]", "");
          const verifyField = this.verifyTarget.querySelector(`[name="crosspost[${fieldName}]"]`);
          if (verifyField) {
            verifyField.value = value;
          }
        }
      });
    }
  }

  // --- fetch_comments ---------------------------------------------------------

  class FetchCommentsController extends Controller {
    static targets = ["button", "icon"];
    static values = { platform: String };

    connect() {
      if (this.hasIconTarget) {
        this.originalIconClass = this.iconTarget.className;
        this.iconIsImage = this.iconTarget.tagName === "IMG";
      }
    }

    async submit(event) {
      event.preventDefault();
      event.stopPropagation();

      const form = this.element;
      const button = this.buttonTarget;
      const icon = this.hasIconTarget ? this.iconTarget : null;
      const formData = new FormData(form);
      const url = form.action;

      if (button) {
        button.disabled = true;
      }
      if (icon) {
        if (!this.originalIconClass) {
          this.originalIconClass = icon.className;
        }
        if (this.iconIsImage) {
          icon.classList.add("is-loading");
        } else {
          icon.className = "fas fa-spinner fa-spin";
        }
      }

      try {
        const response = await fetch(url, {
          method: "POST",
          body: formData,
          headers: {
            "Accept": "application/json",
            "X-Requested-With": "XMLHttpRequest",
            "X-CSRF-Token": csrfToken(),
          },
          credentials: "same-origin",
        });

        const data = await response.json();

        if (data.success) {
          alert(data.message);
        } else {
          alert(data.message || "Failed to fetch comments");
        }
      } catch (error) {
        console.error("Error fetching comments:", error);
        alert("An error occurred while fetching comments.");
      } finally {
        if (button) {
          button.disabled = false;
        }
        if (icon) {
          if (this.iconIsImage) {
            icon.classList.remove("is-loading");
          } else if (this.originalIconClass) {
            icon.className = this.originalIconClass;
          }
        }
      }
    }
  }

  // --- math_captcha -----------------------------------------------------------
  // UX-only validator for the server-rendered math challenge; the authoritative
  // check is server-side against the HMAC-signed captcha[token] field.

  class MathCaptchaController extends Controller {
    static targets = ["container", "question", "a", "b", "op", "answer", "message"];
    static values = { max: { type: Number, default: 10 } };

    connect() {
      this.hide();
    }

    show() {
      this.containerTarget.style.display = "block";
      this.answerTarget.required = true;
      this.updateSubmitDisabled(!this.isValid());
    }

    hide() {
      if (this.hasContainerTarget) this.containerTarget.style.display = "none";
      if (this.hasAnswerTarget) this.answerTarget.required = false;
      this.clearMessage();
    }

    validate() {
      const valid = this.isValid();

      if (this.answerTarget.value.trim() === "") {
        this.clearMessage();
        this.updateSubmitDisabled(true);
        return;
      }

      if (valid) {
        this.showMessage("答案正确。", "success");
        this.updateSubmitDisabled(false);
      } else {
        this.showMessage("答案不正确，请重试。", "error");
        this.updateSubmitDisabled(true);
      }
    }

    ensureValid(event) {
      this.show();
      this.validate();

      if (!this.isValid()) {
        event.preventDefault();
      }
    }

    isValid() {
      const a = parseInt(this.aTarget.value, 10);
      const b = parseInt(this.bTarget.value, 10);
      const op = this.opTarget.value;
      const answerRaw = this.answerTarget.value.trim();
      const answer = parseInt(answerRaw, 10);

      if (Number.isNaN(a) || Number.isNaN(b) || !["+", "-"].includes(op)) return false;
      if (answerRaw === "" || Number.isNaN(answer)) return false;

      const expected = op === "+" ? a + b : a - b;
      return answer === expected;
    }

    showMessage(text, type) {
      if (!this.hasMessageTarget) return;

      this.messageTarget.textContent = text;
      this.messageTarget.style.display = "block";
      this.messageTarget.style.color = type === "success" ? "#0a7a22" : "#b00020";
    }

    clearMessage() {
      if (!this.hasMessageTarget) return;

      this.messageTarget.textContent = "";
      this.messageTarget.style.display = "none";
      this.messageTarget.style.color = "";
    }

    updateSubmitDisabled(disabled) {
      const form = this.element.querySelector("form") || this.element.closest("form");
      if (!form) return;

      form.querySelectorAll("input[type='submit'], button[type='submit']").forEach((el) => {
        el.disabled = !!disabled;
      });
    }
  }

  // --- newsletter (admin settings) ---------------------------------------------

  class NewsletterController extends Controller {
    static targets = ["providerSelect", "nativeSettings", "listmonkSettings", "verifyBtn", "verifyStatus", "listSelect", "templateSelect", "sesVerifyBtn", "sesVerifyStatus"];
    static values = { verifyUrl: String, activeTab: String };

    connect() {
      this.updateSettingsVisibility();
    }

    providerChanged() {
      this.updateSettingsVisibility();
    }

    async verify() {
      const verifyBtn = this.verifyBtnTarget;
      const verifyStatus = this.verifyStatusTarget;
      const listSelect = this.listSelectTarget;
      const templateSelect = this.templateSelectTarget;

      verifyStatus.textContent = "Verifying...";
      verifyStatus.style.color = "gray";
      verifyBtn.disabled = true;

      const formData = {
        url: document.getElementById("listmonk_url").value,
        username: document.getElementById("listmonk_username").value,
        api_key: document.getElementById("listmonk_api_key").value,
        list_id: listSelect.value,
        template_id: templateSelect.value,
      };

      try {
        const response = await fetch(this.verifyUrlValue, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(formData),
        });

        const data = await response.json();

        if (data.success) {
          verifyStatus.textContent = "✓ Verification successful!";
          verifyStatus.style.color = "green";

          listSelect.innerHTML = '<option value="">Select a List</option>';
          data.lists.forEach((list) => {
            const option = document.createElement("option");
            option.value = list.id;
            option.textContent = list.name;
            // option.value and current_list_id differ in type (number vs string)
            if (String(list.id) === String(data.current_list_id)) {
              option.selected = true;
            }
            listSelect.appendChild(option);
          });

          templateSelect.innerHTML = '<option value="">Select a Template</option>';
          data.templates.forEach((template) => {
            const option = document.createElement("option");
            option.value = template.id;
            option.textContent = template.name;
            if (String(template.id) === String(data.current_template_id)) {
              option.selected = true;
            }
            templateSelect.appendChild(option);
          });
        } else {
          verifyStatus.textContent = "✗ " + (data.error || "Verification failed");
          verifyStatus.style.color = "red";
        }
      } catch (error) {
        verifyStatus.textContent = "✗ Error: " + error.message;
        verifyStatus.style.color = "red";
      } finally {
        verifyBtn.disabled = false;
      }
    }

    async verifySes() {
      const verifyBtn = this.sesVerifyBtnTarget;
      const verifyStatus = this.sesVerifyStatusTarget;

      verifyStatus.textContent = "Verifying...";
      verifyStatus.style.color = "gray";
      verifyBtn.disabled = true;

      const formData = {
        smtp_address: document.getElementById("newsletter_setting_smtp_address").value,
        smtp_port: document.getElementById("newsletter_setting_smtp_port").value,
        smtp_user_name: document.getElementById("newsletter_setting_smtp_user_name").value,
        smtp_password: document.getElementById("newsletter_setting_smtp_password").value,
        smtp_domain: document.getElementById("newsletter_setting_smtp_domain").value,
        smtp_authentication: document.getElementById("newsletter_setting_smtp_authentication").value,
        smtp_enable_starttls: document.getElementById("newsletter_setting_smtp_enable_starttls").checked ? "1" : "0",
        from_email: document.getElementById("newsletter_setting_from_email").value,
      };

      try {
        const response = await fetch(this.verifyUrlValue, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(formData),
        });

        const data = await response.json();

        if (data.success) {
          verifyStatus.textContent = "✓ " + (data.message || "Verification successful!");
          verifyStatus.style.color = "green";
        } else {
          verifyStatus.textContent = "✗ " + (data.error || "Verification failed");
          verifyStatus.style.color = "red";
        }
      } catch (error) {
        verifyStatus.textContent = "✗ Error: " + error.message;
        verifyStatus.style.color = "red";
      } finally {
        verifyBtn.disabled = false;
      }
    }

    updateSettingsVisibility() {
      if (this.hasActiveTabValue && this.activeTabValue) {
        if (this.hasNativeSettingsTarget) {
          this.nativeSettingsTarget.style.display = this.activeTabValue === "native" ? "block" : "none";
        }
        if (this.hasListmonkSettingsTarget) {
          this.listmonkSettingsTarget.style.display = this.activeTabValue === "listmonk" ? "block" : "none";
        }
        return;
      }

      if (!this.hasProviderSelectTarget) return;
      const provider = this.providerSelectTarget.value;

      if (this.hasNativeSettingsTarget) {
        this.nativeSettingsTarget.style.display = provider === "native" ? "block" : "none";
      }
      if (this.hasListmonkSettingsTarget) {
        this.listmonkSettingsTarget.style.display = provider === "native" ? "none" : "block";
      }
    }
  }

  // --- newsletter_subscription --------------------------------------------------

  class NewsletterSubscriptionController extends Controller {
    static targets = ["form", "emailInput", "submitBtn", "message"];

    async submit(event) {
      event.preventDefault();

      const captchaController = this.application.getControllerForElementAndIdentifier(this.element, "math-captcha");
      if (captchaController) {
        captchaController.show();
        captchaController.validate();
        if (!captchaController.isValid()) return;
      }

      const email = this.emailInputTarget.value.trim();

      if (!email) {
        this.showMessage("请输入有效的邮箱地址。", "error");
        return;
      }

      const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
      if (!emailRegex.test(email)) {
        this.showMessage("请输入有效的邮箱地址。", "error");
        return;
      }

      this.submitBtnTarget.disabled = true;
      this.submitBtnTarget.textContent = "提交中...";

      try {
        const formData = new FormData(this.formTarget);
        const response = await fetch(this.formTarget.action, {
          method: "POST",
          headers: { "Accept": "application/json" },
          body: formData,
        });

        const data = await response.json();

        if (response.ok && data.success) {
          this.showMessage(data.message || "订阅成功！请检查您的邮箱并点击确认链接。", "success");
          this.emailInputTarget.value = "";
        } else {
          this.showMessage(data.message || "订阅失败，请稍后重试。", "error");
        }
      } catch (error) {
        this.showMessage("网络错误，请稍后重试。", "error");
      } finally {
        this.submitBtnTarget.disabled = false;
        this.submitBtnTarget.textContent = "订阅";
      }
    }

    showMessage(text, type) {
      const messageEl = this.messageTarget;
      messageEl.textContent = text;
      messageEl.className = `newsletter-message newsletter-message-${type}`;
      messageEl.style.display = "block";

      if (type === "success") {
        setTimeout(() => {
          messageEl.style.opacity = "0";
          setTimeout(() => {
            messageEl.style.display = "none";
            messageEl.style.opacity = "1";
          }, 300);
        }, 3000);
      }
    }
  }

  // --- password_toggle ----------------------------------------------------------

  class PasswordToggleController extends Controller {
    static targets = ["input", "toggleButton"];

    connect() {
      this.updateToggleButton();
    }

    toggle() {
      const input = this.inputTarget;
      const type = input.getAttribute("type");

      if (type === "password") {
        input.setAttribute("type", "text");
        this.toggleButtonTarget.textContent = "Hide";
        this.toggleButtonTarget.setAttribute("aria-label", "隐藏密码");
      } else {
        input.setAttribute("type", "password");
        this.toggleButtonTarget.textContent = "Show";
        this.toggleButtonTarget.setAttribute("aria-label", "显示密码");
      }
    }

    updateToggleButton() {
      if (this.hasToggleButtonTarget) {
        const input = this.inputTarget;
        const type = input.getAttribute("type");
        if (type === "password") {
          this.toggleButtonTarget.textContent = "Show";
          this.toggleButtonTarget.setAttribute("aria-label", "显示密码");
        } else {
          this.toggleButtonTarget.textContent = "Hide";
          this.toggleButtonTarget.setAttribute("aria-label", "隐藏密码");
        }
      }
    }
  }

  // --- reply_form ----------------------------------------------------------------

  class ReplyFormController extends Controller {
    static values = { commentId: Number };

    show() {
      const formContainer = this.formContainer();
      if (formContainer) {
        formContainer.style.display = "block";
        formContainer.scrollIntoView({ behavior: "smooth", block: "nearest" });
        const textarea = formContainer.querySelector("textarea");
        if (textarea) {
          setTimeout(() => textarea.focus(), 100);
        }
      }
    }

    hide() {
      const formContainer = this.formContainer();
      if (formContainer) {
        formContainer.style.display = "none";
        const form = formContainer.querySelector("form");
        if (form) {
          form.reset();
        }
      }
    }

    formContainer() {
      return document.getElementById("reply-form-" + this.commentIdValue);
    }
  }

  // --- share ---------------------------------------------------------------------

  class ShareController extends Controller {
    static targets = ["menu"];
    static values = { url: String };

    connect() {
      this.boundCloseOnOutsideClick = this.closeOnOutsideClick.bind(this);
    }

    toggle(event) {
      event.stopPropagation();

      const isVisible = this.menuTarget.style.display === "block";

      document.querySelectorAll('[data-share-target="menu"]').forEach((menu) => {
        if (menu !== this.menuTarget) {
          menu.style.display = "none";
        }
      });

      this.menuTarget.style.display = isVisible ? "none" : "block";

      if (!isVisible) {
        setTimeout(() => {
          document.addEventListener("click", this.boundCloseOnOutsideClick);
        }, 0);
      } else {
        document.removeEventListener("click", this.boundCloseOnOutsideClick);
      }
    }

    close() {
      this.closeMenu();
    }

    closeOnOutsideClick(event) {
      if (!this.element.contains(event.target)) {
        this.closeMenu();
      }
    }

    copy(event) {
      event.preventDefault();
      event.stopPropagation();

      const url = this.urlValue;
      const target = event.currentTarget;

      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(url).then(() => {
          this.showCopySuccess(target);
        }).catch((err) => {
          console.error("复制失败:", err);
          this.fallbackCopy(url, target);
        });
      } else {
        this.fallbackCopy(url, target);
      }
    }

    fallbackCopy(text, target) {
      const textArea = document.createElement("textarea");
      textArea.value = text;
      textArea.style.position = "fixed";
      textArea.style.left = "-999999px";
      document.body.appendChild(textArea);
      textArea.focus();
      textArea.select();

      try {
        const successful = document.execCommand("copy");
        if (successful) {
          this.showCopySuccess(target);
        } else {
          alert("复制失败，请手动复制: " + text);
          this.closeMenu();
        }
      } catch (err) {
        console.error("Fallback: 复制失败", err);
        alert("复制失败，请手动复制: " + text);
        this.closeMenu();
      }

      document.body.removeChild(textArea);
    }

    closeMenu() {
      this.menuTarget.style.display = "none";
      document.removeEventListener("click", this.boundCloseOnOutsideClick);
    }

    showCopySuccess(target) {
      if (!target) return;
      // Ignore repeat clicks while the success feedback is showing.
      if (target.dataset.copying === "true") return;

      target.dataset.copying = "true";
      const originalHTML = target.innerHTML;
      target.innerHTML = '<i class="fas fa-check"></i> 已复制';
      target.style.color = "#4caf50";

      setTimeout(() => {
        target.innerHTML = originalHTML;
        target.style.color = "";
        delete target.dataset.copying;
        this.closeMenu();
      }, 800);
    }
  }

  // --- sidebar -------------------------------------------------------------------

  class SidebarController extends Controller {
    static targets = ["sidebar", "overlay", "toggle"];

    connect() {
      this.boundClose = this.close.bind(this);

      if (this.hasOverlayTarget) {
        this.overlayTarget.addEventListener("click", this.boundClose);
      }

      // Close the menu after tapping a nav link (mobile only).
      if (this.hasSidebarTarget) {
        this.sidebarTarget.querySelectorAll("a.nav-link").forEach((link) => {
          link.addEventListener("click", () => {
            if (window.innerWidth < 768) {
              this.close();
            }
          });
        });
      }

      this.syncAria();
    }

    toggle() {
      if (this.isOpen()) {
        this.close();
      } else {
        this.open();
      }
    }

    open() {
      if (this.hasSidebarTarget) {
        this.sidebarTarget.classList.add("sidebar-open");
        document.body.style.overflow = "hidden";
      }
      if (this.hasOverlayTarget) {
        this.overlayTarget.classList.add("overlay-visible");
      }
      this.syncAria();
    }

    close() {
      if (this.hasSidebarTarget) {
        this.sidebarTarget.classList.remove("sidebar-open");
        document.body.style.overflow = "";
      }
      if (this.hasOverlayTarget) {
        this.overlayTarget.classList.remove("overlay-visible");
      }
      this.syncAria();
    }

    isOpen() {
      return this.hasSidebarTarget && this.sidebarTarget.classList.contains("sidebar-open");
    }

    syncAria() {
      if (!this.hasToggleTarget) return;

      const expanded = this.isOpen();
      this.toggleTargets.forEach((toggle) => {
        toggle.setAttribute("aria-expanded", expanded ? "true" : "false");
      });
    }
  }

  // --- source_reference ------------------------------------------------------------
  // Wired to POST /admin/sources/fetch_twitter via the hooks in
  // _article_form.html (ported verbatim from the Stimulus controller).

  class SourceReferenceController extends Controller {
    static targets = ["url", "fetchBtn", "author", "content", "status"];

    async fetchTwitter(event) {
      event.preventDefault();
      event.stopPropagation();

      const url = this.urlTarget.value.trim();

      if (!url) {
        this.showStatus("Please enter a Source URL first", "error");
        return;
      }

      if (!this.isTwitterUrl(url)) {
        this.showStatus("Not a Twitter/X URL", "error");
        return;
      }

      this.setLoading(this.fetchBtnTarget, true);
      this.showStatus("Fetching tweet content...", "info");

      try {
        const response = await fetch("/admin/sources/fetch_twitter", {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "Accept": "application/json",
          },
          body: JSON.stringify({ url: url }),
        });

        const data = await response.json();

        if (response.ok && data.success) {
          if (this.hasAuthorTarget && data.author) {
            this.authorTarget.value = data.author;
          }
          if (this.hasContentTarget && data.content) {
            this.contentTarget.value = data.content;
          }
          this.showStatus("Tweet content fetched!", "success");
        } else {
          this.showStatus(data.error || "Failed to fetch tweet", "error");
        }
      } catch (error) {
        console.error("Fetch error:", error);
        this.showStatus(`Network error: ${error.message}`, "error");
      } finally {
        this.setLoading(this.fetchBtnTarget, false);
      }
    }

    isTwitterUrl(url) {
      try {
        const uri = new URL(url);
        const host = uri.hostname.toLowerCase();
        return ["twitter.com", "www.twitter.com", "x.com", "www.x.com"].includes(host);
      } catch (e) {
        return false;
      }
    }

    setLoading(button, loading) {
      if (loading) {
        button.disabled = true;
        if (!button.dataset.originalText) {
          button.dataset.originalText = button.innerHTML;
        }
        button.innerHTML = '<i class="fas fa-spinner fa-spin"></i> Loading...';
      } else {
        button.disabled = false;
        if (button.dataset.originalText) {
          button.innerHTML = button.dataset.originalText;
        }
      }
    }

    showStatus(message, type) {
      const statusEl = this.statusTarget;
      statusEl.textContent = message;
      statusEl.style.display = "block";

      switch (type) {
        case "success":
          statusEl.style.backgroundColor = "#d4edda";
          statusEl.style.color = "#155724";
          statusEl.style.border = "1px solid #c3e6cb";
          break;
        case "error":
          statusEl.style.backgroundColor = "#f8d7da";
          statusEl.style.color = "#721c24";
          statusEl.style.border = "1px solid #f5c6cb";
          break;
        case "info":
        default:
          statusEl.style.backgroundColor = "#d1ecf1";
          statusEl.style.color = "#0c5460";
          statusEl.style.border = "1px solid #bee5eb";
          break;
      }

      if (type === "success") {
        setTimeout(() => {
          statusEl.style.display = "none";
        }, 5000);
      }
    }
  }

  // --- theme_toggle ----------------------------------------------------------------

  class ThemeToggleController extends Controller {
    static targets = ["icon"];

    connect() {
      const savedTheme = localStorage.getItem("theme");
      const currentTheme = document.documentElement.getAttribute("data-theme");

      if (savedTheme) {
        this.applyTheme(savedTheme);
      } else if (!currentTheme) {
        const prefersDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
        this.applyTheme(prefersDark ? "dark" : "light");
      } else {
        this.updateIcon(currentTheme);
      }
    }

    toggle() {
      const currentTheme = document.documentElement.getAttribute("data-theme");
      const newTheme = currentTheme === "dark" ? "light" : "dark";
      this.applyTheme(newTheme);
      localStorage.setItem("theme", newTheme);
    }

    applyTheme(theme) {
      document.documentElement.setAttribute("data-theme", theme);
      this.updateIcon(theme);
    }

    updateIcon(theme) {
      if (this.hasIconTarget) {
        if (theme === "dark") {
          this.iconTarget.classList.remove("fa-moon");
          this.iconTarget.classList.add("fa-sun");
        } else {
          this.iconTarget.classList.remove("fa-sun");
          this.iconTarget.classList.add("fa-moon");
        }
      }
    }
  }

  // --- registration & boot -----------------------------------------------------------

  registry.set("batch-selection", BatchSelectionController);
  registry.set("content-form", ContentFormController);
  registry.set("crosspost-form", CrosspostFormController);
  registry.set("fetch-comments", FetchCommentsController);
  registry.set("math-captcha", MathCaptchaController);
  registry.set("newsletter", NewsletterController);
  registry.set("newsletter-subscription", NewsletterSubscriptionController);
  registry.set("password-toggle", PasswordToggleController);
  registry.set("reply-form", ReplyFormController);
  registry.set("share", ShareController);
  registry.set("sidebar", SidebarController);
  registry.set("source-reference", SourceReferenceController);
  registry.set("theme-toggle", ThemeToggleController);

  function start() {
    boot();
    document.querySelectorAll(".flash").forEach(flashFade);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
