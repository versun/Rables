// Package templates renders HTML pages from embedded templates.
//
// layout.html provides the site skeleton (html/head/body, flash area) with
// "title" and "content" blocks. admin_layout.html provides the admin shell
// (sidebar nav + main column) with the same blocks, used by every page whose
// name starts with "admin_" plus the account page (see useAdminLayout).
// Every other *.html file in this directory is a page that fills those
// blocks:
//
//	{{define "title"}}Page title{{end}}
//	{{define "content"}}...{{end}}
//
// A page file becomes renderable by its file base name (e.g. "dummy").
// Files whose name starts with "_" are partials instead: their {{define}}
// blocks (e.g. "comments_tree") are parsed into every page set so pages can
// invoke them with {{template "name" .}}, but they are not renderable pages.
// Template data passed to Render must expose a Flash field of type Flash,
// consumed by the layout through the flashHTML function.
package templates

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"strings"
)

//go:embed *.html
var templateFS embed.FS

const (
	layoutName      = "layout.html"
	adminLayoutName = "admin_layout.html"
)

// pageTemplate is one renderable page: its template set and the name of the
// root layout template to execute.
type pageTemplate struct {
	tpl    *template.Template
	layout string
}

// Renderer executes the embedded page templates. Construct it once with New;
// it is safe for concurrent use.
type Renderer struct {
	pages map[string]pageTemplate
}

// useAdminLayout reports whether the page renders inside the admin shell
// (sidebar nav + main column) instead of the bare layout. auth_password_edit
// is the account page, which uses the admin layout in Rails (UsersController).
func useAdminLayout(page string) bool {
	return strings.HasPrefix(page, "admin_") || page == "auth_password_edit"
}

// New parses the layouts and every embedded page template.
func New() (*Renderer, error) {
	base := template.New("").Funcs(FuncMap())
	if _, err := base.ParseFS(templateFS, layoutName); err != nil {
		return nil, fmt.Errorf("templates: parse layout: %w", err)
	}
	adminBase := template.New("").Funcs(FuncMap())
	if _, err := adminBase.ParseFS(templateFS, adminLayoutName); err != nil {
		return nil, fmt.Errorf("templates: parse admin layout: %w", err)
	}

	entries, err := fs.ReadDir(templateFS, ".")
	if err != nil {
		return nil, fmt.Errorf("templates: list pages: %w", err)
	}

	var partials, pages []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == layoutName || name == adminLayoutName || !strings.HasSuffix(name, ".html") {
			continue
		}
		if strings.HasPrefix(name, "_") {
			partials = append(partials, name)
		} else {
			pages = append(pages, name)
		}
	}

	// Partials contribute their defines to the shared bases (and thus to
	// every page clone below) without becoming pages themselves.
	for _, name := range partials {
		if _, err := base.ParseFS(templateFS, name); err != nil {
			return nil, fmt.Errorf("templates: parse partial %s: %w", name, err)
		}
		if _, err := adminBase.ParseFS(templateFS, name); err != nil {
			return nil, fmt.Errorf("templates: parse partial %s: %w", name, err)
		}
	}

	r := &Renderer{pages: make(map[string]pageTemplate, len(pages))}
	for _, name := range pages {
		// Each page gets its own template set (layout clone + page defines)
		// so per-page "title"/"content" blocks do not collide.
		layout := layoutName
		pageBase := base
		if useAdminLayout(name) {
			layout = adminLayoutName
			pageBase = adminBase
		}
		page, err := pageBase.Clone()
		if err != nil {
			return nil, fmt.Errorf("templates: clone layout: %w", err)
		}
		if _, err := page.ParseFS(templateFS, name); err != nil {
			return nil, fmt.Errorf("templates: parse %s: %w", name, err)
		}
		r.pages[strings.TrimSuffix(name, ".html")] = pageTemplate{tpl: page, layout: layout}
	}
	return r, nil
}

// Render writes the named page (file base name without .html) composed with
// its layout. data must expose a Flash field (see Flash).
func (r *Renderer) Render(w io.Writer, name string, data any) error {
	page, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("templates: unknown page %q", name)
	}
	return page.tpl.ExecuteTemplate(w, page.layout, data)
}
