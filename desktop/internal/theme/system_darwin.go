//go:build darwin

package theme

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// systemPrefersDark reads System Settings > Appearance: the key only exists (as "Dark") while dark mode is on.
func systemPrefersDark() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "defaults", "read", "-g", "AppleInterfaceStyle").Output()
	return err == nil && strings.EqualFold(strings.TrimSpace(string(out)), "dark")
}
