// uishot renders the static screens (browser, settings) to PNGs for review.
package main

import (
	"image"
	"image/png"
	"log"
	"os"
	"time"

	"f1telemetry/model"
	"f1telemetry/ui"
)

func save(name string, img *image.RGBA) {
	f, err := os.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	log.Println("wrote", name)
}

func main() {
	r := ui.NewRenderer()
	save("build/shot_seasons.png", r.RenderBrowse(ui.BrowseView{
		Title:   "SELECT SEASON",
		Sub:     "Replay mode — pick a season, then a weekend and a session",
		Buttons: []string{"SETTINGS", "EXIT"},
		Rows: []ui.BrowseRow{
			{Main: "2026 SEASON", Right: "last viewed"},
			{Main: "2025 SEASON"},
			{Main: "2024 SEASON"},
			{Main: "2023 SEASON"},
		},
	}))
	meetings := []ui.BrowseRow{
		{Left: "Jul 06", Main: "British Grand Prix", Right: "Silverstone"},
		{Left: "Jun 29", Main: "Austrian Grand Prix", Right: "Spielberg"},
		{Left: "Jun 15", Main: "Canadian Grand Prix", Right: "Montréal"},
	}
	save("build/shot_meetings.png", r.RenderBrowse(ui.BrowseView{
		Title:   "2026 SEASON",
		Sub:     "Select a race weekend",
		Buttons: []string{"BACK", "SETTINGS", "EXIT"},
		Rows:    meetings,
	}))
	sessions := []model.SessionOption{
		{SessionName: "Practice 1", Country: "UK", Date: time.Now()},
		{SessionName: "Qualifying", Country: "UK", Date: time.Now()},
		{SessionName: "Race", Country: "UK", Date: time.Now()},
	}
	var rows []ui.BrowseRow
	for _, s := range sessions {
		rows = append(rows, ui.BrowseRow{
			Left: s.Date.Format("Mon 15:04"), Main: s.SessionName, Right: s.Country,
		})
	}
	save("build/shot_sessions.png", r.RenderBrowse(ui.BrowseView{
		Title:   "BRITISH GRAND PRIX",
		Sub:     "Silverstone · 2026 — select a session to replay",
		Buttons: []string{"BACK", "SETTINGS", "EXIT"},
		Rows:    rows,
	}))
	save("build/shot_loading.png", r.RenderMessage("LOADING REPLAY", "Abu Dhabi Grand Prix — Race", 5))
	save("build/shot_browse_loading.png", r.RenderBrowse(ui.BrowseView{
		Title:   "2025 SEASON",
		Sub:     "Select a race weekend",
		Buttons: []string{"BACK", "SETTINGS", "EXIT"},
		Empty:   "Loading weekends from OpenF1...",
		Loading: true,
		Tick:    7,
	}))
	save("build/shot_waiting.png", r.RenderWaiting(ui.WaitingView{
		Tick:    3,
		Elapsed: "6 min",
		Status:  "Connected — no session on air yet",
		Buttons: []string{"RETRY", "SETTINGS", "EXIT"},
	}))
	save("build/shot_settings.png", r.RenderSettings(ui.SettingsView{
		ShowBestSectors: true, DelaySeconds: 0, ReplaySpeed: 60, LiveDelayShown: true,
		CanExit: true, CanBack: true,
	}))

	st := model.State{
		IsReplay:    true,
		MeetingName: "British Grand Prix",
		Session:     &model.Session{Circuit: "Silverstone", SessionName: "Race", Location: "Silverstone", Country: "UK", Year: 2026},
		LastUpdate:  time.Now(),
		Source:      "OpenF1 replay",
	}
	save("build/shot_replay.png", r.Render(st, ui.TabTiming, nil, 0, ui.ViewOptions{}))

	// Header line showing the newest race control message instead of the venue.
	st.LeaderLap, st.TotalLaps = 42, 52
	st.RaceControl = []model.RaceControl{
		{Date: time.Now(), Flag: "YELLOW", Message: "DOUBLE YELLOW IN TRACK SECTOR 7 - CAR 81 (PIA) STOPPED ON TRACK"},
		{Date: time.Now().Add(-time.Minute), Message: "TRACK LIMITS - CAR 4 (NOR) TURN 9"},
	}
	save("build/shot_header_rc.png", r.Render(st, ui.TabTiming, nil, 0, ui.ViewOptions{HeaderRC: true}))
	save("build/shot_time_popup.png", r.Render(st, ui.TabTiming, nil, 0, ui.ViewOptions{TimePopup: true}))
}
