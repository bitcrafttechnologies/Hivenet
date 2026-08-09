// Package web embeds the built frontend into the Go binary.
//
// Hivenet ships as one binary in one container (spec §2): the static Svelte
// build is served by the same HTTP server that hosts the WebSocket endpoints,
// with no separate frontend service.
//
// dist/ must contain at least one file or the whole module fails to compile —
// go:embed treats a pattern matching nothing as an error. The checked-in
// placeholder index.html exists for that reason; `npm run build` overwrites it.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// Assets returns the built frontend rooted at dist/.
func Assets() (fs.FS, error) {
	return fs.Sub(embedded, "dist")
}
