package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"serverdash/internal/history"
	"serverdash/internal/monitor"
)

// diskAlertDays is the forecast below which a filling disk raises an alert.
const diskAlertDays = 7

// DiskForecast returns the growth trend of every mount of a server (see history.DiskForecast).
func (a *App) DiskForecast(serverID string) ([]history.Forecast, error) {
	if a.hist == nil {
		return []history.Forecast{}, nil
	}
	fc, err := a.hist.DiskForecast(serverID, time.Now())
	sort.Slice(fc, func(i, j int) bool { return fc[i].Mount < fc[j].Mount })
	return fc, err
}

// watchDiskForecasts checks every hour whether a disk will be full within diskAlertDays,
// alerting at most once a day per mount.
func (a *App) watchDiskForecasts(ctx context.Context) {
	if a.hist == nil {
		return
	}
	alerted := map[string]time.Time{}
	next := time.NewTimer(2 * time.Minute) // first check soon after start, then hourly
	defer next.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-next.C:
		}
		next.Reset(time.Hour)
		for _, srv := range a.st.ListPublic() {
			fc, err := a.hist.DiskForecast(srv.ID, time.Now())
			if err != nil {
				continue
			}
			for _, f := range fc {
				if f.DaysLeft == nil || *f.DaysLeft >= diskAlertDays {
					continue
				}
				k := srv.ID + "\x00" + f.Mount
				if time.Since(alerted[k]) < 24*time.Hour {
					continue
				}
				alerted[k] = time.Now()
				a.notify(monitor.Alert{
					Title:    fmt.Sprintf("%s: disk %s fills up in %s", srv.Name, f.Mount, daysText(*f.DaysLeft)),
					Body:     fmt.Sprintf("%.0f%% used, growing %s a day.", f.Used/f.Size*100, gib(f.PerDay)),
					Critical: *f.DaysLeft < 2,
				})
			}
		}
	}
}

func daysText(d float64) string {
	if d < 1 {
		return fmt.Sprintf("about %.0f hours", math.Max(1, d*24))
	}
	return fmt.Sprintf("about %.0f days", math.Round(d))
}

func gib(b float64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GB", b/(1<<30))
	}
	return fmt.Sprintf("%.0f MB", b/(1<<20))
}
