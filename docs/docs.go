// Package docs embeds the Markdown documentation so the binary (and the MCP
// server's phoenix://docs/... resources) can serve it.
package docs

import "embed"

//go:embed *.md
var FS embed.FS
