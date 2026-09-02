// Package ui renders the telemetry screens into an RGBA image sized for the
// reMarkable Paper Pro (1620x2160) and converts it to the RGB565 layout
// qtfb expects. Design targets a color e-ink panel: white background, high
// contrast text, muted accent colors, no gradients.
//
// Three tabs, switched via the bottom tab bar: live timing table, track map
// with car positions, and the race control message log.
package ui

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/sijokun/PitWall/model"
)

// The layout is authored against the reMarkable Paper Pro's panel and scaled
// to whatever screen is actually in use. Every length below is a reference
// number; px() converts one to device pixels.
const (
	RefWidth  = 1620
	RefHeight = 2160
)

// Width and Height are the panel size in pixels, and scale is how much the
// reference layout is stretched to fill it. Set them with SetScreen before
// building a Renderer; they default to the Paper Pro.
var (
	Width  = RefWidth
	Height = RefHeight
	scale  = 1.0
)

// px scales a length authored at the reference size to device pixels.
func px(v int) int { return int(math.Round(float64(v) * scale)) }

// SetScreen adapts the layout to a panel of w x h pixels. The scale is
// uniform (the smaller of the two ratios) so text and circles keep their
// proportions; edge-anchored elements still use the full width and height,
// so a panel of a different aspect gains margin rather than distortion.
// Call it before NewRenderer — the font sizes are baked in at that point.
func SetScreen(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	Width, Height = w, h
	sw := float64(w) / float64(RefWidth)
	sh := float64(h) / float64(RefHeight)
	scale = math.Min(sw, sh)
	applyLayout()
}

// Layout lengths, all derived from the reference design by applyLayout.
var (
	margin int

	tabBarH   int
	tabBarTop int

	closeX0, closeY0 int
	closeX1, closeY1 int

	// Browser top-right button row (right-aligned, indexed from the right).
	btnW, btnGap int

	listRowH int

	// Browse-screen list area (no tab bar on those screens).
	browseTop    int
	browseBottom int
	browseRowMin int

	// Timing table geometry (fixed header height).
	timingRowsTop int
	timingRowH    int
)

func init() { applyLayout() }

// applyLayout recomputes every derived length for the current scale. Widths
// and heights come from px(); positions anchored to an edge are measured from
// the real Width/Height so they hug the panel on any aspect ratio.
func applyLayout() {
	margin = px(60)

	tabBarH = px(130)
	tabBarTop = Height - tabBarH

	closeX0, closeY0 = Width-margin-px(300), margin-px(8)
	closeX1, closeY1 = Width-margin, margin+px(68)

	btnW, btnGap = px(280), px(24)

	listRowH = px(110)

	browseTop = px(300)
	browseBottom = Height - px(130)
	browseRowMin = px(62)

	timingRowsTop = margin + px(236)
	timingRowH = px(78)

	applyPopupLayout()
	applySettingsLayout()
	applyGearLayout()
	applyMapLayout()
	applyWaitLayout()
}

// HitTimingRow maps a touch on the timing tab to a table row index
// (bound-check against the standings length).
func HitTimingRow(x, y int) int {
	if y < timingRowsTop || y >= timingRowsTop+22*timingRowH {
		return -1
	}
	return (y - timingRowsTop) / timingRowH
}

// HitClose reports whether a touch hits the CLOSE button (top-right).
func HitClose(x, y int) bool {
	return x >= closeX0-20 && x <= closeX1+20 && y >= 0 && y <= closeY1+20
}

// settingsExitX is the left edge of the settings-screen EXIT button, sitting
// just left of CLOSE.
func settingsExitX() int { return closeX0 - btnW - 24 }

// HitSettingsExit reports whether a touch hits the settings EXIT button.
func HitSettingsExit(x, y int) bool {
	x0 := settingsExitX()
	return x >= x0-20 && x <= x0+btnW+20 && y >= 0 && y <= closeY1+20
}

// backBtnW is the width of the back-arrow button on the settings screen,
// which leaves a replay and returns to the weekend's session list. The
// replay header itself carries only the settings gear; seeking lives in the
// clock popup.
const backBtnW = 130

// settingsBackX is the left edge of the settings back button, left of EXIT.
func settingsBackX(canExit bool) int {
	if canExit {
		return settingsExitX() - backBtnW - 24
	}
	return closeX0 - backBtnW - 24
}

// HitSettingsBack reports whether a touch hits the settings back arrow.
func HitSettingsBack(x, y int, canExit bool) bool {
	x0 := settingsBackX(canExit)
	return x >= x0-14 && x <= x0+backBtnW+14 && y >= 0 && y <= closeY1+20
}

// HitReplayClock reports whether a touch hits the replay clock in the top
// status strip, which opens the time popup.
func HitReplayClock(x, y int) bool {
	// Only the status strip itself — the title row below it must stay free.
	return x >= margin-20 && x <= margin+700 && y >= 0 && y <= 58
}

// ---- replay time popup ----

// SeekMinutes are the jumps offered in the replay time popup.
var SeekMinutes = []int{-10, -5, -1, 1, 5, 10}

// Time popup geometry: a centred panel with one row of seek buttons.
var (
	timePopW   int
	timePopH   int
	timePopY   int
	seekBtnW   int
	seekBtnH   int
	seekBtnGap int
)

func applyPopupLayout() {
	timePopW = px(1120)
	timePopH = px(420)
	timePopY = px(420)
	seekBtnW = px(160)
	seekBtnH = px(104)
	seekBtnGap = px(20)
}

// timePopRect returns the popup panel's rect.
func timePopRect() (x0, y0, x1, y1 int) {
	x0 = (Width - timePopW) / 2
	return x0, timePopY, x0 + timePopW, timePopY + timePopH
}

// seekBtnRect returns the rect of seek button i (index into SeekMinutes).
func seekBtnRect(i int) (x0, y0, x1, y1 int) {
	px0, py0, _, _ := timePopRect()
	row := len(SeekMinutes)*seekBtnW + (len(SeekMinutes)-1)*seekBtnGap
	x0 = px0 + (timePopW-row)/2 + i*(seekBtnW+seekBtnGap)
	y0 = py0 + 210
	return x0, y0, x0 + seekBtnW, y0 + seekBtnH
}

// HitSeekButton maps a touch to a SeekMinutes index, or -1.
func HitSeekButton(x, y int) int {
	for i := range SeekMinutes {
		x0, y0, x1, y1 := seekBtnRect(i)
		if x >= x0 && x <= x1 && y >= y0 && y <= y1 {
			return i
		}
	}
	return -1
}

// InTimePopup reports whether a touch landed inside the popup panel (taps
// outside it close the popup).
func InTimePopup(x, y int) bool {
	x0, y0, x1, y1 := timePopRect()
	return x >= x0 && x <= x1 && y >= y0 && y <= y1
}

// browserBtnX returns the left edge of button i counted from the right.
func browserBtnX(i int) int {
	return Width - margin - (i+1)*btnW - i*btnGap
}

// HitHeaderButton maps a touch to one of the top-right header buttons,
// indexed from the right (0 = rightmost) to match the Buttons slices the
// browse and waiting screens are drawn from. Returns -1 for a miss.
func HitHeaderButton(x, y int) int {
	if y < closeY0-20 || y > closeY1+20 {
		return -1
	}
	for i := 0; i < 3; i++ {
		x0 := browserBtnX(i)
		if x >= x0-10 && x <= x0+btnW+10 {
			return i
		}
	}
	return -1
}

// SpeedOptions are the replay speeds offered on the settings screen.
var SpeedOptions = []float64{1, 2, 5, 10, 30, 60, 120}

// DelaySteps back the live-delay stepper: a row of -/+ buttons around the
// current value, so any number of seconds is reachable rather than a fixed
// set. The zero entry is the value readout itself, which is not a button.
var DelaySteps = []int{-10, -5, -1, 0, 1, 5, 10}

// DelayValueIndex is the position of the value readout within DelaySteps.
const DelayValueIndex = 3

// DelayMaxSeconds caps the live delay. Feed events are held in memory until
// they are due, so an unbounded delay would grow the pending buffer without
// limit; 10 minutes is far beyond any real broadcast lag.
const DelayMaxSeconds = 600

// ClampDelay keeps a delay in seconds within the supported range.
func ClampDelay(sec int) int {
	if sec < 0 {
		return 0
	}
	if sec > DelayMaxSeconds {
		return DelayMaxSeconds
	}
	return sec
}

// SetStripe overrides the row-shading gray.
func SetStripe(v uint8) { stripe = color.RGBA{v, v, v, 0xff} }

// RedrawOptions are the screen-redraw interval choices (seconds).
var RedrawOptions = []int{1, 3, 5, 10}

// Grayscale desaturates an RGBA pixel buffer in place (Rec. 601 luma) — used
// for full-screen B&W, which the e-ink panel refreshes faster than color.
func Grayscale(pix []uint8) {
	for i := 0; i+3 < len(pix); i += 4 {
		l := uint8((uint32(pix[i])*77 + uint32(pix[i+1])*150 + uint32(pix[i+2])*29) >> 8)
		pix[i], pix[i+1], pix[i+2] = l, l, l
	}
}

// HeaderLineLabels back the header-line chips: the session's venue, or the
// newest race control message.
var HeaderLineLabels = []string{"Session", "Last message"}

// SessionModeLabels back the session-mode chips: follow the live feed, or
// browse and replay a past session from OpenF1.
var SessionModeLabels = []string{"Live", "Replay"}

// Setting identifies a control tapped on the settings screen.
type Setting int

const (
	SettingNone Setting = iota
	SettingSessionMode
	SettingHeaderLine
	SettingBestSectors
	SettingDelay
	SettingSpeed
	SettingMarker
	SettingTracking
	SettingTcam
	SettingMono
	SettingRedraw
	SettingFullBW
	SettingMiniSectors
	SettingTimeMode
	SettingMapMini
)

// SettingsView carries the current settings values into RenderSettings.
type SettingsView struct {
	SessionMode     int  // 0 = live, 1 = replay
	HeaderRC        bool // header line shows the newest race control message
	ShowBestSectors bool
	MiniSectors     MiniStyle     // mini-sectors under the sector times
	MapMiniSectors  bool          // mini-sector boundary ticks on the map
	TimeMode        TimeMode      // timezone for on-screen clocks
	TrackOffset     time.Duration // circuit GMT offset, for the CLOCKS caption
	DelaySeconds    int
	ReplaySpeed     float64
	Marker          MarkerStyle
	Tracking        TrackingMode
	ShowTcam        bool
	Mono            bool
	RedrawSeconds   int
	FullBW          bool
	LiveDelayShown  bool // a live client exists, so the delay actually applies
	CanExit         bool // show the EXIT button
	CanBack         bool // a replay is open: show the back-to-sessions arrow
	Page            int  // settings page shown when the sections span several
}

// Settings screen geometry: each section is a label + a row of chips + a
// caption. Sections that don't fit on one screen at the reference pitch
// spill onto further pages, flipped with the PREV/NEXT pager at the bottom.
var (
	setSecTop   int // first section's label baseline
	setSecGap   int // vertical distance between section labels
	setChipTop  int // chip row top, below the section label
	setChipW    int
	setChipH    int
	setChipGap  int
	setNoteDy   int // caption baseline below the chip row
	setFitOne   int // sections that fit when there is no pager row
	setFitPager int // sections that fit above the pager row
	setPagerY0  int // pager button row top
	setPagerY1  int // pager button row bottom
)

func applySettingsLayout() {
	setSecTop = px(286)
	setSecGap = px(168)
	setChipTop = px(22)
	setChipW = px(150)
	setChipH = px(66)
	setChipGap = px(22)
	setNoteDy = px(34)
	setPagerY1 = Height - px(24)
	setPagerY0 = setPagerY1 - px(76)

	tail := setChipTop + setChipH + setNoteDy
	setFitOne = 1 + (Height-px(20)-setSecTop-tail)/setSecGap
	setFitPager = 1 + (setPagerY0-px(20)-setSecTop-tail)/setSecGap
	if setFitPager < 1 {
		setFitPager = 1
	}
}

// visibleSettings is the section list for a session mode: the live delay
// only applies to the live feed and the replay speed only to replays, so
// each mode shows its own and hides the other's.
func visibleSettings(sessionMode int) []Setting {
	out := make([]Setting, 0, len(settingsSections))
	for _, s := range settingsSections {
		if (s == SettingDelay && sessionMode != 0) || (s == SettingSpeed && sessionMode == 0) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// settingsPerPage balances n visible sections across the fewest pages that
// fit — all of them on one pager-less screen when possible.
func settingsPerPage(n int) int {
	if n <= setFitOne {
		return n
	}
	pages := (n + setFitPager - 1) / setFitPager
	return (n + pages - 1) / pages
}

// settingsPages is how many pages the sections span in a session mode.
func settingsPages(sessionMode int) int {
	n := len(visibleSettings(sessionMode))
	per := settingsPerPage(n)
	return (n + per - 1) / per
}

// SettingsPages exposes the page count for the app's page stepping.
func SettingsPages(sessionMode int) int { return settingsPages(sessionMode) }

// settingsChipW is the chip width of a control — wider for the few whose
// options are words rather than numbers.
func settingsChipW(kind Setting) int {
	switch kind {
	case SettingSessionMode, SettingHeaderLine:
		return 280
	case SettingTimeMode:
		return 220
	}
	return setChipW
}

// settingsChipRect returns the pixel rect of chip i of the control drawn at
// the given on-screen row. Render and hit-testing share it so they agree.
func settingsChipRect(kind Setting, row, i int) (x0, y0, x1, y1 int) {
	top := setSecTop + row*setSecGap + setChipTop
	w := settingsChipW(kind)
	x0 = margin + i*(w+setChipGap)
	return x0, top, x0 + w, top + setChipH
}

// settingsSections is every settings control, top to bottom; visibleSettings
// filters it per session mode.
var settingsSections = []Setting{
	SettingSessionMode, SettingHeaderLine, SettingBestSectors, SettingMiniSectors, SettingTimeMode,
	SettingDelay, SettingSpeed, SettingMarker, SettingTracking, SettingMapMini, SettingTcam,
	SettingMono, SettingRedraw, SettingFullBW,
}

func settingsChipCount(kind Setting) int {
	switch kind {
	case SettingSessionMode:
		return len(SessionModeLabels)
	case SettingHeaderLine:
		return len(HeaderLineLabels)
	case SettingDelay:
		return len(DelaySteps)
	case SettingSpeed:
		return len(SpeedOptions)
	case SettingMarker:
		return len(MarkerLabels)
	case SettingTracking:
		return len(TrackingLabels)
	case SettingRedraw:
		return len(RedrawOptions)
	case SettingTimeMode:
		return len(TimeModeLabels)
	case SettingMiniSectors:
		return len(MiniStyleLabels)
	}
	return 2 // the toggles: ON/OFF, Color/B&W, Off/On
}

// HitSettingsControl maps a touch to a settings control and option index,
// among the sections visible on the given page in the given session mode.
func HitSettingsControl(x, y, page, sessionMode int) (Setting, int) {
	vis := visibleSettings(sessionMode)
	per := settingsPerPage(len(vis))
	for idx := page * per; idx < len(vis) && idx < (page+1)*per; idx++ {
		kind := vis[idx]
		for i := 0; i < settingsChipCount(kind); i++ {
			x0, y0, x1, y1 := settingsChipRect(kind, idx-page*per, i)
			if x >= x0 && x <= x1 && y >= y0 && y <= y1 {
				return kind, i
			}
		}
	}
	return SettingNone, -1
}

// HitSettingsPager maps a touch to the settings pager: -1 = PREV, +1 = NEXT,
// 0 = neither (always 0 when everything fits on one page).
func HitSettingsPager(x, y, sessionMode int) int {
	if settingsPages(sessionMode) <= 1 || y < setPagerY0-20 || y > setPagerY1+20 {
		return 0
	}
	switch {
	case x >= margin-20 && x <= margin+btnW+20:
		return -1
	case x >= Width-margin-btnW-20 && x <= Width-margin+20:
		return 1
	}
	return 0
}

// Settings gear icon, top-right of the header on live/map/race-control views.
// gearCY is set so the icon's top edge lines up with the source-text row (its
// baseline is at y=42, so its top is ~20 from the screen top).
var (
	gearCX int
	gearCY int
	gearR  int
)

func applyGearLayout() {
	gearCX = Width - margin - px(18)
	gearCY = px(47)
	gearR = px(20)
}

// HitSettingsGear reports whether a touch hits the header settings gear.
func HitSettingsGear(x, y int) bool {
	return x >= gearCX-44 && x <= gearCX+44 && y >= 0 && y <= gearCY+40
}

// browseRowH is the height of one row when n rows have to fit on the page:
// full height for short lists, shrinking (to a floor) for long ones.
func browseRowH(n int) int {
	if n <= 0 {
		return listRowH
	}
	h := (browseBottom - browseTop) / n
	if h > listRowH {
		h = listRowH
	}
	if h < browseRowMin {
		h = browseRowMin
	}
	return h
}

// BrowseMaxRows is how many of n rows actually fit on one page.
func BrowseMaxRows(n int) int {
	return (browseBottom - browseTop) / browseRowH(n)
}

// HitBrowseRow maps a touch on a browse screen to a row index, or -1. n is
// the number of rows in the list, which sets the row height.
func HitBrowseRow(x, y, n int) int {
	if n <= 0 {
		return -1
	}
	h := browseRowH(n)
	if y < browseTop || y >= browseBottom {
		return -1
	}
	i := (y - browseTop) / h
	if i >= n {
		return -1
	}
	return i
}

type Tab int

const (
	TabTiming Tab = iota
	TabMap
	TabRaceControl
	numTabs
)

var tabNames = [numTabs]string{"TIMING", "MAP", "RACE CONTROL"}

// HitTab returns which tab a touch at (x, y) selects, or -1 if the touch is
// outside the tab bar.
func HitTab(x, y int) Tab {
	if y < tabBarTop {
		return -1
	}
	i := x / (Width / int(numTabs))
	if i < 0 || i >= int(numTabs) {
		return -1
	}
	return Tab(i)
}

type Renderer struct {
	title, h1, row, small, mono, monoSmall, monoTiny font.Face

	// Clock display for the current frame, set at the top of Render so every
	// timestamp on screen agrees.
	clockMode   TimeMode
	clockOffset time.Duration // track's GMT offset for TimeTrack

	// segAnim interpolates Sector-mode car markers between mini-segment
	// boundaries, so dots glide along the track instead of teleporting on
	// each crossing.
	segAnim map[int]*segAnimState
}

type segAnimState struct {
	count  int
	at     time.Time // state clock when the completed-segment count changed
	sawLap bool      // has ever had segments this session — a car back at
	// count 0 crossed start/finish and is gliding into its next lap
}

// A car crossing no mini-segment for this long is position-uncertain (ringed
// with a "?"), and after the longer threshold dropped from the map.
const (
	microStallWarn = 30 * time.Second
	microStallHide = 3 * time.Minute
)

// segProgress estimates how many mini-segments past its last confirmed
// crossing a car has gone (e.g. 1.4 = mid-way through the second one):
// elapsed time (on the state's clock, so replay speed is respected) since
// the completed-segment count last advanced, against the expected time per
// segment. Prediction runs a few segments deep so the marker keeps moving
// on every refresh even when the data lags, then holds until a real
// crossing (or the stall "?" at 30s) resolves it.
func (r *Renderer) segProgress(num, cseg int, now time.Time, perSeg float64) float64 {
	a := r.segAnim[num]
	if a == nil {
		a = &segAnimState{count: cseg, at: now}
		r.segAnim[num] = a
	}
	if cseg > 0 {
		a.sawLap = true
	}
	if a.count != cseg {
		a.count, a.at = cseg, now
		return 0
	}
	if perSeg <= 0 {
		return 0
	}
	f := now.Sub(a.at).Seconds() / perSeg
	if f < 0 {
		return 0
	}
	if f > 2.9 {
		f = 2.9
	}
	return f
}

// clock converts a timestamp into the timezone the clocks setting selects.
func (r *Renderer) clock(t time.Time) time.Time {
	switch r.clockMode {
	case TimeTrack:
		return t.In(time.FixedZone("track", int(r.clockOffset/time.Second)))
	case TimeUTC:
		return t.UTC()
	}
	return t.Local()
}

func mustFace(ttf []byte, size float64) font.Face {
	f, err := opentype.Parse(ttf)
	if err != nil {
		panic(err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err)
	}
	return face
}

// NewRenderer builds the faces at the current scale, so call SetScreen first
// when targeting a panel other than the Paper Pro.
func NewRenderer() *Renderer {
	return &Renderer{
		title:     mustFace(gobold.TTF, 72*scale),
		h1:        mustFace(gobold.TTF, 44*scale),
		row:       mustFace(goregular.TTF, 40*scale),
		small:     mustFace(goregular.TTF, 30*scale),
		mono:      mustFace(gomono.TTF, 40*scale),
		monoSmall: mustFace(gomono.TTF, 30*scale),
		monoTiny:  mustFace(gomono.TTF, 24*scale),
		segAnim:   map[int]*segAnimState{},
	}
}

var (
	black = color.RGBA{0x11, 0x11, 0x11, 0xff}
	gray  = color.RGBA{0x66, 0x66, 0x66, 0xff}
	light = color.RGBA{0xdd, 0xdd, 0xdd, 0xff}
	white = color.RGBA{0xff, 0xff, 0xff, 0xff}
	// Saturated variants: the Gallery e-ink panel washes out muted tones,
	// so timing colors need full saturation to stay distinguishable.
	green  = color.RGBA{0x00, 0xff, 0x00, 0xff}
	purple = color.RGBA{0xff, 0x00, 0xff, 0xff}
	yellow = color.RGBA{0xe0, 0x66, 0x00, 0xff} // orange: pure yellow is unreadable on white
	// 0xdd = lightest gray that doesn't trigger a flash on bw panels
	stripe = color.RGBA{0xdd, 0xdd, 0xdd, 0xff}
	// tcamYellow is the onboard-camera yellow, drawn on a dark outline so it
	// stays visible over any team color.
	tcamYellow = color.RGBA{0xf5, 0xc8, 0x00, 0xff}
)

// yellowTcam marks drivers running the yellow onboard (T-cam) camera — used to
// tell teammates apart. The others run the black camera (no mark).
var yellowTcam = map[int]bool{
	1: true, 12: true, 44: true, 6: true, 55: true, 87: true,
	14: true, 30: true, 43: true, 5: true, 77: true,
}

func compoundColor(c string) color.RGBA {
	switch strings.ToUpper(c) {
	case "SOFT":
		return color.RGBA{0xc0, 0x22, 0x22, 0xff}
	case "MEDIUM":
		return color.RGBA{0xc8, 0xa4, 0x00, 0xff}
	case "HARD":
		return color.RGBA{0x55, 0x55, 0x55, 0xff}
	case "INTERMEDIATE":
		return color.RGBA{0x2a, 0x8a, 0x2a, 0xff}
	case "WET":
		return color.RGBA{0x22, 0x55, 0xbb, 0xff}
	}
	return gray
}

func teamColor(hex string) color.RGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(strings.TrimPrefix(hex, "#"), "%02x%02x%02x", &r, &g, &b); err != nil {
		return gray
	}
	return color.RGBA{r, g, b, 0xff}
}

func (r *Renderer) text(img *image.RGBA, face font.Face, col color.Color, x, y int, s string) {
	d := font.Drawer{Dst: img, Src: image.NewUniform(col), Face: face, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

func (r *Renderer) textRight(img *image.RGBA, face font.Face, col color.Color, right, y int, s string) {
	w := font.MeasureString(face, s).Ceil()
	r.text(img, face, col, right-w, y, s)
}

func (r *Renderer) textCenter(img *image.RGBA, face font.Face, col color.Color, cx, y int, s string) {
	w := font.MeasureString(face, s).Ceil()
	r.text(img, face, col, cx-w/2, y, s)
}

// fitFace picks the largest heading face that fits s into avail pixels.
func (r *Renderer) fitFace(s string, avail int) font.Face {
	if font.MeasureString(r.title, s).Ceil() <= avail {
		return r.title
	}
	return r.h1
}

// fitText ellipsizes s to avail pixels in the face fitFace picks for it.
func (r *Renderer) fitText(s string, avail int) string {
	face := r.fitFace(s, avail)
	if font.MeasureString(face, s).Ceil() <= avail {
		return s
	}
	rs := []rune(s)
	for len(rs) > 0 && font.MeasureString(face, strings.TrimSpace(string(rs))+"...").Ceil() > avail {
		rs = rs[:len(rs)-1]
	}
	return strings.TrimSpace(string(rs)) + "..."
}

func fillRect(img *image.RGBA, x, y, w, h int, col color.Color) {
	draw.Draw(img, image.Rect(x, y, x+w, y+h), image.NewUniform(col), image.Point{}, draw.Src)
}

func fillCircle(img *image.RGBA, cx, cy, r int, col color.Color) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				img.Set(cx+dx, cy+dy, col)
			}
		}
	}
}

// drawLine stamps discs along the segment — good enough for track outlines.
func drawLine(img *image.RGBA, x0, y0, x1, y1 float64, rad int, col color.Color) {
	dist := math.Hypot(x1-x0, y1-y0)
	steps := int(dist/2) + 1
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		fillCircle(img, int(x0+(x1-x0)*t), int(y0+(y1-y0)*t), rad, col)
	}
}

// Render draws the full screen for the given state and active tab.
// trackMap may be nil (not fetched yet / unavailable).
func (r *Renderer) Render(st model.State, tab Tab, trackMap *model.TrackMap, popupDriver int, opts ViewOptions) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), image.NewUniform(white), image.Point{}, draw.Src)

	r.clockMode, r.clockOffset = opts.TimeMode, st.TrackOffset

	bodyTop := r.header(img, st, opts)

	switch tab {
	case TabMap:
		r.renderMap(img, st, trackMap, bodyTop, opts)
	case TabRaceControl:
		r.renderRaceControl(img, st, bodyTop, opts.RCPage)
	default:
		r.renderTiming(img, st, bodyTop, opts.ShowTcam, opts.Mini)
	}

	if popupDriver != 0 && tab == TabTiming {
		r.drawLapPopup(img, st, trackMap, popupDriver, opts.PopupLap)
	}
	r.tabBar(img, tab)
	if opts.TimePopup && st.IsReplay {
		r.drawTimePopup(img, st)
	}
	return img
}

// lapPopupGeom is the driver popup's layout, computed identically by drawing
// and hit-testing so taps land on what is shown. sel is the selected past
// lap whose map is open (0 = the lap in progress).
type lapPopupGeom struct {
	x0, x1, y0 int
	panelH     int
	rows       int // history rows shown
	mapH       int // 0 = no lap map
	mapSecs    [3]model.Sector
	mapLabel   string
}

func lapPopupLayout(st model.State, tm *model.TrackMap, num, sel int) lapPopupGeom {
	g := lapPopupGeom{mapLabel: "THIS LAP"}
	for i := range st.Standings {
		if st.Standings[i].Number == num {
			g.mapSecs = st.Standings[i].Sectors
			break
		}
	}
	hist := st.LapHistory[num]
	if sel > 0 {
		for _, rec := range hist {
			if rec.Lap == sel {
				g.mapSecs = rec.Sectors
				g.mapLabel = fmt.Sprintf("LAP %d", sel)
				break
			}
		}
	}
	if tm != nil && len(tm.Points) >= 2 {
		for _, s := range g.mapSecs {
			if len(s.Segments) > 0 {
				g.mapH = px(500)
				break
			}
		}
	}
	g.rows = len(hist)
	if max := (tabBarTop - px(220) - px(150) - g.mapH - px(40)) / px(56); g.rows > max {
		g.rows = max
	}
	if g.rows > 18 {
		g.rows = 18
	}
	if g.rows < 1 {
		g.rows = 1 // room for the "no laps" note
	}
	g.x0, g.x1 = px(220), Width-px(220)
	g.panelH = px(150) + g.rows*px(56) + g.mapH + px(40)
	g.y0 = (tabBarTop - g.panelH) / 2
	if g.y0 < px(200) {
		g.y0 = px(200)
	}
	return g
}

// HitLapPopupRow maps a tap inside the driver popup to the lap number of the
// history row it landed on (0 = none). sel must be the currently selected
// lap, since selection shifts the layout the same way it does when drawing.
func HitLapPopupRow(st model.State, tm *model.TrackMap, num, sel, x, y int) int {
	g := lapPopupLayout(st, tm, num, sel)
	hist := st.LapHistory[num]
	if len(hist) == 0 || x < g.x0 || x > g.x1 {
		return 0
	}
	top := g.y0 + 130 + px(18)
	if y < top {
		return 0
	}
	k := (y - top) / px(56)
	if k >= g.rows {
		return 0
	}
	i := len(hist) - 1 - k
	if i < 0 || hist[i].Lap <= 0 {
		return 0
	}
	return hist[i].Lap
}

// drawLapPopup overlays a panel with the driver's recent laps and, when the
// track outline and mini-segments are in hand, a lap painted onto the map —
// where the driver was fast and slow. sel picks a past lap for the map
// (0 = the lap in progress); tapping a history row selects it.
func (r *Renderer) drawLapPopup(img *image.RGBA, st model.State, tm *model.TrackMap, num, sel int) {
	var drv *model.Standing
	for i := range st.Standings {
		if st.Standings[i].Number == num {
			drv = &st.Standings[i]
			break
		}
	}
	hist := st.LapHistory[num]
	g := lapPopupLayout(st, tm, num, sel)
	rows, mapH := g.rows, g.mapH
	x0, x1, y0, panelH := g.x0, g.x1, g.y0, g.panelH

	// Panel with border.
	fillRect(img, x0-6, y0-6, x1-x0+12, panelH+12, black)
	fillRect(img, x0, y0, x1-x0, panelH, white)

	name := fmt.Sprintf("#%d", num)
	colour := ""
	if drv != nil {
		name = drv.Acronym
		colour = drv.TeamColour
	}
	fillRect(img, x0+30, y0+28, 16, 56, teamColor(colour))
	r.text(img, r.h1, black, x0+70, y0+70, fmt.Sprintf("%s — LAST LAPS", name))
	hint := "tap to close"
	if mapH > 0 && len(hist) > 0 {
		hint = "tap a lap for its map · outside closes"
	}
	r.textRight(img, r.small, gray, x1-30, y0+70, hint)

	var (
		cLap  = px(120)
		cS1   = px(420)
		cS2   = px(620)
		cS3   = px(820)
		cTime = px(1100)
	)
	hy := y0 + 130
	r.text(img, r.small, gray, x0+cLap-60, hy, "LAP")
	r.textRight(img, r.small, gray, x0+cS1, hy, "S1")
	r.textRight(img, r.small, gray, x0+cS2, hy, "S2")
	r.textRight(img, r.small, gray, x0+cS3, hy, "S3")
	r.textRight(img, r.small, gray, x0+cTime, hy, "TIME")

	if len(hist) == 0 {
		r.text(img, r.row, gray, x0+60, hy+70, "No completed laps yet.")
	}
	// Newest first.
	y := hy + 56
	for i := len(hist) - 1; i >= 0 && len(hist)-1-i < rows; i-- {
		rec := hist[i]
		if (len(hist)-1-i)%2 == 1 {
			fillRect(img, x0+16, y-38, x1-x0-32, 52, stripe)
		}
		if sel > 0 && rec.Lap == sel {
			// Frame the lap whose map is open below.
			bx, by, bw, bh := x0+16, y-38, x1-x0-32, 52
			fillRect(img, bx, by, bw, 3, black)
			fillRect(img, bx, by+bh-3, bw, 3, black)
			fillRect(img, bx, by, 3, bh, black)
			fillRect(img, bx+bw-3, by, 3, bh, black)
		}
		r.text(img, r.monoSmall, black, x0+cLap-60, y, fmt.Sprint(rec.Lap))
		for si, right := range []int{cS1, cS2, cS3} {
			sec := rec.Sectors[si]
			col := color.Color(yellow)
			switch {
			case sec.OverallBest:
				col = purple
			case sec.PersonalBest:
				col = green
			case sec.Value == "":
				col = gray
			}
			v := sec.Value
			if v == "" {
				v = "--"
			}
			r.textRight(img, r.monoSmall, col, x0+right, y, v)
		}
		tcol := color.Color(yellow)
		switch {
		case rec.OverallBest:
			tcol = purple
		case rec.PersonalBest:
			tcol = green
		case rec.Time == "":
			tcol = gray
		}
		tv := rec.Time
		if tv == "" {
			tv = "-:--.-"
		}
		r.textRight(img, r.monoSmall, tcol, x0+cTime, y, tv)
		y += px(56)
	}

	if mapH > 0 {
		my0 := y0 + px(150) + rows*px(56) + px(20)
		r.text(img, r.small, gray, x0+30, my0+px(20), g.mapLabel+" — mini-sector pace on track")
		r.drawLapTrack(img, tm, g.mapSecs, st.SectorSegments,
			x0+px(50), my0+px(50), x1-px(50), my0+mapH-px(40))
	}
}

// drawLapTrack paints the track outline with a driver's mini-segment
// statuses for the lap in progress — purple/green/orange where set, the
// plain light outline where the car hasn't been yet — fitted into the given
// rectangle with the same rotation as the map tab.
func (r *Renderer) drawLapTrack(img *image.RGBA, tm *model.TrackMap, secs [3]model.Sector, counts [3]int, rx0, ry0, rx1, ry1 int) {
	// The lap's statuses on the session's full mini-sector grid, S1 through
	// S3, zero-padded where a sector hasn't been revealed.
	var statuses []int
	for i, s := range secs {
		seg := patchSegmentHoles(s.Segments)
		n := counts[i]
		if len(seg) > n {
			n = len(seg)
		}
		for j := 0; j < n; j++ {
			v := 0
			if j < len(seg) {
				v = seg[j]
			}
			statuses = append(statuses, v)
		}
	}
	total := len(statuses)
	if total == 0 || tm == nil || len(tm.Points) < 2 {
		return
	}

	rot := tm.Rotation * math.Pi / 180
	sin, cos := math.Sin(rot), math.Cos(rot)
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	pts := make([][2]float64, len(tm.Points))
	for i, p := range tm.Points {
		x, y := p[0]*cos-p[1]*sin, p[0]*sin+p[1]*cos
		pts[i] = [2]float64{x, y}
		minX, maxX = math.Min(minX, x), math.Max(maxX, x)
		minY, maxY = math.Min(minY, y), math.Max(maxY, y)
	}
	scale := math.Min(float64(rx1-rx0)/(maxX-minX), float64(ry1-ry0)/(maxY-minY))
	offX := float64(rx0+rx1)/2 - (minX+maxX)/2*scale
	offY := float64(ry0+ry1)/2 + (minY+maxY)/2*scale
	n := len(pts)
	sx := make([]float64, n)
	sy := make([]float64, n)
	for i, p := range pts {
		sx[i], sy[i] = p[0]*scale+offX, -p[1]*scale+offY
	}
	cumAt := make([]float64, n)
	for i := 1; i < n; i++ {
		cumAt[i] = cumAt[i-1] + math.Hypot(sx[i]-sx[i-1], sy[i]-sy[i-1])
	}
	length := cumAt[n-1]
	if length <= 0 {
		return
	}

	// statusAt maps a lap-distance fraction to the covering mini-segment:
	// through the circuit's mini-sector boundary table when it is known
	// (segments are ~equal-time, not equal-length), else proportionally.
	statusAt := func(f float64) int {
		idx := 0
		if mini := tm.MiniSectors; len(mini) > 0 {
			k := 0
			for k+1 < len(mini) && mini[k+1] <= f {
				k++
			}
			idx = k * total / len(mini)
		} else {
			idx = int(f * float64(total))
		}
		if idx >= total {
			idx = total - 1
		}
		return statuses[idx]
	}
	for i := 1; i < n; i++ {
		f := (cumAt[i-1] + cumAt[i]) / 2 / length
		col, rad := color.Color(light), 7
		if s := statusAt(f); s != 0 {
			col, rad = segmentColor(s), 9
		}
		drawLine(img, sx[i-1], sy[i-1], sx[i], sy[i], rad, col)
	}
	// Start/finish tick.
	r.drawSectorMarker(img, sx, sy, cumAt, 0)
}

// header draws the shared title area; returns the y where the body starts.
func (r *Renderer) header(img *image.RGBA, st model.State, opts ViewOptions) int {
	title := "PIT WALL"
	sub := "waiting for session data..."
	if s := st.Session; s != nil {
		title = fmt.Sprintf("%s — %s", strings.ToUpper(s.Circuit), strings.ToUpper(s.SessionName))
		sub = fmt.Sprintf("%s, %s · %d", s.Location, s.Country, s.Year)
	}
	if st.MeetingName != "" {
		name := strings.ToUpper(st.MeetingName)
		if s := st.Session; s != nil {
			title = fmt.Sprintf("%s — %s", name, strings.ToUpper(s.SessionName))
		} else {
			title = name
		}
	}

	// With the header line set to race control, the newest message takes the
	// two big lines and the session name moves up into the status strip.
	rcMsg, rcFlag := "", ""
	if opts.HeaderRC {
		if rcMsg, rcFlag = r.latestRC(st); rcMsg == "" {
			rcMsg = "No race control messages yet"
		}
	}

	// Source + clock strip at the very top. During a replay the clock is the
	// handle for the time popup, so it is drawn dark with a hint after it.
	switch {
	case st.Err != "":
		status := "error: " + st.Err
		if len(status) > 80 {
			status = status[:80]
		}
		r.text(img, r.small, gray, margin, px(42), status)
	case st.IsReplay:
		x := margin
		pre := "source: " + st.Source + " · "
		r.text(img, r.small, gray, x, px(42), pre)
		x += font.MeasureString(r.small, pre).Ceil()
		clock := r.clock(st.LastUpdate).Format("15:04:05")
		r.text(img, r.monoSmall, black, x, px(42), clock)
		cw := font.MeasureString(r.monoSmall, clock).Ceil()
		fillRect(img, x, px(50), cw, px(2), black) // underline: this is tappable
		r.text(img, r.small, gray, x+cw+px(16), px(42), "· tap to jump")
	default:
		r.text(img, r.small, gray, margin, px(42),
			"source: "+st.Source+" · "+r.clock(st.LastUpdate).Format("15:04:05"))
	}
	if rcMsg != "" {
		// The big lines now carry the message, so keep the session name here.
		id := title
		if s := st.Session; s != nil {
			id += " · " + s.Location
		}
		r.textRight(img, r.small, gray, gearCX-gearR-24, 42, id)
	}

	y := margin + 60
	// Shrink the big line so it clears the top-right elements: the gear (plus
	// the back arrow during a replay) and, when flying, the flag chip.
	right := Width - margin - px(80) // gear
	// Track-status chip, top right. Sectors under yellow imply the flag even
	// when the status feed has not caught up.
	trackFlag := currentFlag(st)
	if trackFlag == "" && len(st.YellowSectors) > 0 {
		trackFlag = "YELLOW"
	}
	if trackFlag != "" {
		right -= font.MeasureString(r.h1, trackFlag).Ceil() + px(72)
	}
	avail := right - margin - px(30)

	// Second line: the venue (or the message's overflow), with the lap
	// counter right-aligned and, under the flag chip, which sectors it is
	// flying in.
	subW := Width - 2*margin
	lap := ""
	if st.LeaderLap > 0 && isRace(st) {
		lap = fmt.Sprintf("LAP %d", st.LeaderLap)
		if st.TotalLaps > 0 {
			lap = fmt.Sprintf("LAP %d/%d", st.LeaderLap, st.TotalLaps)
		}
		subW -= font.MeasureString(r.h1, lap).Ceil() + px(40)
	}
	sectors := yellowSectorLabel(st)
	if sectors != "" {
		subW -= font.MeasureString(r.row, sectors).Ceil() + px(40)
	}

	if rcMsg != "" {
		// Race control mode: flag chip, then the message. Race control is
		// wordy, so step the face down until the whole message fits in the
		// header's two-line box rather than ellipsizing it away.
		chipW := 0
		if rcFlag != "" {
			chipW = font.MeasureString(r.row, rcFlag).Ceil() + px(48)
			fillRect(img, margin, y-px(40), chipW-px(20), px(54), flagColor(rcFlag))
			r.text(img, r.row, white, margin+px(14), y, rcFlag)
		}
		face, lineH, lines := r.fitMessage(rcMsg, avail-chipW, subW)
		for i, line := range lines {
			x := margin
			if i == 0 {
				x += chipW
			}
			r.text(img, face, black, x, y+i*lineH, line)
		}
	} else {
		r.text(img, r.fitFace(title, avail), black, margin, y, r.fitText(title, avail))
		if lines := wrapWidth(r.row, sub, subW, subW, 1); len(lines) > 0 {
			r.text(img, r.row, gray, margin, y+px(56), lines[0])
		}
	}
	y += px(56)
	sx := Width - margin
	if lap != "" {
		r.textRight(img, r.h1, black, sx, y, lap)
		sx -= font.MeasureString(r.h1, lap).Ceil() + px(40)
	}
	if sectors != "" {
		r.textRight(img, r.row, flagColor("YELLOW"), sx, y, sectors)
	}

	fx := Width - margin
	if trackFlag != "" {
		fw := font.MeasureString(r.h1, trackFlag).Ceil() + px(48)
		fillRect(img, fx-fw, margin-px(8), fw, px(76), flagColor(trackFlag))
		r.text(img, r.h1, white, fx-fw+px(24), margin+px(46), trackFlag)
	}
	// Settings gear: same top-right spot in every mode.
	r.drawGear(img, gearCX, gearCY, gearR, gray)

	y += px(46)
	fillRect(img, margin, y, Width-2*margin, px(4), black)
	return headerBodyTop()
}

// headerBodyTop is where tab content starts under the header — the sum of
// header()'s line steps, kept here so pagination can size pages without an
// image to draw on.
func headerBodyTop() int { return margin + 60 + px(56) + px(46) + px(54) }

// ---- Timing tab ----

func (r *Renderer) renderTiming(img *image.RGBA, st model.State, y int, showTcam bool, mini MiniStyle) {
	var (
		colPos  = margin
		colBar  = margin + px(80)
		colDrv  = margin + px(110)
		colTyre = margin + px(330)
		colInt  = margin + px(620) // right edge
		colGap  = margin + px(800) // right edge
		colS1   = margin + px(950) // right edges of the three sector columns
		colS2   = margin + px(1085)
		colS3   = margin + px(1220)
		colLast = Width - margin - px(90) // right edge
		colPit  = Width - margin          // right edge
	)
	race := isRace(st)
	hdr := gray
	r.text(img, r.small, hdr, colPos, y, "POS")
	r.text(img, r.small, hdr, colDrv, y, "DRIVER")
	r.text(img, r.small, hdr, colTyre, y, "TYRE")
	if race {
		r.textRight(img, r.small, hdr, colInt, y, "INT")
		r.textRight(img, r.small, hdr, colGap, y, "GAP")
	} else {
		r.textRight(img, r.small, hdr, colInt, y, "BEST")
		r.textRight(img, r.small, hdr, colGap, y, "GAP")
	}
	r.textRight(img, r.small, hdr, colS1, y, "S1")
	r.textRight(img, r.small, hdr, colS2, y, "S2")
	r.textRight(img, r.small, hdr, colS3, y, "S3")
	// Session overall-best sector time under each sector column label
	// (purple), in the gap between the header and the first row.
	for si, right := range [3]int{colS1, colS2, colS3} {
		if v := st.BestSectors[si]; v != "" {
			r.textRight(img, r.monoTiny, purple, right, y+px(34), v)
		}
	}
	r.textRight(img, r.small, hdr, colLast, y, "LAST")
	// Session overall-best lap under the LAST label, matching the per-sector
	// bests to its left. The FL marker on the row says who is holding it.
	if st.BestLap != "" {
		r.textRight(img, r.monoTiny, purple, colLast, y+px(34), st.BestLap)
	}
	if race {
		r.textRight(img, r.small, hdr, colPit, y, "PIT")
	} else {
		r.textRight(img, r.small, hdr, colPit, y, "LAPS")
	}
	y += px(20)

	rowH := timingRowH
	// Draw only the rows that fit above the tab bar, so a shorter panel drops
	// the tail of the field instead of painting over the tabs.
	maxRows := (tabBarTop - y) / rowH
	for i, s := range st.Standings {
		if i >= maxRows {
			break
		}
		ry := y + i*rowH
		base := ry + px(54)
		if i%2 == 1 {
			fillRect(img, margin-px(16), ry+px(6), Width-2*margin+px(32), rowH-px(6), stripe)
		}
		pos := "-"
		if s.Position > 0 {
			pos = fmt.Sprint(s.Position)
		}
		r.text(img, r.h1, black, colPos, base, pos)
		// Team color bar; a yellow frame around it marks the yellow-T-cam
		// driver of the team so teammates are distinguishable here too.
		if showTcam && yellowTcam[s.Number] {
			fillRect(img, colBar-px(5), ry+px(9), px(24), rowH-px(12), tcamYellow)
		}
		fillRect(img, colBar, ry+px(14), px(14), rowH-px(22), teamColor(s.TeamColour))
		nameCol := color.Color(black)
		if s.KnockedOut || s.Retired {
			nameCol = gray
		}
		r.text(img, r.h1, nameCol, colDrv, base, s.Acronym)
		if !strings.HasPrefix(s.Acronym, "#") {
			r.text(img, r.small, gray, colDrv+px(125), base, fmt.Sprint(s.Number))
		}

		if s.Compound != "" {
			cc := compoundColor(s.Compound)
			cx, cy, rad := colTyre+px(20), ry+rowH/2, px(22)
			fillCircle(img, cx, cy, rad, cc)
			letter := s.Compound[:1]
			lw := font.MeasureString(r.h1, letter).Ceil()
			r.text(img, r.h1, white, cx-lw/2, cy+px(15), letter)
			if s.TyreLaps > 0 {
				r.text(img, r.small, gray, colTyre+px(52), base, fmt.Sprintf("%d", s.TyreLaps))
			}
		}

		rowMain := color.Color(black)
		if s.KnockedOut || s.Retired {
			rowMain = gray
		}
		if race {
			interval := s.Interval
			if s.Position == 1 {
				interval = "—"
			}
			if s.InPit {
				interval = "PIT"
			}
			if s.Retired {
				interval = "OUT"
			}
			r.textRight(img, r.monoSmall, rowMain, colInt, base, interval)
			gap := s.GapToLeader
			if s.Position == 1 {
				gap = "—"
			}
			r.textRight(img, r.monoSmall, rowMain, colGap, base, gap)
		} else {
			bestCol := rowMain
			if s.Position == 1 && s.BestLap != "" {
				bestCol = purple
			}
			r.textRight(img, r.monoSmall, bestCol, colInt, base, s.BestLap)
			gap := s.GapToLeader
			switch {
			case s.KnockedOut:
				gap = "OUT"
			case s.InPit:
				gap = "PIT"
			case s.Position == 1:
				gap = "—"
			}
			r.textRight(img, r.monoSmall, rowMain, colGap, base, gap)
		}

		for si, sec := range s.Sectors {
			right := [3]int{colS1, colS2, colS3}[si]
			col := color.Color(black)
			switch {
			case sec.OverallBest:
				col = purple
			case sec.PersonalBest:
				col = green
			case sec.Stale:
				col = gray
			case sec.Value != "":
				col = yellow
			}
			v := sec.Value
			if v == "" {
				v = "--"
			}
			r.textRight(img, r.monoSmall, col, right, base, v)
			if mini == MiniDots {
				drawSegmentDots(img, right, ry, sec.Segments)
			}
		}
		if mini == MiniLine {
			drawSegmentLine(img, colS1-(colS2-colS1), colS3, ry, s.Sectors, st.SectorSegments)
		}

		lapCol := color.Color(black)
		switch {
		case s.OverallBestLap:
			lapCol = purple
		case s.PersonalBest:
			lapCol = green
		case s.LastLap != "":
			lapCol = yellow
		}
		last := s.LastLap
		if last == "" {
			last = "-:--.-"
		}
		r.textRight(img, r.monoSmall, lapCol, colLast, base, last)
		// Fastest lap of the session: a purple dot in the gap between the lap
		// time and the pit count. It stays with the driver as they run slower
		// laps, unlike the purple LAST time, which only marks a lap that was
		// fastest at the moment it was set.
		if st.BestLapBy != 0 && s.Number == st.BestLapBy {
			fillCircle(img, colLast+px(30), ry+rowH/2, px(10), purple)
		}
		if race {
			r.textRight(img, r.small, gray, colPit, base, fmt.Sprint(s.Pits))
		} else {
			r.textRight(img, r.small, gray, colPit, base, fmt.Sprint(s.Laps))
		}
	}
	if len(st.Standings) == 0 {
		r.text(img, r.row, gray, margin, y+80, "No timing data yet for this session.")
	}
}

// drawSegmentDots draws one small dot per mini-segment of a sector, in track
// order, right-aligned under the sector time whose column ends at right.
// ry is the row top; the dots sit in the strip below the time's baseline.
func drawSegmentDots(img *image.RGBA, right, ry int, segs []int) {
	segs = patchSegmentHoles(segs)
	n := len(segs)
	if n == 0 {
		return
	}
	rad := px(4)
	step := 2*rad + px(4)
	// The sector columns are px(135) apart; shrink the pitch if a circuit has
	// more mini-segments than fit at the comfortable spacing.
	if maxW := px(120); n*step > maxW {
		step = maxW / n
	}
	cy := ry + px(68)
	for i, status := range segs {
		cx := right - rad - (n-1-i)*step
		fillCircle(img, cx, cy, rad, segmentColor(status))
	}
}

// drawSegmentLine draws the lap's mini-segments as one continuous segmented
// line spanning the S1–S3 columns from x0 to x1 — left to right around the
// lap, every segment the same width (the F1 mini-sector grid is ~equal-time)
// with a small break at each sector boundary. A sector with no data yet
// keeps its share of the line as placeholders, sized by the session's known
// segment counts, so the bar always reads as the same full lap.
func drawSegmentLine(img *image.RGBA, x0, x1, ry int, secs [3]model.Sector, counts [3]int) {
	var n [3]int
	total := 0
	for i, s := range secs {
		n[i] = len(s.Segments)
		if n[i] == 0 {
			n[i] = counts[i]
		}
		total += n[i]
	}
	if total == 0 {
		return
	}
	gaps := 0
	for _, c := range n {
		if c > 0 {
			gaps++
		}
	}
	if gaps > 0 {
		gaps--
	}
	secGap := px(10)
	w := float64(x1-x0-gaps*secGap) / float64(total)
	y, h := ry+px(62), px(10)
	x := float64(x0)
	for i, s := range secs {
		seg := patchSegmentHoles(s.Segments)
		for j := 0; j < n[i]; j++ {
			status := 0
			if j < len(seg) {
				status = seg[j]
			}
			x2 := x + w
			fillRect(img, int(x)+1, y, int(x2)-int(x)-2, h, segmentColor(status))
			x = x2
		}
		if n[i] > 0 {
			x += float64(secGap)
		}
	}
}

// patchSegmentHoles treats a zero with a later nonzero status in the same
// sector as crossed-but-unreported (OpenF1 leaves the first S1 segment null
// on most laps), so it draws as plain completed rather than a not-yet-run
// gap in the middle of the lap.
func patchSegmentHoles(segs []int) []int {
	last := -1
	for i, s := range segs {
		if s != 0 {
			last = i
		}
	}
	patched := false
	for i := 0; i < last; i++ {
		if segs[i] == 0 {
			patched = true
			break
		}
	}
	if !patched {
		return segs
	}
	out := make([]int, len(segs))
	copy(out, segs)
	for i := 0; i < last; i++ {
		if out[i] == 0 {
			out[i] = 2048
		}
	}
	return out
}

// segmentColor maps a feed mini-segment status to the timing palette: the
// same purple/green/yellow the sector times use, gray for the pit lane, and
// the stripe gray as a placeholder for segments not run yet this lap. Codes
// the feed hasn't documented (e.g. 2052) count as plain completed.
func segmentColor(status int) color.RGBA {
	switch status {
	case 0:
		return stripe // not run yet
	case 2049:
		return green
	case 2051:
		return purple
	case 2064:
		return gray // pit lane
	}
	return yellow // 2048 and other completed variants
}

// isRace reports whether the state describes a race (default when unknown).
func isRace(st model.State) bool {
	return st.SessionType == "" || strings.EqualFold(st.SessionType, "Race")
}

// ---- Map tab: driver filter ----

// Map driver-filter geometry: an ALL/NONE control and a grid of team-colored
// number chips at the top of the map, toggling which cars are drawn.
var (
	mapFilterTop   int // top of the filter strip (= map body top)
	mapFilterBtnW  int
	mapFilterBtnH  int
	mapChipD       int
	mapChipGapX    int
	mapChipGapY    int
	mapChipsPerRow int
	mapChipsTop    int
)

// applyMapLayout also drops the driver chips per row when the panel is too
// narrow to hold the reference count at the scaled chip pitch.
func applyMapLayout() {
	mapFilterTop = timingRowsTop - px(20)
	mapFilterBtnW = px(160)
	mapFilterBtnH = px(56)
	mapChipD = px(54)
	mapChipGapX = px(30)
	mapChipGapY = px(18)
	mapChipsPerRow = 15
	if pitch := mapChipD + mapChipGapX; pitch > 0 {
		if n := (Width - 2*margin + mapChipGapX) / pitch; n < mapChipsPerRow {
			mapChipsPerRow = max(1, n)
		}
	}
	mapChipsTop = mapFilterTop + mapFilterBtnH + px(22)
}

// MapFilter identifies a control tapped in the map driver filter.
type MapFilter int

const (
	MapFilterMiss MapFilter = iota
	MapFilterAll
	MapFilterClear
	MapFilterDriver
)

// MarkerStyle is how a car is drawn on the map.
type MarkerStyle int

const (
	MarkerNumber MarkerStyle = iota // team disc with car number
	MarkerCode                      // team disc with 3-letter code
	MarkerDot                       // plain team dot, no text
)

// MiniStyle selects how mini-sectors are drawn under the sector times.
type MiniStyle int

const (
	MiniOff  MiniStyle = iota
	MiniDots           // a dot cluster under each sector time
	MiniLine           // one continuous segmented line across S1–S3
)

// MiniStyleLabels backs the settings-screen chips.
var MiniStyleLabels = []string{"Off", "Dots", "Line"}

// TimeMode selects the timezone for on-screen clocks (header time, race
// control timestamps, the replay clock).
type TimeMode int

const (
	TimeDevice TimeMode = iota // this device's local timezone
	TimeTrack                  // the circuit's local time (feed GMT offset)
	TimeUTC
)

// TimeModeLabels backs the settings-screen chips.
var TimeModeLabels = []string{"Device", "Track", "UTC"}

// zoneNote describes what a clock mode resolves to right now — the current
// time there plus the zone (name and/or UTC offset) — for the CLOCKS caption.
func zoneNote(mode TimeMode, trackOffset time.Duration) string {
	var t time.Time
	switch mode {
	case TimeTrack:
		t = time.Now().In(time.FixedZone("track", int(trackOffset/time.Second)))
	case TimeUTC:
		t = time.Now().UTC()
	default:
		t = time.Now().Local()
	}
	name, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	utc := fmt.Sprintf("UTC%s%02d:%02d", sign, off/3600, off%3600/60)
	// A real zone abbreviation (CEST, JST) is worth showing; the synthetic
	// "track"/"UTC" names and bare numeric ones are not.
	if name != "" && name != "track" && name != "UTC" && name[0] != '+' && name[0] != '-' {
		return fmt.Sprintf("now %s (%s, %s)", t.Format("15:04"), name, utc)
	}
	return fmt.Sprintf("now %s (%s)", t.Format("15:04"), utc)
}

// TrackingMode selects how car positions on the map are sourced.
type TrackingMode int

const (
	TrackAuto  TrackingMode = iota // GPS when broadcast, else micro-sector
	TrackGPS                       // GPS only
	TrackMicro                     // micro-sector timing only
	TrackOff                       // no car markers
)

// ViewOptions carries the user's view preferences into the renderer: the
// map's driver markers, plus what the header's second line shows.
type ViewOptions struct {
	Hidden    map[int]bool
	Marker    MarkerStyle
	Tracking  TrackingMode
	ShowTcam  bool      // yellow T-cam border on yellow-camera drivers
	Mini      MiniStyle // mini-sectors under the timing tab's sector times
	MapMini   bool      // mini-sector boundary ticks on the map's outline
	Mono      bool      // grayscale markers (faster e-ink refresh)
	HeaderRC  bool      // header subtitle shows the newest race control message
	TimePopup bool      // the replay clock popup is open
	RCPage    int       // race control page (0 = newest messages)
	PopupLap  int       // past lap selected in the driver popup (0 = current)
	TimeMode  TimeMode  // timezone for on-screen clocks
}

// MarkerLabels / TrackingLabels back the settings-screen chips.
var (
	MarkerLabels   = []string{"Number", "Code", "Dot"}
	TrackingLabels = []string{"Auto", "GPS", "Sector", "Off"}
)

// MapDriverNumbers returns the car numbers shown in the filter, grouped by
// team (then number) so teammates sit next to each other, in a stable order.
func MapDriverNumbers(st model.State) []int {
	type dv struct {
		num  int
		team string
	}
	seen := map[int]bool{}
	var ds []dv
	for _, s := range st.Standings {
		if s.Number > 0 && !seen[s.Number] {
			seen[s.Number] = true
			key := s.Team
			if key == "" {
				key = s.TeamColour
			}
			ds = append(ds, dv{s.Number, key})
		}
	}
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].team != ds[j].team {
			return ds[i].team < ds[j].team
		}
		// Within a team, the yellow-T-cam driver comes first, then his teammate.
		if yi, yj := yellowTcam[ds[i].num], yellowTcam[ds[j].num]; yi != yj {
			return yi
		}
		return ds[i].num < ds[j].num
	})
	nums := make([]int, len(ds))
	for i, d := range ds {
		nums[i] = d.num
	}
	return nums
}

func mapChipCenter(i int) (int, int) {
	col, row := i%mapChipsPerRow, i/mapChipsPerRow
	cx := margin + mapChipD/2 + col*(mapChipD+mapChipGapX)
	cy := mapChipsTop + mapChipD/2 + row*(mapChipD+mapChipGapY)
	return cx, cy
}

// mapFilterBottom is the y below the whole filter strip (n = driver count).
func mapFilterBottom(n int) int {
	if n == 0 {
		return mapFilterTop + mapFilterBtnH
	}
	rows := (n + mapChipsPerRow - 1) / mapChipsPerRow
	return mapChipsTop + rows*(mapChipD+mapChipGapY)
}

func mapAllRect() (int, int, int, int) {
	return margin, mapFilterTop, margin + mapFilterBtnW, mapFilterTop + mapFilterBtnH
}
func mapNoneRect() (int, int, int, int) {
	x0 := margin + mapFilterBtnW + 24
	return x0, mapFilterTop, x0 + mapFilterBtnW, mapFilterTop + mapFilterBtnH
}

// HitMapFilter maps a touch to a map-filter control; n is the driver count.
func HitMapFilter(x, y, n int) (MapFilter, int) {
	if ax0, ay0, ax1, ay1 := mapAllRect(); x >= ax0 && x <= ax1 && y >= ay0 && y <= ay1 {
		return MapFilterAll, -1
	}
	if nx0, ny0, nx1, ny1 := mapNoneRect(); x >= nx0 && x <= nx1 && y >= ny0 && y <= ny1 {
		return MapFilterClear, -1
	}
	for i := 0; i < n; i++ {
		cx, cy := mapChipCenter(i)
		dx, dy := x-cx, y-cy
		if dx*dx+dy*dy <= (mapChipD/2+8)*(mapChipD/2+8) {
			return MapFilterDriver, i
		}
	}
	return MapFilterMiss, -1
}

// contrastOn returns black or white, whichever reads better on col.
func contrastOn(c color.RGBA) color.Color {
	if 0.299*float64(c.R)+0.587*float64(c.G)+0.114*float64(c.B) > 150 {
		return black
	}
	return white
}

// drawDriverDot draws a car marker: a team-colored disc labeled per style
// (number, 3-letter code, or a plain dot). T-cam drivers get an extra outer
// ring (yellow, or black in mono); everyone else is a plain disc. mono renders
// grayscale — a white disc with a black outline — which the e-ink panel
// refreshes faster. When not shown it's a hollow gray ring with the number
// (filter chips).
func (r *Renderer) drawDriverDot(img *image.RGBA, cx, cy, rad, num int, code string, teamCol color.RGBA, style MarkerStyle, shown, tcam, mono bool) {
	label := fmt.Sprint(num)
	if style == MarkerCode && code != "" {
		label = code
	}
	drawLabel := func(txt color.Color) {
		switch style {
		case MarkerDot:
			// no label
		case MarkerCode:
			r.textCenter(img, r.monoTiny, txt, cx, cy+8, label)
		default:
			r.textCenter(img, r.small, txt, cx, cy+10, label)
		}
	}

	if !shown {
		fillCircle(img, cx, cy, rad, gray)
		fillCircle(img, cx, cy, rad-4, white)
		r.textCenter(img, r.small, gray, cx, cy+11, fmt.Sprint(num))
		return
	}
	// T-cam ring, drawn just outside the disc.
	if tcam {
		bc := color.Color(tcamYellow)
		if mono {
			bc = black
		}
		fillCircle(img, cx, cy, rad+5, bc)
	}
	if style == MarkerDot {
		c := color.Color(teamCol)
		if mono {
			c = black
		}
		fillCircle(img, cx, cy, rad, c)
		return
	}
	if mono {
		fillCircle(img, cx, cy, rad, black)
		fillCircle(img, cx, cy, rad-4, white)
		drawLabel(black)
		return
	}
	fillCircle(img, cx, cy, rad, teamCol)
	drawLabel(contrastOn(teamCol))
}

// drawMapFilter draws the ALL/NONE controls and driver chips, returning the y
// below the strip so the track can be laid out under it. Chips follow the
// marker style (number/code — Dot falls back to number so they stay
// identifiable), the T-cam border, and mono per settings.
func (r *Renderer) drawMapFilter(img *image.RGBA, st model.State, opts ViewOptions) int {
	nums := MapDriverNumbers(st)
	if len(nums) == 0 {
		return mapFilterTop
	}
	colOf := map[int]string{}
	codeOf := map[int]string{}
	for _, s := range st.Standings {
		colOf[s.Number] = s.TeamColour
		codeOf[s.Number] = s.Acronym
	}
	drawBtn := func(x0, y0, x1, y1 int, label string) {
		fillRect(img, x0, y0, x1-x0, 3, black)
		fillRect(img, x0, y1-3, x1-x0, 3, black)
		fillRect(img, x0, y0, 3, y1-y0, black)
		fillRect(img, x1-3, y0, 3, y1-y0, black)
		r.textCenter(img, r.row, black, (x0+x1)/2, y0+(y1-y0)/2+14, label)
	}
	ax0, ay0, ax1, ay1 := mapAllRect()
	drawBtn(ax0, ay0, ax1, ay1, "ALL")
	nx0, ny0, nx1, ny1 := mapNoneRect()
	drawBtn(nx0, ny0, nx1, ny1, "NONE")
	chipStyle := opts.Marker
	if chipStyle == MarkerDot {
		chipStyle = MarkerNumber
	}
	for i, num := range nums {
		cx, cy := mapChipCenter(i)
		shown := !opts.Hidden[num]
		r.drawDriverDot(img, cx, cy, mapChipD/2, num, codeOf[num], teamColor(colOf[num]),
			chipStyle, shown, opts.ShowTcam && shown && yellowTcam[num], opts.Mono)
	}
	return mapFilterBottom(len(nums))
}

// ---- Map tab ----

func (r *Renderer) renderMap(img *image.RGBA, st model.State, tm *model.TrackMap, y int, opts ViewOptions) {
	filterBottom := r.drawMapFilter(img, st, opts)
	if tm == nil || len(tm.Points) < 2 {
		msg := "Track map not available yet."
		if st.Session == nil {
			msg = "Waiting for session data..."
		}
		r.text(img, r.row, gray, margin, filterBottom+80, msg)
		return
	}

	// Fit the rotated outline into the drawing area below the filter strip.
	areaX0, areaY0 := float64(margin+30), float64(filterBottom+30)
	areaX1, areaY1 := float64(Width-margin-px(30)), float64(tabBarTop-px(100))

	rot := tm.Rotation * math.Pi / 180
	sin, cos := math.Sin(rot), math.Cos(rot)
	rotate := func(x, y float64) (float64, float64) {
		return x*cos - y*sin, x*sin + y*cos
	}

	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	pts := make([][2]float64, len(tm.Points))
	for i, p := range tm.Points {
		x, yy := rotate(p[0], p[1])
		pts[i] = [2]float64{x, yy}
		minX, maxX = math.Min(minX, x), math.Max(maxX, x)
		minY, maxY = math.Min(minY, yy), math.Max(maxY, yy)
	}
	scale := math.Min((areaX1-areaX0)/(maxX-minX), (areaY1-areaY0)/(maxY-minY))
	offX := (areaX0+areaX1)/2 - (minX+maxX)/2*scale
	// Screen y grows downward: flip.
	offY := (areaY0+areaY1)/2 + (minY+maxY)/2*scale
	project := func(x, yy float64) (float64, float64) {
		return x*scale + offX, -yy*scale + offY
	}

	// Project every point once, then measure cumulative track length so the
	// outline can be split into the three timing sectors by distance.
	n := len(pts)
	sx := make([]float64, n)
	sy := make([]float64, n)
	for i, p := range pts {
		sx[i], sy[i] = project(p[0], p[1])
	}
	cumAt := make([]float64, n) // cumulative distance at each point
	for i := 1; i < n; i++ {
		cumAt[i] = cumAt[i-1] + math.Hypot(sx[i]-sx[i-1], sy[i]-sy[i-1])
	}

	// Marshal sectors: which one covers each outline point, so the stretches
	// race control has flagged can be drawn in the flag colour.
	var msx, msy []float64
	var msNum []int
	for _, ms := range tm.MarshalSectors {
		px, py := project(rotate(ms.X, ms.Y))
		msx, msy, msNum = append(msx, px), append(msy, py), append(msNum, ms.Number)
	}
	secOf := marshalSectorOfPoint(sx, sy, msx, msy, msNum)

	// Draw the outline: plain gray, thicker and yellow under a yellow flag.
	for i := 1; i < n; i++ {
		col, rad := color.Color(light), 7
		if secOf != nil && st.YellowSectors[secOf[i]] {
			col, rad = flagColor("YELLOW"), 10
		}
		drawLine(img, sx[i-1], sy[i-1], sx[i], sy[i], rad, col)
	}

	// Sector boundaries. The feed's mini-segment counts, distributed by
	// curvature-weighted distance (corners pack denser, so get more segments
	// per metre), place the boundaries where they really fall; without them
	// fall back to thirds. Each boundary is drawn as a black tick across the
	// track with the two sectors it divides numbered on either side.
	segCounts := st.SectorSegments
	totalSeg := segCounts[0] + segCounts[1] + segCounts[2]
	bounds := segmentBoundaries(sx, sy, cumAt, totalSeg, 0)
	// boundaryPct returns the lap percent of the sector boundary that falls
	// after cumSeg micro-segments. The timing sectors align to the F1
	// mini-sector grid, which is ~equal-time (long on straights, short in
	// corners) — so mapping the cumulative segment fraction onto a mini-sector
	// *index* captures the fast/slow distribution that segment counts alone
	// (and smooth-geometry curvature weighting) get wrong. Falls back to the
	// curvature-weighted split, then to a plain proportional one.
	boundaryPct := func(cumSeg int) float64 {
		if mini := tm.MiniSectors; len(mini) > 0 && totalSeg > 0 {
			mi := int(float64(cumSeg) / float64(totalSeg) * float64(len(mini)))
			if mi >= len(mini) {
				mi = len(mini) - 1
			}
			return mini[mi] * 100
		}
		if bounds != nil {
			return bounds[cumSeg]
		}
		if totalSeg > 0 {
			return float64(cumSeg) / float64(totalSeg) * 100
		}
		return 0
	}
	// Start/finish is at 0%; the other two are the S1|S2 and S2|S3 lines.
	markers := []float64{0, 100.0 / 3, 200.0 / 3}
	if totalSeg > 0 {
		markers = []float64{0, boundaryPct(segCounts[0]), boundaryPct(segCounts[0] + segCounts[1])}
	}
	for _, pct := range markers {
		r.drawSectorMarker(img, sx, sy, cumAt, pct)
	}
	// Mini-sector boundaries as lighter ticks between the sector markers —
	// from the circuit's boundary table when known, else the curvature-
	// weighted segment split.
	if opts.MapMini {
		var pcts []float64
		if mini := tm.MiniSectors; len(mini) > 0 {
			for _, f := range mini {
				pcts = append(pcts, f*100)
			}
		} else if bounds != nil {
			for k := 1; k < totalSeg; k++ {
				pcts = append(pcts, bounds[k])
			}
		}
		for _, pct := range pcts {
			r.drawMiniTick(img, sx, sy, cumAt, pct)
		}
	}

	for _, c := range tm.Corners {
		rx, ry := rotate(c.X, c.Y)
		px, py := project(rx, ry)
		r.textCenter(img, r.monoSmall, gray, int(px), int(py)+10, fmt.Sprint(c.Number))
	}

	// Cars, drawn as team-colored markers (style per settings). The tracking
	// source follows settings: GPS uses the Position feed; Sector places each
	// car by micro-segments completed this lap; Auto prefers GPS once enough
	// cars broadcast coordinates (some 2026 sessions send none) and otherwise
	// falls back to Sector; Off draws nothing. Deselected drivers are skipped.
	const dotR = 28
	const minGPSCars = 5
	gpsCars := 0
	for _, cp := range st.CarPositions {
		if cp.X != 0 || cp.Y != 0 {
			gpsCars++
		}
	}
	note := func(s string) { r.text(img, r.small, gray, margin, tabBarTop-px(70), s) }
	drawGPS := func() int {
		drawn := 0
		for _, cp := range st.CarPositions {
			if opts.Hidden[cp.Number] || (cp.X == 0 && cp.Y == 0) {
				continue
			}
			rx, ry := rotate(cp.X, cp.Y)
			px, py := project(rx, ry)
			col := teamColor(cp.TeamColour)
			if !cp.OnTrack {
				col = gray
			}
			r.drawDriverDot(img, int(px), int(py), dotR, cp.Number, cp.Acronym, col, opts.Marker, true, opts.ShowTcam && yellowTcam[cp.Number], opts.Mono)
			drawn++
		}
		return drawn
	}
	drawMicro := func() int {
		drawn := 0
		if bounds == nil {
			return 0
		}
		for _, s := range st.Standings {
			if opts.Hidden[s.Number] || s.Retired || s.KnockedOut || s.InPit {
				continue
			}
			cseg := s.CompletedSegments
			if cseg >= len(bounds) {
				cseg = len(bounds) - 1
			}
			// Glide toward the next boundary while waiting for the crossing,
			// paced by the driver's own lap time.
			perSeg := 90.0 / float64(max(totalSeg, 1))
			if lap := model.ParseLapSeconds(s.LastLap); lap > 0 && totalSeg > 0 {
				perSeg = lap / float64(totalSeg)
			}
			f := r.segProgress(s.Number, cseg, st.LastUpdate, perSeg)
			// No segments this lap: keep gliding out of start/finish if the
			// car has run laps before (its count just wrapped to zero) —
			// but never park never-seen cars on the line.
			if cseg <= 0 {
				a := r.segAnim[s.Number]
				if a == nil || !a.sawLap {
					continue
				}
				cseg = 0
			}
			// Predicted position: whole predicted segments ahead of the
			// confirmed count, plus a fraction into the next, clamped to
			// stay this side of start/finish until the real wrap arrives.
			idx := cseg + int(f)
			frac := f - float64(int(f))
			if idx >= len(bounds)-1 {
				idx, frac = len(bounds)-2, 0.9
			}
			pct := bounds[idx] + (bounds[idx+1]-bounds[idx])*frac
			// A car that has crossed no segment in a while is somewhere
			// unknown (stopped, garage, red flag): ring it with a "?" badge,
			// and after long enough drop it from the map entirely.
			stall := time.Duration(0)
			if a := r.segAnim[s.Number]; a != nil {
				stall = st.LastUpdate.Sub(a.at)
			}
			if stall > microStallHide {
				continue
			}
			cx, cy := pointAtPercent(sx, sy, cumAt, pct)
			stalled := stall > microStallWarn
			if stalled {
				fillCircle(img, int(cx), int(cy), dotR+8, black)
			}
			r.drawDriverDot(img, int(cx), int(cy), dotR, s.Number, s.Acronym, teamColor(s.TeamColour), opts.Marker, true, opts.ShowTcam && yellowTcam[s.Number], opts.Mono)
			if stalled {
				bx, by := int(cx)+dotR+6, int(cy)-dotR-6
				fillCircle(img, bx, by, 18, black)
				fillCircle(img, bx, by, 15, white)
				r.textCenter(img, r.small, black, bx, by+11, "?")
			}
			drawn++
		}
		return drawn
	}
	switch opts.Tracking {
	case TrackOff:
		note("Driver tracking off (Settings).")
	case TrackGPS:
		if drawGPS() == 0 {
			note("No GPS positions being broadcast this session.")
		}
	case TrackMicro:
		if drawMicro() == 0 {
			note("No live positions yet (session not running).")
		} else {
			note("Positions estimated from sector timing.")
		}
	default: // TrackAuto
		switch {
		case gpsCars >= minGPSCars:
			drawGPS()
		case drawMicro() > 0:
			note("No GPS feed — positions estimated from sector timing.")
		default:
			note("No live positions yet (session not running).")
		}
	}
	// The flagged sectors are named in the header, on every tab; here the
	// coloured stretches of track speak for themselves.
	r.textRight(img, r.small, gray, Width-margin, filterBottom+10, tm.Name)
}

// marshalSectorOfPoint returns, for each projected outline point, the number
// of the marshal sector covering it — nil when the circuit has no marshal
// sector data. msx/msy are the projected sector start positions and msNum
// their numbers; a sector runs from its start to the next one, wrapping.
func marshalSectorOfPoint(sx, sy, msx, msy []float64, msNum []int) []int {
	if len(msx) == 0 || len(sx) == 0 {
		return nil
	}
	type start struct{ idx, num int }
	starts := make([]start, 0, len(msx))
	for k := range msx {
		best, bestD := 0, math.Inf(1)
		for i := range sx {
			dx, dy := sx[i]-msx[k], sy[i]-msy[k]
			if d := dx*dx + dy*dy; d < bestD {
				best, bestD = i, d
			}
		}
		starts = append(starts, start{best, msNum[k]})
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].idx < starts[j].idx })

	out := make([]int, len(sx))
	cur := starts[len(starts)-1].num // before the first boundary: the last sector
	next := 0
	for i := range sx {
		for next < len(starts) && starts[next].idx <= i {
			cur = starts[next].num
			next++
		}
		out[i] = cur
	}
	return out
}

// drawSectorMarker draws a small black tick across the track at the given lap
// percent, marking a sector boundary — sized to sit inside the track line.
func (r *Renderer) drawSectorMarker(img *image.RGBA, sx, sy, arc []float64, pct float64) {
	px, py := pointAtPercent(sx, sy, arc, pct)
	ax, ay := pointAtPercent(sx, sy, arc, pct+0.6)
	bx, by := pointAtPercent(sx, sy, arc, pct-0.6)
	tx, ty := ax-bx, ay-by
	tl := math.Hypot(tx, ty)
	if tl < 1e-6 {
		tx, ty, tl = 1, 0, 1
	}
	tx, ty = tx/tl, ty/tl
	nx, ny := -ty, tx // unit normal across the track
	const half = 7    // track line is ~14px wide, so this stays within it
	drawLine(img, px-nx*half, py-ny*half, px+nx*half, py+ny*half, 4, black)
}

// drawMiniTick marks a mini-sector boundary: a thinner, gray version of the
// sector marker so the grid reads without competing with the S1/S2/S3 ticks.
func (r *Renderer) drawMiniTick(img *image.RGBA, sx, sy, arc []float64, pct float64) {
	px, py := pointAtPercent(sx, sy, arc, pct)
	ax, ay := pointAtPercent(sx, sy, arc, pct+0.6)
	bx, by := pointAtPercent(sx, sy, arc, pct-0.6)
	tx, ty := ax-bx, ay-by
	tl := math.Hypot(tx, ty)
	if tl < 1e-6 {
		tx, ty, tl = 1, 0, 1
	}
	tx, ty = tx/tl, ty/tl
	nx, ny := -ty, tx // unit normal across the track
	const half = 7
	drawLine(img, px-nx*half, py-ny*half, px+nx*half, py+ny*half, 2, gray)
}

// smoothedCurvatures approximates per-point track curvature (via the turn
// angle between adjacent segments), then box-smooths it. Ported from the
// f1-telemetry track-map engine.
func smoothedCurvatures(sx, sy []float64) []float64 {
	n := len(sx)
	raw := make([]float64, n)
	if n < 3 {
		return raw
	}
	for i := 1; i < n-1; i++ {
		ax, ay := sx[i]-sx[i-1], sy[i]-sy[i-1]
		bx, by := sx[i+1]-sx[i], sy[i+1]-sy[i]
		la, lb := math.Hypot(ax, ay), math.Hypot(bx, by)
		if la < 1e-6 || lb < 1e-6 {
			continue
		}
		raw[i] = math.Abs(ax*by-ay*bx) / (la * lb * ((la + lb) / 2))
	}
	raw[0], raw[n-1] = raw[1], raw[n-2]
	const window = 5
	half := window / 2
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		sum, cnt := 0.0, 0
		for j := max(0, i-half); j <= min(n-1, i+half); j++ {
			sum += raw[j]
			cnt++
		}
		out[i] = sum / float64(cnt)
	}
	return out
}

// lerpLookup returns ys interpolated at the point where xs first reaches
// targetX (xs must be ascending).
func lerpLookup(xs, ys []float64, targetX float64) float64 {
	for i := 1; i < len(xs); i++ {
		if xs[i] >= targetX {
			denom := xs[i] - xs[i-1]
			t := 0.0
			if denom > 1e-9 {
				t = (targetX - xs[i-1]) / denom
			}
			return ys[i-1] + t*(ys[i]-ys[i-1])
		}
	}
	return ys[len(ys)-1]
}

// segmentBoundaries splits the track into totalSegments micro-segments and
// returns their totalSegments+1 boundary positions as percent [0,100) of the
// lap, starting at startPct. Segments are distributed by curvature-weighted
// distance so corners (where the field packs together) get more of them —
// this is what makes segment-derived car and sector positions line up with
// reality. Ported from f1-telemetry's computeSegmentBoundaries.
func segmentBoundaries(sx, sy, arc []float64, totalSegments int, startPct float64) []float64 {
	if totalSegments <= 0 || len(sx) < 3 {
		return nil
	}
	totalArc := arc[len(arc)-1]
	if totalArc == 0 {
		return nil
	}
	curv := smoothedCurvatures(sx, sy)
	maxCurv := 1e-6
	for _, c := range curv {
		if c > maxCurv {
			maxCurv = c
		}
	}
	const curvWeight = 4.0
	w := make([]float64, len(sx))
	for i := 1; i < len(sx); i++ {
		ds := arc[i] - arc[i-1]
		avg := (curv[i-1] + curv[i]) / 2
		w[i] = w[i-1] + ds*(1+curvWeight*(avg/maxCurv))
	}
	totalW := w[len(w)-1]
	startW := lerpLookup(arc, w, startPct/100*totalArc)
	out := make([]float64, 0, totalSegments+1)
	for k := 0; k <= totalSegments; k++ {
		targetW := startW + float64(k)/float64(totalSegments)*totalW
		wrapped := targetW
		if targetW > totalW {
			wrapped = targetW - totalW
		}
		pct := lerpLookup(w, arc, wrapped) / totalArc * 100
		if targetW > totalW {
			pct += 100
		}
		out = append(out, pct)
	}
	return out
}

// pointAtPercent returns the (x,y) at the given percent [0,100) of lap arc.
func pointAtPercent(sx, sy, arc []float64, pct float64) (float64, float64) {
	total := arc[len(arc)-1]
	pct = math.Mod(pct, 100)
	if pct < 0 {
		pct += 100
	}
	target := pct / 100 * total
	for i := 1; i < len(arc); i++ {
		if arc[i] >= target {
			denom := arc[i] - arc[i-1]
			t := 0.0
			if denom > 1e-9 {
				t = (target - arc[i-1]) / denom
			}
			return sx[i-1] + t*(sx[i]-sx[i-1]), sy[i-1] + t*(sy[i]-sy[i-1])
		}
	}
	return sx[len(sx)-1], sy[len(sy)-1]
}

// ---- Race control tab ----

// rcTextX is where message text starts, right of the timestamp column.
func rcTextX() int { return margin + px(130) }

// rcPagerRow is the pager button row of the race control tab, sitting just
// above the tab bar.
func rcPagerRow() (y0, y1 int) {
	y1 = tabBarTop - px(24)
	return y1 - px(76), y1
}

// rcChipW is the drawn width of a message's flag chip (0 = no chip).
func (r *Renderer) rcChipW(rc model.RaceControl) int {
	if rc.Flag == "" || rc.Flag == "CLEAR" {
		return 0
	}
	return font.MeasureString(r.small, rc.Flag).Ceil() + 28
}

// rcMessageH is the vertical space one message takes, wrapping included.
func (r *Renderer) rcMessageH(rc model.RaceControl) int {
	x := rcTextX()
	if cw := r.rcChipW(rc); cw > 0 {
		x += cw + 20
	}
	lines := wrapWidth(r.row, rc.Message, Width-margin-x, Width-margin-rcTextX(), 3)
	n := len(lines)
	if n < 1 {
		n = 1
	}
	return 62 + 48*(n-1)
}

// raceControlPageStarts returns the index of the first message on each page,
// newest first. A single screenful means one pager-less page; otherwise the
// pages stop above the pager row.
func (r *Renderer) raceControlPageStarts(st model.State) []int {
	top := headerBodyTop() + px(56)
	limit := tabBarTop - px(80)
	fits := func(lim int) bool {
		y := top
		for _, rc := range st.RaceControl {
			if y > lim {
				return false
			}
			y += r.rcMessageH(rc)
		}
		return true
	}
	if fits(limit) {
		return []int{0}
	}
	pagerY0, _ := rcPagerRow()
	limit = pagerY0 - px(50)
	starts := []int{0}
	y := top
	for i, rc := range st.RaceControl {
		if y > limit {
			starts = append(starts, i)
			y = top
		}
		y += r.rcMessageH(rc)
	}
	return starts
}

// RaceControlPages is how many pages the current messages span — for the
// app's page stepping.
func (r *Renderer) RaceControlPages(st model.State) int {
	return len(r.raceControlPageStarts(st))
}

// HitRaceControlPager maps a touch on the race control tab to the pager:
// -1 = NEWER, +1 = OLDER, 0 = neither. Purely geometric; the app bounds the
// step with RaceControlPages.
func HitRaceControlPager(x, y int) int {
	y0, y1 := rcPagerRow()
	if y < y0-20 || y > y1+20 {
		return 0
	}
	switch {
	case x >= margin-20 && x <= margin+btnW+20:
		return -1
	case x >= Width-margin-btnW-20 && x <= Width-margin+20:
		return 1
	}
	return 0
}

func (r *Renderer) renderRaceControl(img *image.RGBA, st model.State, y int, page int) {
	r.text(img, r.small, gray, margin, y, "RACE CONTROL — LATEST FIRST")
	y += px(56)
	if len(st.RaceControl) == 0 {
		r.text(img, r.row, gray, margin, y+40, "No race control messages yet.")
		return
	}
	starts := r.raceControlPageStarts(st)
	if page < 0 || page >= len(starts) {
		page = 0
	}
	limit := tabBarTop - px(80)
	if len(starts) > 1 {
		pagerY0, _ := rcPagerRow()
		limit = pagerY0 - px(50)
	}
	textX := rcTextX()
	for _, rc := range st.RaceControl[starts[page]:] {
		if y > limit {
			break
		}
		r.text(img, r.monoSmall, gray, margin, y, r.clock(rc.Date).Format("15:04"))
		x := textX
		if cw := r.rcChipW(rc); cw > 0 {
			fillRect(img, x, y-30, cw, 42, flagColor(flagLabel(rc.Flag)))
			r.text(img, r.small, white, x+14, y, rc.Flag)
			x += cw + 20
		}
		lines := wrapWidth(r.row, rc.Message, Width-margin-x, Width-margin-textX, 3)
		for li, line := range lines {
			if li > 0 {
				y += 48
			}
			lx := textX
			if li == 0 {
				lx = x
			}
			r.text(img, r.row, black, lx, y, line)
		}
		y += 62
	}
	if pages := len(starts); pages > 1 {
		y0, y1 := rcPagerRow()
		r.drawPagerButton(img, margin, y0, y1, "< NEWER", page > 0)
		r.drawPagerButton(img, Width-margin-btnW, y0, y1, "OLDER >", page < pages-1)
		r.textCenter(img, r.row, gray, Width/2, y0+(y1-y0)/2+14,
			fmt.Sprintf("PAGE %d/%d", page+1, pages))
	}
}

// wrapWidth word-wraps text by measured pixel width: the first line gets
// firstW, the rest restW. At most maxLines lines; overflow is ellipsized.
func wrapWidth(face font.Face, text string, firstW, restW, maxLines int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := ""
	limit := firstW
	for _, w := range words {
		candidate := w
		if cur != "" {
			candidate = cur + " " + w
		}
		if font.MeasureString(face, candidate).Ceil() <= limit || cur == "" {
			cur = candidate
			continue
		}
		lines = append(lines, cur)
		cur = w
		limit = restW
	}
	lines = append(lines, cur)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		last := lines[maxLines-1]
		for font.MeasureString(face, last+"...").Ceil() > restW && strings.Contains(last, " ") {
			last = last[:strings.LastIndex(last, " ")]
		}
		lines[maxLines-1] = last + "..."
	}
	return lines
}

// ---- Tab bar ----

func (r *Renderer) tabBar(img *image.RGBA, active Tab) {
	fillRect(img, 0, tabBarTop, Width, px(2), black)
	tw := Width / int(numTabs)
	label := tabBarTop + px(82)
	for i := Tab(0); i < numTabs; i++ {
		x := int(i) * tw
		if i == active {
			fillRect(img, x, tabBarTop+px(2), tw, tabBarH-px(2), black)
			r.textCenter(img, r.h1, white, x+tw/2, label, tabNames[i])
		} else {
			r.textCenter(img, r.h1, black, x+tw/2, label, tabNames[i])
		}
		if i > 0 {
			fillRect(img, x, tabBarTop+px(20), px(2), tabBarH-px(40), light)
		}
	}
}

// ---- shared ----

func currentFlag(st model.State) string {
	switch st.TrackStatus {
	case "Yellow":
		return "YELLOW"
	case "SCDeployed", "SCEnding":
		return "SAFETY CAR"
	case "Red":
		return "RED FLAG"
	case "VSCDeployed", "VSCEnding":
		return "VSC"
	case "AllClear":
		return ""
	}
	for _, m := range st.RaceControl {
		switch m.Flag {
		case "RED":
			return "RED FLAG"
		case "YELLOW", "DOUBLE YELLOW":
			return "YELLOW"
		case "CHEQUERED":
			return "FINISH"
		case "GREEN", "CLEAR":
			return ""
		}
		if strings.Contains(m.Message, "SAFETY CAR") {
			return "SAFETY CAR"
		}
	}
	return ""
}

// fitMessage lays a race control message out for the header: the largest
// face whose wrap fits the space (two big lines, or three small ones), the
// line height to use, and the wrapped lines. firstW is the width left of the
// first line by the flag chip, restW that of the lines under it.
func (r *Renderer) fitMessage(msg string, firstW, restW int) (font.Face, int, []string) {
	for _, o := range []struct {
		face  font.Face
		max   int
		lineH int
	}{
		{r.h1, 2, 56},
		{r.row, 2, 52},
		{r.small, 3, 42},
	} {
		lines := wrapWidth(o.face, msg, firstW, restW, o.max)
		if len(lines) < o.max || !strings.HasSuffix(lines[len(lines)-1], "...") {
			return o.face, o.lineH, lines
		}
	}
	// Still too long: keep the smallest face, ellipsized.
	return r.small, 42, wrapWidth(r.small, msg, firstW, restW, 3)
}

// yellowSectorLabel names the marshal sectors currently under yellow, for
// the line under the flag chip ("" when none are).
func yellowSectorLabel(st model.State) string {
	if len(st.YellowSectors) == 0 {
		return ""
	}
	nums := make([]int, 0, len(st.YellowSectors))
	for s := range st.YellowSectors {
		nums = append(nums, s)
	}
	sort.Ints(nums)
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = fmt.Sprint(n)
	}
	label := "SECTOR "
	if len(nums) > 1 {
		label = "SECTORS "
	}
	return label + strings.Join(parts, ", ")
}

// latestRC returns the newest race control message (prefixed with its time,
// in the clocks setting's timezone) and the flag chip to draw before it, if
// any. State.RaceControl is newest first in both data sources.
func (r *Renderer) latestRC(st model.State) (msg, flag string) {
	if len(st.RaceControl) == 0 {
		return "", ""
	}
	m := st.RaceControl[0]
	if m.Flag != "" && m.Flag != "CLEAR" {
		flag = flagLabel(m.Flag)
	}
	return r.clock(m.Date).Format("15:04") + "  " + m.Message, flag
}

func flagLabel(flag string) string {
	switch flag {
	case "RED":
		return "RED FLAG"
	case "YELLOW", "DOUBLE YELLOW":
		return "YELLOW"
	case "CHEQUERED":
		return "FINISH"
	}
	return flag
}

func flagColor(flag string) color.RGBA {
	switch flag {
	case "RED FLAG":
		return color.RGBA{0xb0, 0x20, 0x20, 0xff}
	case "YELLOW", "SAFETY CAR", "VSC":
		return color.RGBA{0xb8, 0x96, 0x00, 0xff}
	case "GREEN":
		return green
	case "BLUE":
		return color.RGBA{0x22, 0x55, 0xbb, 0xff}
	default:
		return color.RGBA{0x33, 0x33, 0x33, 0xff}
	}
}

// ---- browse screens (season → weekend → session) ----

// BrowseRow is one selectable line of a browse screen.
type BrowseRow struct {
	Left  string // date or other short tag
	Main  string
	Right string
}

// BrowseView is a full browse screen: a header, a row list, and up to three
// top-right buttons (index 0 = rightmost, matching HitHeaderButton).
type BrowseView struct {
	Title   string
	Sub     string
	Buttons []string
	Rows    []BrowseRow
	Empty   string // shown instead of the rows when there are none
	Note    string // status line at the bottom
	Loading bool   // waiting on the server: animate under Empty
	Tick    int    // animation frame
}

// RenderBrowse draws a browse screen. Row height shrinks with the row count
// so a full 24-weekend season still fits on one page.
func (r *Renderer) RenderBrowse(v BrowseView) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), image.NewUniform(white), image.Point{}, draw.Src)

	y := margin + 60
	// Shrink (then ellipsize) the title so it never runs into the buttons.
	avail := Width - 2*margin
	if n := len(v.Buttons); n > 0 {
		avail = browserBtnX(n-1) - margin - 40
	}
	r.text(img, r.fitFace(v.Title, avail), black, margin, y, r.fitText(v.Title, avail))
	y += px(56)
	r.text(img, r.row, gray, margin, y, v.Sub)
	for i, label := range v.Buttons {
		r.drawButton(img, browserBtnX(i), label)
	}
	y += px(46)
	fillRect(img, margin, y, Width-2*margin, 4, black)

	rowH := browseRowH(len(v.Rows))
	mainFace := r.h1
	if rowH < 90 {
		mainFace = r.row
	}
	for i, row := range v.Rows {
		if i >= BrowseMaxRows(len(v.Rows)) {
			break
		}
		ry := browseTop + i*rowH
		base := ry + rowH/2 + 16
		if i%2 == 1 {
			fillRect(img, margin-16, ry+3, Width-2*margin+32, rowH-6, stripe)
		}
		x := margin
		if row.Left != "" {
			r.text(img, r.monoSmall, gray, x, base-2, row.Left)
			x += 200
		}
		r.text(img, mainFace, black, x, base, row.Main)
		if row.Right != "" {
			r.textRight(img, r.row, gray, Width-margin, base, row.Right)
		}
	}
	if len(v.Rows) == 0 && v.Empty != "" {
		r.text(img, r.row, gray, margin, browseTop+60, v.Empty)
		if v.Loading {
			drawMarchingBar(img, margin, browseTop+100, 720, v.Tick)
		}
	}
	if v.Note != "" {
		r.text(img, r.small, gray, margin, Height-margin, v.Note)
	}
	return img
}

// ---- waiting-for-live screen ----

// WaitingView is the animated "no session yet" screen shown in live mode.
type WaitingView struct {
	Tick    int    // animation frame, incremented once per redraw
	Elapsed string // how long we have been waiting, "M:SS"
	Status  string // what the feed is doing right now
	Note    string // error / hint line at the bottom
	Buttons []string
	// DelayTotal > 0 means a live session is on air but its timing is held
	// in the TV-sync buffer: the marching bar becomes a fill bar showing
	// DelayBuffered of DelayTotal, with a countdown under it.
	DelayBuffered time.Duration
	DelayTotal    time.Duration
}

// Waiting-screen geometry. The marching bar is the only thing that moves:
// on e-ink, one narrow animated band means one small partial update per
// refresh instead of repainting half the screen.
var (
	waitCX     int
	waitTitleY int
	waitBarY   int
	waitBarW   int
)

const waitBarCol = 24 // cells in the marching bar

func applyWaitLayout() {
	waitCX = Width / 2
	waitTitleY = px(880)
	waitBarY = px(1024)
	waitBarW = px(1080)
}

// drawMarchingBar draws the loading animation: a short block of filled cells
// sweeping left to right, stepping once per tick. It is the only animated
// element on the waiting and loading screens — one narrow band keeps the
// e-ink partial update small.
func drawMarchingBar(img *image.RGBA, x0, y, w, tick int) {
	cellW := w / waitBarCol
	for i := 0; i < waitBarCol; i++ {
		col := color.Color(light)
		if ((i-tick)%waitBarCol+waitBarCol)%waitBarCol < 6 {
			col = black
		}
		fillRect(img, x0+i*cellW, y, cellW-6, 22, col)
	}
}

// drawFillBar draws a progress bar in the marching bar's band: the same
// cells, filled left to right in proportion to done/total.
func drawFillBar(img *image.RGBA, x0, y, w int, done, total time.Duration) {
	cellW := w / waitBarCol
	filled := 0
	if total > 0 {
		filled = int(float64(waitBarCol) * float64(done) / float64(total))
	}
	if filled > waitBarCol {
		filled = waitBarCol
	}
	for i := 0; i < waitBarCol; i++ {
		col := color.Color(light)
		if i < filled {
			col = black
		}
		fillRect(img, x0+i*cellW, y, cellW-6, 22, col)
	}
}

// fmtClock renders a duration as M:SS (seconds rounded down).
func fmtClock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d / time.Second)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// RenderWaiting draws the live-mode waiting screen: a headline, the feed
// status, and a single marching bar driven by Tick that shows the app is
// still polling for a session.
func (r *Renderer) RenderWaiting(v WaitingView) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), image.NewUniform(white), image.Point{}, draw.Src)

	y := margin + 60
	r.text(img, r.title, black, margin, y, "F1 TELEMETRY")
	y += px(56)
	r.text(img, r.row, gray, margin, y, "Live mode — following the F1 timing feed")
	for i, label := range v.Buttons {
		r.drawButton(img, browserBtnX(i), label)
	}
	y += px(46)
	fillRect(img, margin, y, Width-2*margin, 4, black)

	title := "WAITING FOR LIVE SESSION"
	if v.DelayTotal > 0 {
		title = "SESSION ON AIR — BUFFERING"
	}
	r.textCenter(img, r.title, black, waitCX, waitTitleY, title)
	status := v.Status
	if status == "" {
		status = "Checking the F1 live timing feed"
	}
	r.textCenter(img, r.row, gray, waitCX, waitTitleY+70, status)

	if v.DelayTotal > 0 {
		// Session found — the delay buffer is filling. A fill bar instead
		// of the marching bar, and a countdown instead of the wait clock.
		drawFillBar(img, waitCX-waitBarW/2, waitBarY, waitBarW, v.DelayBuffered, v.DelayTotal)
		left := v.DelayTotal - v.DelayBuffered
		if left < 0 {
			left = 0
		}
		r.textCenter(img, r.monoSmall, gray, waitCX, waitBarY+100,
			fmt.Sprintf("buffering %s / %s — timing in %s",
				fmtClock(v.DelayBuffered), fmtClock(v.DelayTotal), fmtClock(left)))
		r.textCenter(img, r.small, gray, waitCX, waitBarY+220,
			"The live delay (TV sync) holds the feed back by the set amount.")
		r.textCenter(img, r.small, gray, waitCX, waitBarY+270,
			"Lower it in Settings to see timing sooner.")
	} else {
		drawMarchingBar(img, waitCX-waitBarW/2, waitBarY, waitBarW, v.Tick)

		r.textCenter(img, r.monoSmall, gray, waitCX, waitBarY+100, "waiting "+v.Elapsed)
		r.textCenter(img, r.small, gray, waitCX, waitBarY+220,
			"Timing appears automatically the moment a session goes live.")
		r.textCenter(img, r.small, gray, waitCX, waitBarY+270,
			"To watch a past session, switch SESSION MODE to Replay in Settings.")
	}
	if v.Note != "" {
		r.text(img, r.small, gray, margin, Height-margin, v.Note)
	}
	return img
}

// drawButton draws an outlined button of btnW width at (x0, closeY0).
func (r *Renderer) drawButton(img *image.RGBA, x0 int, label string) {
	r.drawButtonW(img, x0, btnW, label)
}

// drawButtonW draws an outlined button of the given width, dropping to the
// smaller face when the label would not fit.
func (r *Renderer) drawButtonW(img *image.RGBA, x0, w int, label string) {
	x1, y0, y1 := x0+w, closeY0, closeY1
	fillRect(img, x0, y0, x1-x0, 4, black)
	fillRect(img, x0, y1-4, x1-x0, 4, black)
	fillRect(img, x0, y0, 4, y1-y0, black)
	fillRect(img, x1-4, y0, 4, y1-y0, black)
	face := r.h1
	if font.MeasureString(face, label).Ceil() > w-28 {
		face = r.row
	}
	r.textCenter(img, face, black, (x0+x1)/2, y0+52, label)
}

// drawBackButton draws the replay back arrow in an outlined box at x0.
func (r *Renderer) drawBackButton(img *image.RGBA, x0 int) {
	x1, y0, y1 := x0+backBtnW, closeY0, closeY1
	fillRect(img, x0, y0, x1-x0, 4, black)
	fillRect(img, x0, y1-4, x1-x0, 4, black)
	fillRect(img, x0, y0, 4, y1-y0, black)
	fillRect(img, x1-4, y0, 4, y1-y0, black)
	// Arrow: a shaft with a chevron head, pointing left.
	cy := float64(y0+y1) / 2
	ax0, ax1 := float64(x0+38), float64(x1-34)
	drawLine(img, ax0, cy, ax1, cy, 4, black)
	drawLine(img, ax0, cy, ax0+22, cy-22, 4, black)
	drawLine(img, ax0, cy, ax0+22, cy+22, 4, black)
}

// drawTimePopup overlays the replay clock control: the current virtual time
// and buttons jumping it by SeekMinutes.
func (r *Renderer) drawTimePopup(img *image.RGBA, st model.State) {
	x0, y0, x1, y1 := timePopRect()
	fillRect(img, x0-6, y0-6, x1-x0+12, y1-y0+12, black)
	fillRect(img, x0, y0, x1-x0, y1-y0, white)

	r.text(img, r.h1, black, x0+40, y0+70, "REPLAY TIME")
	r.textRight(img, r.small, gray, x1-40, y0+70, "tap outside to close")
	r.textCenter(img, r.title, black, (x0+x1)/2, y0+170, r.clock(st.LastUpdate).Format("15:04:05"))

	for i, m := range SeekMinutes {
		bx0, by0, bx1, by1 := seekBtnRect(i)
		fillRect(img, bx0, by0, bx1-bx0, 4, black)
		fillRect(img, bx0, by1-4, bx1-bx0, 4, black)
		fillRect(img, bx0, by0, 4, by1-by0, black)
		fillRect(img, bx1-4, by0, 4, by1-by0, black)
		label := fmt.Sprintf("+%d", m)
		if m < 0 {
			label = fmt.Sprintf("−%d", -m) // U+2212: a proper minus sign
		}
		r.textCenter(img, r.title, black, (bx0+bx1)/2, by0+72, label)
	}
	_, sy0, _, _ := seekBtnRect(0)
	r.textCenter(img, r.row, gray, (x0+x1)/2, sy0+seekBtnH+56,
		"minutes — the replay keeps playing from the new time")
}

// drawGear renders a settings gear icon centred at (cx, cy).
func (r *Renderer) drawGear(img *image.RGBA, cx, cy, rad int, col color.Color) {
	for k := 0; k < 8; k++ {
		a := float64(k) / 8 * 2 * math.Pi
		drawLine(img,
			float64(cx)+math.Cos(a)*float64(rad-2), float64(cy)+math.Sin(a)*float64(rad-2),
			float64(cx)+math.Cos(a)*float64(rad+6), float64(cy)+math.Sin(a)*float64(rad+6),
			5, col)
	}
	fillCircle(img, cx, cy, rad, col)
	fillCircle(img, cx, cy, rad-8, white)
	fillCircle(img, cx, cy, rad-14, col)
}

// drawChip draws one selectable option chip (filled when selected).
func (r *Renderer) drawChip(img *image.RGBA, kind Setting, row, i int, label string, sel bool) {
	x0, y0, x1, y1 := settingsChipRect(kind, row, i)
	face := r.row
	if font.MeasureString(face, label).Ceil() > x1-x0-28 {
		face = r.small
	}
	if sel {
		fillRect(img, x0, y0, x1-x0, y1-y0, black)
		r.textCenter(img, face, white, (x0+x1)/2, y0+(y1-y0)/2+14, label)
		return
	}
	fillRect(img, x0, y0, x1-x0, 3, black)
	fillRect(img, x0, y1-3, x1-x0, 3, black)
	fillRect(img, x0, y0, 3, y1-y0, black)
	fillRect(img, x1-3, y0, 3, y1-y0, black)
	r.textCenter(img, face, black, (x0+x1)/2, y0+(y1-y0)/2+14, label)
}

// drawSettingsSection draws a section label, its chip row, and a caption at
// the given on-screen row.
func (r *Renderer) drawSettingsSection(img *image.RGBA, kind Setting, row int, label string, chips []string, selected int, note string) {
	r.text(img, r.h1, black, margin, setSecTop+row*setSecGap, label)
	for i, c := range chips {
		r.drawChip(img, kind, row, i, c, i == selected)
	}
	if note != "" {
		_, _, _, y1 := settingsChipRect(kind, row, 0)
		// One line only — ellipsize rather than run off the panel edge.
		if lines := wrapWidth(r.small, note, Width-2*margin, Width-2*margin, 1); len(lines) > 0 {
			r.text(img, r.small, gray, margin, y1+setNoteDy, lines[0])
		}
	}
}

// RenderSettings draws the settings screen — the sections of v.Page, a CLOSE
// button (HitClose), and the PREV/NEXT pager (HitSettingsPager) when the
// sections span several pages. Controls are hit-tested via HitSettingsControl.
func (r *Renderer) RenderSettings(v SettingsView) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), image.NewUniform(white), image.Point{}, draw.Src)

	vis := visibleSettings(v.SessionMode)
	per := settingsPerPage(len(vis))
	pages := (len(vis) + per - 1) / per
	page := v.Page
	if page < 0 || page >= pages {
		page = 0
	}
	// sec draws one settings section at its row, skipping controls hidden in
	// this session mode or shown on another page.
	sec := func(kind Setting, label string, chips []string, selected int, note string) {
		idx := slices.Index(vis, kind)
		if idx < 0 || idx/per != page {
			return
		}
		r.drawSettingsSection(img, kind, idx-page*per, label, chips, selected, note)
	}

	y := margin + 60
	r.text(img, r.title, black, margin, y, "SETTINGS")
	y += px(56)
	r.text(img, r.row, gray, margin, y, "Tap to change · saved automatically")
	r.drawButton(img, closeX0, "CLOSE")
	if v.CanExit {
		r.drawButton(img, settingsExitX(), "EXIT")
	}
	if v.CanBack {
		r.drawBackButton(img, settingsBackX(v.CanExit))
	}
	y += px(46)
	fillRect(img, margin, y, Width-2*margin, 4, black)

	// Session mode: follow the live feed or replay a past session.
	sec(SettingSessionMode, "SESSION MODE", SessionModeLabels, v.SessionMode,
		"Live follows the F1 feed and waits for a session; Replay browses OpenF1 by season, weekend and session.")

	// What the header's second line shows.
	headerSel := 0
	if v.HeaderRC {
		headerSel = 1
	}
	sec(SettingHeaderLine, "HEADER LINE", HeaderLineLabels, headerSel,
		"The line under the title: the session's venue, or the newest race control message.")

	// Overall best sectors toggle.
	bestSel := 1
	if v.ShowBestSectors {
		bestSel = 0
	}
	sec(SettingBestSectors, "OVERALL BEST TIMES", []string{"ON", "OFF"}, bestSel,
		"Show the session's fastest S1/S2/S3 and lap above those columns (the purple fastest-lap dot always shows).")

	// Mini-sectors under the sector times: hidden, dot clusters, or one lap line.
	sec(SettingMiniSectors, "MINI SECTORS", MiniStyleLabels, int(v.MiniSectors),
		"Colored mini-sectors under the S1/S2/S3 times: a dot cluster per sector, or one continuous lap line.")

	// Clock timezone: device local, the circuit's, or UTC. The caption shows
	// what the selected option resolves to right now.
	sec(SettingTimeMode, "CLOCKS", TimeModeLabels, int(v.TimeMode),
		"Timezone for the header, race control and replay clocks — "+zoneNote(v.TimeMode, v.TrackOffset)+".")

	// Live delay (TV sync), shown in live mode only. A stepper rather than fixed choices:
	// -/+ buttons either side of the current value, which is highlighted to
	// read as a field rather than another button.
	delayChips := make([]string, len(DelaySteps))
	for i, step := range DelaySteps {
		switch {
		case i == DelayValueIndex:
			if v.DelaySeconds == 0 {
				delayChips[i] = "OFF"
			} else {
				delayChips[i] = fmt.Sprintf("%ds", v.DelaySeconds)
			}
		case step > 0:
			delayChips[i] = fmt.Sprintf("+%d", step)
		default:
			delayChips[i] = fmt.Sprintf("%d", step)
		}
	}
	delayNote := "Hold the live feed back to match your TV broadcast delay. Tap -/+ to adjust by 1, 5 or 10 seconds."
	if !v.LiveDelayShown {
		delayNote += " (applies when a live session is connected)"
	}
	sec(SettingDelay, "LIVE DELAY (TV SYNC)", delayChips, DelayValueIndex, delayNote)

	// Replay speed, shown in replay mode only.
	speedChips := make([]string, len(SpeedOptions))
	speedSel := -1
	for i, s := range SpeedOptions {
		speedChips[i] = fmt.Sprintf("%gx", s)
		if s == v.ReplaySpeed {
			speedSel = i
		}
	}
	sec(SettingSpeed, "REPLAY SPEED", speedChips, speedSel,
		"Playback speed for OpenF1 replays (1x = realtime).")

	// Map driver marker style.
	sec(SettingMarker, "MAP DRIVER MARKERS", MarkerLabels, int(v.Marker),
		"How cars are drawn on the map: team disc with number, 3-letter code, or a plain dot.")

	// Driver tracking source.
	sec(SettingTracking, "DRIVER TRACKING", TrackingLabels, int(v.Tracking),
		"Auto uses GPS when broadcast and falls back to sector timing; or force one, or turn markers off.")

	// Mini-sector grid on the map's track outline.
	mapMiniSel := 1
	if v.MapMiniSectors {
		mapMiniSel = 0
	}
	sec(SettingMapMini, "MAP MINI SECTORS", []string{"ON", "OFF"}, mapMiniSel,
		"Tick every mini-sector boundary on the map's track outline, between the S1/S2/S3 markers.")

	// T-cam marks.
	tcamSel := 1
	if v.ShowTcam {
		tcamSel = 0
	}
	sec(SettingTcam, "T-CAM MARKS", []string{"ON", "OFF"}, tcamSel,
		"Highlight the yellow-onboard-camera driver of each team (yellow border on the map, dot in timing).")

	// B&W markers.
	monoSel := 0
	if v.Mono {
		monoSel = 1
	}
	sec(SettingMono, "MAP MARKER COLOR", []string{"Color", "B&W"}, monoSel,
		"B&W markers drop team colors but refresh faster on the e-ink panel.")

	// Redraw interval.
	redrawChips := make([]string, len(RedrawOptions))
	redrawSel := -1
	for i, s := range RedrawOptions {
		redrawChips[i] = fmt.Sprintf("%ds", s)
		if s == v.RedrawSeconds {
			redrawSel = i
		}
	}
	sec(SettingRedraw, "REDRAW INTERVAL", redrawChips, redrawSel,
		"Minimum time between data updates. Longer is easier on the e-ink panel; taps always redraw at once.")

	// Full-screen B&W.
	fullSel := 0
	if v.FullBW {
		fullSel = 1
	}
	sec(SettingFullBW, "FULL B&W", []string{"Off", "On"}, fullSel,
		"Render the whole screen in grayscale — the fastest e-ink refresh of all.")

	if pages > 1 {
		r.drawPagerButton(img, margin, setPagerY0, setPagerY1, "< PREV", page > 0)
		r.drawPagerButton(img, Width-margin-btnW, setPagerY0, setPagerY1, "NEXT >", page < pages-1)
		r.textCenter(img, r.row, gray, Width/2, setPagerY0+(setPagerY1-setPagerY0)/2+14,
			fmt.Sprintf("PAGE %d/%d", page+1, pages))
	}

	return img
}

// drawPagerButton draws one button of a pager row; a grayed-out button marks
// the direction as unavailable from this page.
func (r *Renderer) drawPagerButton(img *image.RGBA, x0, y0, y1 int, label string, active bool) {
	col := color.Color(black)
	if !active {
		col = light
	}
	w, h := btnW, y1-y0
	fillRect(img, x0, y0, w, 3, col)
	fillRect(img, x0, y1-3, w, 3, col)
	fillRect(img, x0, y0, 3, h, col)
	fillRect(img, x0+w-3, y0, 3, h, col)
	r.textCenter(img, r.row, col, x0+w/2, y0+h/2+14, label)
}

// RenderMessage draws a full-screen status message (e.g. replay loading),
// with the marching bar under it while tick advances.
func (r *Renderer) RenderMessage(title, msg string, tick int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), image.NewUniform(white), image.Point{}, draw.Src)
	r.textCenter(img, r.title, black, Width/2, Height/2-40, title)
	r.textCenter(img, r.row, gray, Width/2, Height/2+40, msg)
	drawMarchingBar(img, Width/2-waitBarW/2, Height/2+110, waitBarW, tick)
	return img
}

// ToRGB565 packs img into dst using the little-endian RGB565 layout qtfb's
// FBFMT_RMPP_RGB565 expects. dst must be at least Width*Height*2 bytes.
func ToRGB565(img *image.RGBA, dst []byte) {
	pix := img.Pix
	n := Width * Height
	for i := 0; i < n; i++ {
		r, g, b := pix[i*4], pix[i*4+1], pix[i*4+2]
		v := uint16(r>>3)<<11 | uint16(g>>2)<<5 | uint16(b>>3)
		dst[i*2] = byte(v)
		dst[i*2+1] = byte(v >> 8)
	}
}
