// preview renders one frame of the F1 telemetry screen to a PNG — useful
// for developing the UI without a reMarkable attached. Tries the free F1
// live timing stream first; falls back to OpenF1 historical data when no
// session is running.
package main

import (
	"context"
	"flag"
	"fmt"
	"image/png"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/sijokun/PitWall/circuit"
	"github.com/sijokun/PitWall/dnsfix"
	"github.com/sijokun/PitWall/livetiming"
	"github.com/sijokun/PitWall/model"
	"github.com/sijokun/PitWall/openf1"
	"github.com/sijokun/PitWall/ui"
)

func main() {
	out := flag.String("o", "preview.png", "output PNG path")
	source := flag.String("source", "auto", "data source: auto | live | openf1")
	sessionKey := flag.String("session", "latest", "OpenF1 session_key for the fallback source")
	wait := flag.Duration("wait", 8*time.Second, "how long to wait for live timing data")
	replay := flag.String("replay", "", "render from a recorded session file (fastf1 livetiming save format)")
	tabName := flag.String("tab", "timing", "tab to render: timing | map | rc")
	circuitKey := flag.Int("circuit", 0, "override MultiViewer circuit key for the map tab")
	circuitYear := flag.Int("year", 2025, "circuit year for -circuit")
	screen := flag.String("screen", "", "panel size WxH (default: Paper Pro 1620x2160)")
	into := flag.Duration("into", 0, "with -source openf1: render this far into the session (e.g. 60m) instead of its final state")
	flag.Parse()
	if *screen != "" {
		var w, h int
		if _, err := fmt.Sscanf(*screen, "%dx%d", &w, &h); err != nil {
			log.Fatalf("bad -screen %q: want WxH", *screen)
		}
		ui.SetScreen(w, h)
	}
	dnsfix.Install()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var st model.State
	gotLive := false
	if *replay != "" {
		live := livetiming.New()
		n, err := live.ReplayFile(*replay)
		if err != nil {
			log.Fatalf("replay: %v", err)
		}
		log.Printf("replayed %d feed updates", n)
		st = live.Snapshot()
		st.Source = "Replay"
		gotLive = true
		*source = "live"
	}
	if !gotLive && *source != "openf1" {
		live := livetiming.New()
		go live.Run(ctx)
		deadline := time.Now().Add(*wait)
		for time.Now().Before(deadline) {
			if live.HasTiming() {
				gotLive = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if gotLive {
			time.Sleep(2 * time.Second) // let the initial snapshot fill in
			st = live.Snapshot()
		} else {
			log.Printf("no live session data after %v", *wait)
		}
	}
	if !gotLive && *source != "live" {
		log.Printf("falling back to OpenF1 (most recent session)")
		api := openf1.New(*sessionKey)
		api.APIKey = os.Getenv("OPENF1_API_KEY")
		if key, err := strconv.Atoi(*sessionKey); *into > 0 && err == nil {
			// Play the session forward to a chosen point instead of taking the
			// finished state — mid-race frames show gaps and tyre spread that
			// a chequered-flag snapshot does not.
			rep, err := api.LoadReplay(ctx, key)
			if err != nil {
				log.Fatalf("load replay: %v", err)
			}
			st = rep.Advance(*into)
			log.Printf("advanced %v into session %d", *into, key)
		} else {
			if *into > 0 {
				log.Printf("-into needs a numeric -session; showing the final state")
			}
			st = api.Refresh(ctx)
		}
	}
	if st.Err != "" {
		log.Printf("warning: %s", st.Err)
	}

	tab := ui.TabTiming
	switch *tabName {
	case "map":
		tab = ui.TabMap
	case "rc":
		tab = ui.TabRaceControl
	}
	var trackMap *model.TrackMap
	key, year := st.CircuitKey, 0
	if st.Session != nil {
		year = st.Session.Year
	}
	if *circuitKey != 0 {
		key, year = *circuitKey, *circuitYear
	}
	if tab == ui.TabMap && key != 0 {
		tm, err := circuit.Fetch(ctx, key, year)
		if err != nil {
			log.Printf("circuit map: %v", err)
		}
		trackMap = tm
	}

	img := ui.NewRenderer().Render(st, tab, trackMap, 0, ui.ViewOptions{})
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (source=%s, %d drivers)", *out, st.Source, len(st.Standings))
}
