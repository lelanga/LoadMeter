// Package web embeds the static single-page UI served by `phoenix serve`.
package web

import "embed"

// Static holds the files under web/static (index.html, app.js, modules, style.css).
//
//go:embed static
var Static embed.FS
