// Package theme resolves the light/dark preference for the native window.
package theme

// Valid reports whether mode is a known theme mode ("" means system).
func Valid(mode string) bool {
	return mode == "" || mode == "system" || mode == "light" || mode == "dark"
}

// IsDark tells whether the window should be dark for the given mode.
func IsDark(mode string) bool {
	switch mode {
	case "light":
		return false
	case "dark":
		return true
	}
	return systemPrefersDark()
}

// Background is the window colour shown before the page paints (matches --page in style.css).
func Background(mode string) (r, g, b uint8) {
	if IsDark(mode) {
		return 13, 13, 13
	}
	return 241, 241, 238
}
