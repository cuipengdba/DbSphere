package dbsphere

import "embed"

// Assets contains the browser client, served directly by the Go binary.
//
//go:embed public/index.html public/app.js public/styles.css public/favicon.svg
var Assets embed.FS
