package dbsphere

import (
	"embed"
	"encoding/json"
)

// Assets contains the browser client, served directly by the Go binary.
//
//go:embed public/index.html public/app.js public/observability.js public/monitoring.js public/live.js public/live.css public/overview.js public/overview.css public/wallboard.js public/wallboard.css public/container-storage.js public/container-storage.css public/theme.js public/styles.css public/freshness.css public/console.css public/favicon.svg
//go:embed public/log-analysis.js public/log-analysis.css
//go:embed public/change-governance.js public/restore-drill.js public/service-operations.js public/managed-sites.js
var Assets embed.FS

// Version is embedded in the binary from the same package manifest used to
// name the offline bundle and GitHub release assets.
//
//go:embed package.json
var packageManifest []byte

var Version = func() string {
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(packageManifest, &manifest); err != nil || manifest.Version == "" {
		panic("invalid DbSphere package version")
	}
	return manifest.Version
}()
