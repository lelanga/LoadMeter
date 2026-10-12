// Package examples embeds the sample scenarios shipped with Load Meter.
package examples

import "embed"

//go:embed *.json *.csv
var FS embed.FS
