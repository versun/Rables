// Package assets embeds the frontend static files served by httpd under
// /assets/: app.js (vanilla JS, plan §9), app.css (core styles) and the
// vendored lexxy rich-text editor (lexxy.min.js + lexxy.css, lexxy gem
// 0.9.28, MIT — the same editor Rails uses via the lexxy gem).
package assets

import "embed"

//go:embed app.js app.css lexxy.min.js lexxy.css
var FS embed.FS
