// Package examples embeds the sample scenarios shipped with Phoenix.
package examples

import "embed"

//go:embed *.json *.csv
var FS embed.FS
