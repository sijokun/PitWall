//go:build linux

// pitwall — live Formula 1 timing for the reMarkable Paper Pro, running as an
// AppLoad (qtfb) app.
//
// During sessions it renders F1's push feed as events arrive (no account or
// token required); otherwise it shows a browser of past OpenF1 sessions that
// can be replayed on-device. Bottom tab bar switches
// TIMING / MAP / RACE CONTROL; in a replay the CLOSE button (top right)
// returns to the browser; tapping elsewhere forces a deep e-ink refresh.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/sijokun/PitWall/app"
	"github.com/sijokun/PitWall/qtfb"
	"github.com/sijokun/PitWall/ui"
)

type fbDisplay struct {
	fb   *qtfb.Client
	prev []uint8 // last presented RGBA frame
}

// convertLines packs RGBA lines [y0, y1) into the RGB565 shm buffer.
func (d *fbDisplay) convertLines(frame []uint8, w, y0, y1 int) {
	for i := y0 * w; i < y1*w; i++ {
		r, g, b := frame[i*4], frame[i*4+1], frame[i*4+2]
		v := uint16(r>>3)<<11 | uint16(g>>2)<<5 | uint16(b>>3)
		d.fb.Shm[i*2] = byte(v)
		d.fb.Shm[i*2+1] = byte(v >> 8)
	}
}

// Present diffs against the previous frame and flushes only the changed
// horizontal bands as partial e-ink updates — a single driver's timing cell
// refreshes just that table row. Large diffs fall back to a full update.
func (d *fbDisplay) Present(frame []uint8, w, h int) {
	stride := w * 4
	if d.prev == nil {
		d.prev = make([]uint8, len(frame))
		copy(d.prev, frame)
		d.convertLines(frame, w, 0, h)
		d.fb.FullUpdate()
		return
	}

	// Collect dirty line bands, merging gaps under 16 lines.
	var bands [][2]int
	for y := 0; y < h; y++ {
		if bytes.Equal(frame[y*stride:(y+1)*stride], d.prev[y*stride:(y+1)*stride]) {
			continue
		}
		if n := len(bands); n > 0 && y-bands[n-1][1] < 16 {
			bands[n-1][1] = y + 1
		} else {
			bands = append(bands, [2]int{y, y + 1})
		}
	}
	if len(bands) == 0 {
		return // nothing changed — no e-ink churn at all
	}

	dirty := 0
	for _, b := range bands {
		dirty += b[1] - b[0]
	}
	for _, b := range bands {
		d.convertLines(frame, w, b[0], b[1])
		copy(d.prev[b[0]*stride:b[1]*stride], frame[b[0]*stride:b[1]*stride])
	}
	if dirty > h*55/100 {
		d.fb.FullUpdate()
		return
	}
	for _, b := range bands {
		d.fb.PartialUpdate(0, b[0], w, b[1]-b[0])
	}
}

func (d *fbDisplay) DeepRefresh() {
	d.fb.RequestFullRefresh()
}

func main() {
	source := flag.String("source", "auto", "data source: auto | live | openf1")
	speed := flag.Float64("speed", 60, "replay speed factor")
	minRedraw := flag.Duration("min-redraw", time.Second, "minimum interval between e-ink redraws")
	screen := flag.String("screen", "", "panel size WxH (default: Paper Pro 1620x2160)")
	flag.Parse()

	// The layout adapts to the panel, but AppLoad's protocol never reports its
	// size, so a non-Paper-Pro device has to be told. Do this before anything
	// builds a renderer: the font sizes are fixed at that point.
	if *screen != "" {
		var w, h int
		if _, err := fmt.Sscanf(*screen, "%dx%d", &w, &h); err != nil {
			log.Fatalf("bad -screen %q: want WxH, e.g. 1404x1872", *screen)
		}
		ui.SetScreen(w, h)
	}

	fb, err := qtfb.Connect(qtfb.KeyFromEnv(), qtfb.FmtRMPPRGB565)
	if err != nil {
		log.Fatalf("qtfb: %v", err)
	}
	defer fb.Close()
	fb.Width, fb.Height = ui.Width, ui.Height
	if need := fb.Width * fb.Height * 2; len(fb.Shm) < need {
		// The shared buffer is width*height*2 for RGB565, so its size is the
		// one clue the server does give us about the real panel.
		log.Fatalf("qtfb: shm is %d bytes but %dx%d needs %d — pass -screen WxH for this device's panel",
			len(fb.Shm), fb.Width, fb.Height, need)
	}
	fb.SetRefreshMode(qtfb.RefreshContent)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	quit := make(chan struct{})
	var quitOnce sync.Once

	a := app.New(app.Config{
		Source:      *source,
		OpenF1Key:   os.Getenv("OPENF1_API_KEY"),
		ReplaySpeed: *speed,
		MinRedraw:   *minRedraw,
		OnExit:      func() { quitOnce.Do(func() { close(quit) }) },
	}, &fbDisplay{fb: fb})

	go a.Run(ctx)

	lastDeep := time.Now()
	for {
		select {
		case ev := <-fb.Input:
			if ev.Type == qtfb.InputTouchRelease || ev.Type == qtfb.InputPenRelease {
				a.Touch(ctx, int(ev.X), int(ev.Y))
			}
		case <-time.After(3 * time.Minute):
			// Periodic deep refresh clears e-ink ghosting.
			if time.Since(lastDeep) > 3*time.Minute {
				fb.RequestFullRefresh()
				lastDeep = time.Now()
			}
		case <-quit:
			log.Println("exit via settings")
			return
		case <-fb.Terminated:
			log.Println("terminated by AppLoad")
			return
		}
	}
}
