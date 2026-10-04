package main

import (
	"embed"
	"log"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"serverdash/internal/history"
	"serverdash/internal/store"
	"serverdash/internal/theme"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	st, err := store.Open()
	if err != nil {
		log.Fatal(err)
	}
	hist, err := history.Open(st.Dir())
	if err != nil {
		log.Printf("history disabled: %v", err)
		hist = nil
	}
	app := NewApp(st, hist)
	// Start with the saved theme so the window never flashes the wrong colour.
	mode := st.Settings().Theme
	bgR, bgG, bgB := theme.Background(mode)
	winTheme := windows.SystemDefault
	switch mode {
	case "light":
		winTheme = windows.Light
	case "dark":
		winTheme = windows.Dark
	}

	err = wails.Run(&options.App{
		Title:            "Server Dashboard",
		Width:            1360,
		Height:           880,
		MinWidth:         900,
		MinHeight:        600,
		WindowStartState: options.Maximised,
		BackgroundColour: &options.RGBA{R: bgR, G: bgG, B: bgB, A: 255},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		// Launching a second time brings the running window to front instead of starting another copy.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "b7d1f0c2-serverdash",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				runtime.WindowUnminimise(app.ctx)
				runtime.WindowShow(app.ctx)
			},
		},
		Windows: &windows.Options{
			Theme:               winTheme,
			WebviewUserDataPath: filepath.Join(st.Dir(), "webview"),
			DisablePinchZoom:    true,
			// The UI only draws a few simple canvas charts, which CPU rendering handles fine;
			// dropping the WebView2 GPU process saves ~190 MB of RAM.
			WebviewGpuIsDisabled: true,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
