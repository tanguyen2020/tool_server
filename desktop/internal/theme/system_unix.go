//go:build !windows && !darwin

package theme

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// systemPrefersDark follows what WebKitGTK uses for prefers-color-scheme: a GTK theme named "...-dark"
// (GTK_THEME or the desktop's gtk-theme), then GNOME's dark style. Unknown desktops count as light,
// the GTK default.
func systemPrefersDark() bool {
	if t := os.Getenv("GTK_THEME"); t != "" {
		return isDarkName(t)
	}
	if v, ok := gsetting("gtk-theme"); ok && isDarkName(v) {
		return true
	}
	v, _ := gsetting("color-scheme")
	return v == "prefer-dark"
}

func isDarkName(theme string) bool { return strings.Contains(strings.ToLower(theme), "dark") }

func gsetting(key string) (string, bool) {
	// Bounded: the window waits on this at startup.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gsettings", "get", "org.gnome.desktop.interface", key).Output()
	if err != nil {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(string(out)), "'"), true
}
