//go:build windows

package theme

import "golang.org/x/sys/windows/registry"

// systemPrefersDark reads the "app mode" chosen in Windows Settings > Personalization > Colors.
func systemPrefersDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err != nil || v == 0
}
