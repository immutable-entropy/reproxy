package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// UIState is the window layout remembered between sessions. Unlike Config
// it isn't edited by the user directly: the UI saves it on exit and restores
// it on launch.
//
// Geometry and the splitter state are Qt's own opaque encodings
// (QWidget::saveGeometry, QSplitter::saveState), which already cope with
// monitors being removed or resolutions changing. They are stored as base64.
type UIState struct {
	WindowGeometry []byte `json:"window_geometry,omitempty"`
	OuterSplitter  []byte `json:"outer_splitter,omitempty"`
	// TrafficDetailWidth is how wide the Traffic section's request/response
	// pane was when last open, in pixels. The pane starts closed every
	// session and slides out to this width; zero uses the default share.
	TrafficDetailWidth int `json:"traffic_detail_width,omitempty"`
	// Panels records which hideable panels are shown, by name. Panels
	// missing from the map keep their default.
	Panels map[string]bool `json:"panels,omitempty"`
	// Compact remembers the request list's density (comfortable vs compact
	// rows). It is set from the Traffic header's density button and
	// auto-enabled past 50 requests.
	Compact bool `json:"compact,omitempty"`
	// ShowDNS lists DNS lookups in the request list. They are recorded
	// either way, and hidden by default (a held one always shows).
	ShowDNS bool `json:"show_dns,omitempty"`
	// HiddenMethods are the request methods the Traffic panel's method
	// filter hides. Empty shows every method.
	HiddenMethods []string `json:"hidden_methods,omitempty"`
	// NavSection is the navigation item that was selected, so the window
	// reopens on the same section. An empty or unknown value opens the
	// default one.
	NavSection string `json:"nav_section,omitempty"`
	// NavLabels shows the navigation items' names beside their icons. It is
	// set from View → Navigation; icons only is the default.
	NavLabels bool `json:"nav_labels,omitempty"`
	// ExplorerGraph shows the Explorer section as a graph rather than the
	// default tree; it is switched from the Explorer's header. (The
	// Explorer was the Map: the names in the file are kept.)
	ExplorerGraph bool `json:"map_graph,omitempty"`
	// ExplorerHiddenKinds are the content types (sitemap.Kind values) the
	// Explorer's content filter hides. Empty shows everything.
	ExplorerHiddenKinds []string `json:"map_hidden_kinds,omitempty"`
	// ExplorerHiddenTypes are the entity types (domain, host, url) the
	// Explorer's type filter hides. Empty shows everything.
	ExplorerHiddenTypes []string `json:"explorer_hidden_types,omitempty"`
	// ExplorerRelated draws the hosts the targets led to but don't cover
	// in the Explorer as well as the targets' own. They are hidden by
	// default: the Targets section is where they are dealt with.
	ExplorerRelated bool `json:"explorer_related,omitempty"`
	// SessionsSide is where the Sessions panel is pinned: "bottom" (the
	// default), "top", or "window" for a window of its own. It is set from
	// View → Sessions → Position or the panel's own kebab.
	SessionsSide string `json:"sessions_side,omitempty"`
	// SessionsHidden are the Sessions panel's tabs left out of it
	// (View → Sessions → Contents). Empty holds every tab; all of them
	// hides the panel altogether.
	SessionsHidden []string `json:"sessions_hidden,omitempty"`
	// SessionsWindow is the geometry of the window the Sessions panel
	// floats in, so it reopens where it was. Qt's own encoding, base64.
	SessionsWindow []byte `json:"sessions_window,omitempty"`
}

// UIStatePath returns the layout file that sits beside the settings file at
// configPath.
func UIStatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "ui-state.json")
}

// LoadUIState reads the layout at path. A missing file gives the zero
// UIState, meaning "use the default layout".
func LoadUIState(path string) (UIState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return UIState{}, nil
	}
	if err != nil {
		return UIState{}, fmt.Errorf("reading layout: %w", err)
	}
	var s UIState
	if err := json.Unmarshal(data, &s); err != nil {
		return UIState{}, fmt.Errorf("parsing layout %s: %w", path, err)
	}
	return s, nil
}

// SaveUIState writes the layout to path atomically.
func SaveUIState(path string, s UIState) error {
	return writeJSON(path, s)
}
