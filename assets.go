package dbsphere

import (
	"embed"
	"encoding/json"
)

// Assets contains the browser client, served directly by the Go binary.
//
//go:embed public/index.html public/app.js public/styles.css public/freshness.css public/favicon.svg
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
