// icon generates the AppLoad launcher icon. AppLoad draws icon.png at
// 150x150 on a Kaleido e-ink panel, so every candidate here is flat, two-tone
// and built from shapes that survive downscaling and dithering: no hairlines,
// no gradients, nothing that depends on colour to be understood.
//
//	go run ./cmd/icon              # write all candidates + icon.png (chequered)
//	go run ./cmd/icon -style track # pick a different one for icon.png
package main

import (
	"flag"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"os"
)

// The source is 256x256: an exact 150 would be sharper still, but 256
// downscales cleanly and leaves room if the launcher grid ever grows.
const size = 256

var (
	black = color.RGBA{0x11, 0x11, 0x11, 0xff}
	white = color.RGBA{0xff, 0xff, 0xff, 0xff}
	clear = color.RGBA{0, 0, 0, 0}
)

func newCanvas() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), image.NewUniform(clear), image.Point{}, draw.Src)
	return img
}

func fillRect(img *image.RGBA, x, y, w, h int, c color.Color) {
	draw.Draw(img, image.Rect(x, y, x+w, y+h), image.NewUniform(c), image.Point{}, draw.Src)
}

func fillCircle(img *image.RGBA, cx, cy, r int, c color.Color) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				img.Set(cx+dx, cy+dy, c)
			}
		}
	}
}

// stroke stamps discs along a segment — the same approach the track map uses.
func stroke(img *image.RGBA, x0, y0, x1, y1 float64, rad int, c color.Color) {
	steps := int(math.Hypot(x1-x0, y1-y0)) + 1
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		fillCircle(img, int(x0+(x1-x0)*t), int(y0+(y1-y0)*t), rad, c)
	}
}

// chequered draws a waved chequered flag: 5x5 cells, each column lifted along
// a sine so the flag reads as cloth rather than a checkerboard. Black cells
// only — the gaps are transparent, so it works on any launcher background.
func chequered() *image.RGBA {
	img := newCanvas()
	const (
		cols, rows = 5, 5
		pad        = 18
		amp        = 13.0
	)
	cw := (size - 2*pad) / cols
	ch := (size - 2*pad - 20) / rows
	for cx := 0; cx < cols; cx++ {
		// Whole column shifts together: a flag ripples across, not per-cell.
		dy := int(amp * math.Sin(float64(cx)/float64(cols)*2*math.Pi))
		for cy := 0; cy < rows; cy++ {
			if (cx+cy)%2 != 0 {
				continue
			}
			fillRect(img, pad+cx*cw, pad+10+cy*ch+dy, cw, ch, black)
		}
	}
	// Pole: a solid bar down the left edge anchors the wave.
	fillRect(img, pad-14, pad-6, 8, size-2*pad+12, black)
	return img
}

// layout is a circuit drawn as control points in unit space (y down): a long
// left-hand straight, a sweep around the bottom, a long right-hander and a
// single kink back to the line. Harmonic ovals just look like blobs at icon
// size, and tight infield loops overshoot into a hook once splined — this is
// as much detail as survives 150px.
var layout = [][2]float64{
	{0.24, 0.10}, {0.12, 0.30}, {0.13, 0.60}, {0.26, 0.84}, {0.50, 0.90},
	{0.72, 0.82}, {0.88, 0.60}, {0.84, 0.36}, {0.66, 0.30}, {0.52, 0.34},
	{0.44, 0.20},
}

// trackPoints samples a closed Catmull-Rom spline through the layout, scaled
// into the canvas with room for the stroke.
func trackPoints(perSeg int) [][2]float64 {
	const pad = 34
	span := float64(size - 2*pad)
	at := func(i int) [2]float64 {
		p := layout[((i%len(layout))+len(layout))%len(layout)]
		return [2]float64{pad + p[0]*span, pad + p[1]*span}
	}
	var pts [][2]float64
	for i := range layout {
		p0, p1, p2, p3 := at(i-1), at(i), at(i+1), at(i+2)
		for k := 0; k < perSeg; k++ {
			t := float64(k) / float64(perSeg)
			t2, t3 := t*t, t*t*t
			var p [2]float64
			for d := 0; d < 2; d++ {
				p[d] = 0.5 * ((2 * p1[d]) +
					(-p0[d]+p2[d])*t +
					(2*p0[d]-5*p1[d]+4*p2[d]-p3[d])*t2 +
					(-p0[d]+3*p1[d]-3*p2[d]+p3[d])*t3)
			}
			pts = append(pts, p)
		}
	}
	return pts
}

// track draws that loop as a thick outline with a start/finish bar across it.
func track() *image.RGBA {
	img := newCanvas()
	pts := trackPoints(18)
	for i := range pts {
		p, q := pts[i], pts[(i+1)%len(pts)]
		stroke(img, p[0], p[1], q[0], q[1], 11, black)
	}
	// Start/finish bar, on the long left-hand straight where there is room
	// for it to read even at 40px.
	best := 0
	for i, p := range pts {
		if p[0] < pts[best][0] {
			best = i
		}
	}
	p, q := pts[best], pts[(best+3)%len(pts)]
	dx, dy := q[0]-p[0], q[1]-p[1]
	l := math.Hypot(dx, dy)
	nx, ny := -dy/l, dx/l // across the track
	stroke(img, p[0]-nx*18, p[1]-ny*18, p[0]+nx*18, p[1]+ny*18, 8, white)
	stroke(img, p[0]-nx*15, p[1]-ny*15, p[0]+nx*15, p[1]+ny*15, 5, black)
	return img
}

// bars draws a timing tower: four leaderboard rows, the leader filled solid,
// each with the position stripe the timing tab uses.
func bars() *image.RGBA {
	img := newCanvas()
	const (
		pad     = 26
		rowH    = 40
		gap     = 16
		stripeW = 16
		border  = 6
	)
	widths := []int{size - 2*pad, size - 2*pad - 26, size - 2*pad - 52, size - 2*pad - 78}
	for i, w := range widths {
		y := pad + i*(rowH+gap)
		fillRect(img, pad, y, stripeW, rowH, black) // team-colour stripe
		x := pad + stripeW + 8
		bw := w - stripeW - 8
		if i == 0 {
			fillRect(img, x, y, bw, rowH, black) // leader: solid
			continue
		}
		fillRect(img, x, y, bw, border, black)
		fillRect(img, x, y+rowH-border, bw, border, black)
		fillRect(img, x, y, border, rowH, black)
		fillRect(img, x+bw-border, y, border, rowH, black)
	}
	return img
}

func save(path string, img *image.RGBA) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	log.Println("wrote", path)
}

func main() {
	style := flag.String("style", "chequered", "icon written to icon.png: chequered | track | bars")
	flag.Parse()

	styles := map[string]func() *image.RGBA{
		"chequered": chequered,
		"track":     track,
		"bars":      bars,
	}
	if err := os.MkdirAll("build", 0o755); err != nil {
		log.Fatal(err)
	}
	for name, draw := range styles {
		save("build/icon_"+name+".png", draw())
	}
	draw, ok := styles[*style]
	if !ok {
		log.Fatalf("unknown style %q", *style)
	}
	save("icon.png", draw())
}
