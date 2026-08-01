// devapp is a desktop simulator for the reMarkable app: identical
// controller and rendering as the device, shown in a window at half scale
// with mouse clicks as touches. Debug the UI live on a Mac/PC — against the
// real feed, the OpenF1 session browser/replays, or a recorded file.
//
//	go run ./cmd/devapp                      # live mode: session, else waiting screen
//	go run ./cmd/devapp -source openf1       # start in the replay browser
//	go run ./cmd/devapp -replay testdata/... -speed 60
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"

	"github.com/sijokun/PitWall/app"
	"github.com/sijokun/PitWall/ui"
)

const windowScale = 2

type game struct {
	mu    sync.Mutex
	frame *ebiten.Image
	a     *app.App
	ctx   context.Context
}

func (g *game) Present(frame []uint8, w, h int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.frame == nil {
		g.frame = ebiten.NewImage(w, h)
	}
	g.frame.WritePixels(frame)
}

func (g *game) DeepRefresh() {}

func (g *game) Update() error {
	if inpututil.IsMouseButtonJustReleased(ebiten.MouseButtonLeft) {
		x, y := ebiten.CursorPosition()
		go g.a.Touch(g.ctx, x*windowScale, y*windowScale)
	}
	return nil
}

func (g *game) Draw(screen *ebiten.Image) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.frame == nil {
		return
	}
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Scale(1.0/windowScale, 1.0/windowScale)
	op.Filter = ebiten.FilterLinear
	screen.DrawImage(g.frame, op)
}

func (g *game) Layout(_, _ int) (int, int) {
	return ui.Width / windowScale, ui.Height / windowScale
}

func main() {
	source := flag.String("source", "auto", "data source: auto | live | openf1")
	replay := flag.String("replay", "", "replay a recorded fastf1 save file instead of connecting")
	speed := flag.Float64("speed", 60, "replay speed factor")
	minRedraw := flag.Duration("min-redraw", 300*time.Millisecond, "minimum interval between redraws")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := &game{ctx: ctx}
	g.a = app.New(app.Config{
		Source:      *source,
		OpenF1Key:   os.Getenv("OPENF1_API_KEY"),
		FileReplay:  *replay,
		ReplaySpeed: *speed,
		MinRedraw:   *minRedraw,
		OnExit:      func() { os.Exit(0) },
	}, g)

	go g.a.Run(ctx)

	ebiten.SetWindowSize(ui.Width/windowScale, ui.Height/windowScale)
	ebiten.SetWindowTitle("Pit Wall — reMarkable simulator")
	if err := ebiten.RunGame(g); err != nil {
		log.Fatal(err)
	}
}
