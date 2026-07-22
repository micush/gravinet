package webadmin

// Vendored @xterm/xterm (MIT) — see vendor/xterm/VENDORED.md for why this is
// here, what it is, and how to update it. Served unauthenticated by
// handleXtermJS/handleXtermCSS (webadmin.go), the same trust level as the
// main index page's own HTML/JS/CSS — this is a static browser asset, not a
// Go module dependency (go.mod is still dependency-free), matching the
// existing embed use in internal/tun for the bundled Windows driver.

import _ "embed"

//go:embed vendor/xterm/xterm.js
var xtermJS string

//go:embed vendor/xterm/xterm.css
var xtermCSS string
