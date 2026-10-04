//go:build windows

package updater

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// UninstallKey is written by the Windows installer (build/windows/installer/setup.nsi).
const UninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\ServerDashboard`

// RecordInstalledVersion updates the version shown in "Apps & features" when this copy is the installed
// one (the installer wrote the key; the app later replaces itself without running the installer).
func RecordInstalledVersion(version string) {
	if IsDev(version) {
		return
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, UninstallKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	loc, _, err := k.GetStringValue("InstallLocation")
	exe, err2 := os.Executable()
	if err != nil || err2 != nil || !strings.EqualFold(filepath.Clean(loc), filepath.Dir(exe)) {
		return
	}
	if cur, _, _ := k.GetStringValue("DisplayVersion"); cur != version {
		_ = k.SetStringValue("DisplayVersion", version)
	}
}
