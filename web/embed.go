// Package web embeds the HTML templates, message catalogs and static assets.
package web

import "embed"

//go:embed templates static locales
var FS embed.FS
