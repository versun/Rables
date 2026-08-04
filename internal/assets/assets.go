// Package assets embeds the frontend static files served by httpd under
// /assets/: app.js (vanilla JS, plan §9), app.css (core styles), the
// vendored lexxy rich-text editor (lexxy.min.js + lexxy.css, lexxy gem
// 0.9.28, MIT — the same editor Rails uses via the lexxy gem) and the
// vendored EasyMDE markdown editor (easymde.min.js + easymde.min.css,
// v2.20.0, MIT).
package assets

import "embed"

//go:embed app.js app.css lexxy.min.js lexxy.css easymde.min.js easymde.min.css
var FS embed.FS
