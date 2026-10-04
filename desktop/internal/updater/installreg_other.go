//go:build !windows

package updater

// RecordInstalledVersion only matters on Windows ("Apps & features").
func RecordInstalledVersion(string) {}
