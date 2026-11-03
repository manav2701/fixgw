// Package web embeds the dashboard, a static page with no build step.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html style.css app.js
var files embed.FS

// Handler serves the dashboard.
func Handler() http.Handler {
	sub, _ := fs.Sub(files, ".")
	return http.FileServerFS(sub)
}
