package common

import (
	"github.com/panyam/demokit"
	"github.com/panyam/demokit/notebookbridge"
	"github.com/panyam/demokit/tui"
)

// SetupRenderer wires the renderer that matches demokit's resolved mode:
//
//	--tui  → tui.New()            (Lipgloss boxes)
//	--note → notebookbridge.New() (cell-based notebook UI)
//	default → demokit's plain renderer (no call needed)
//
// Both renderers use BorderHorizontalOnly so a triple-click / drag-select
// over a verbatim block grabs only the content -- no side box characters --
// and long lines (curl payloads, JSON blobs) stay byte-exact for paste.
//
// Centralizing this is what lets `make note` (which shells out to --note)
// light up notebook mode for every walkthrough without per-example glue.
// Each main.go calls common.SetupRenderer(demo) just before demo.Execute().
func SetupRenderer(demo *demokit.Demo) {
	switch demokit.Mode() {
	case "tui":
		demo.WithRenderer(tui.New().WithBorderStyle(demokit.BorderHorizontalOnly))
	case "notebook":
		demo.WithRenderer(notebookbridge.New().WithBorderStyle(demokit.BorderHorizontalOnly))
	}
}
