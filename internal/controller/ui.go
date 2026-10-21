package controller

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui/index.html
var uiFS embed.FS

//go:embed grafana/*.json
var grafanaFS embed.FS

func uiHandler() http.Handler {
	sub, _ := fs.Sub(uiFS, "ui")
	return http.FileServer(http.FS(sub))
}

// GrafanaDashboards returns the bundled dashboard JSON files by name.
func GrafanaDashboards() (map[string][]byte, error) {
	out := map[string][]byte{}
	entries, err := grafanaFS.ReadDir("grafana")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := grafanaFS.ReadFile("grafana/" + e.Name())
		if err != nil {
			return nil, err
		}
		out[e.Name()] = b
	}
	return out, nil
}
