package main

import (
	"context"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"serverdash/internal/updater"
)

// version is stamped by the release build: -ldflags "-X main.version=1.2.3". Local builds stay "dev".
var version = "dev"

const updateEvery = 6 * time.Hour

func (a *App) AppVersion() string { return version }

type updateProgress struct {
	Version string `json:"version"`
	Done    int64  `json:"done"`
	Total   int64  `json:"total"`
}

// CheckUpdate looks for a newer signed release.
func (a *App) CheckUpdate() (updater.Info, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	return a.upd.Check(ctx)
}

// DownloadUpdate fetches and verifies the newer release found by CheckUpdate ("update.progress" events),
// then announces it with "update.ready".
func (a *App) DownloadUpdate() error {
	info, err := a.CheckUpdate()
	if err != nil {
		return err
	}
	if !info.Available {
		return nil
	}
	if !info.CanInstall {
		// Installed system-wide: say so once per version, the user downloads it by hand.
		if a.announced != info.Latest {
			a.announced = info.Latest
			runtime.EventsEmit(a.ctx, "update.available", info)
		}
		return nil
	}
	if a.upd.Staged() == info.Latest {
		runtime.EventsEmit(a.ctx, "update.ready", info)
		return nil
	}
	err = a.upd.Download(a.ctx, func(done, total int64) {
		runtime.EventsEmit(a.ctx, "update.progress", updateProgress{info.Latest, done, total})
	})
	if err != nil {
		return err
	}
	info.Staged = info.Latest
	runtime.EventsEmit(a.ctx, "update.ready", info)
	return nil
}

// InstallUpdate puts the downloaded version in place and restarts the app.
func (a *App) InstallUpdate() error {
	from := version
	to := a.upd.Staged()
	if err := a.upd.Apply(); err != nil {
		a.audit("", "update", from+" → "+to, err)
		return err
	}
	a.audit("", "update", from+" → "+to, nil)
	if err := a.upd.Relaunch(); err != nil {
		return err
	}
	runtime.Quit(a.ctx)
	return nil
}

// SetAutoUpdate turns the background check and download on or off.
func (a *App) SetAutoUpdate(on bool) error {
	s := a.st.Settings()
	s.AutoUpdate = on
	return a.st.SetSettings(s)
}

// autoUpdate checks shortly after start and then every few hours; a newer release is downloaded in the
// background and installed when the app closes (or right away from the banner).
func (a *App) autoUpdate(ctx context.Context) {
	if updater.IsDev(version) {
		return
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if a.st.Settings().AutoUpdate {
			if err := a.DownloadUpdate(); err != nil {
				runtime.LogInfo(a.ctx, "update check: "+err.Error())
			}
		}
		timer.Reset(updateEvery)
	}
}

// installOnExit installs a downloaded update when the app closes, so the next start runs the new version.
func (a *App) installOnExit() {
	if a.upd == nil || a.upd.Staged() == "" {
		return
	}
	to := a.upd.Staged()
	err := a.upd.Apply()
	a.audit("", "update", version+" → "+to+" (on exit)", err)
}
