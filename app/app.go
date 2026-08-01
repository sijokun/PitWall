// Package app is the shared controller behind both the on-device binary
// and the desktop simulator: it owns the data sources, the screen state
// machine and touch handling, and pushes rendered frames to a Display.
//
// The user picks one of two session modes in the settings screen:
//
//   - Live:   connect to the F1 push feed. Until a session is on air, an
//     animated waiting screen shows that the app is still polling.
//   - Replay: browse OpenF1 by season → race weekend → session, and play
//     the chosen one back on a virtual clock.
//
// Screens:
//   - live:     a session is running; frames follow the push feed.
//   - waiting:  live mode, nothing on air yet (animated).
//   - seasons / meetings / sessions: the replay browser's three levels.
//   - loading:  replay data is being fetched.
//   - replay:   a past session plays back on a virtual clock.
//
// Redraws are event-driven (feed updates, replay ticks, touches, loading
// animation). Data updates are debounced to at most one per MinRedraw to
// respect the e-ink panel; taps, state transitions and the loading animation
// bypass that wait so the screen answers immediately.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sijokun/PitWall/certs"
	"github.com/sijokun/PitWall/circuit"
	"github.com/sijokun/PitWall/dnsfix"
	"github.com/sijokun/PitWall/livetiming"
	"github.com/sijokun/PitWall/model"
	"github.com/sijokun/PitWall/openf1"
	"github.com/sijokun/PitWall/ui"
)

// Display abstracts the output device (qtfb framebuffer or a desktop
// window). Present is called from the app's render goroutine.
type Display interface {
	Present(frame []uint8, w, h int) // RGBA pixels
	DeepRefresh()                    // e-ink deep refresh; no-op elsewhere
}

type mode int

const (
	modeLive mode = iota
	modeWaiting
	modeSeasons
	modeMeetings
	modeSessions
	modeLoading
	modeReplay
	modeSettings
)

// Session modes (Settings.SessionMode), matching ui.SessionModeLabels.
const (
	sessionLive   = 0
	sessionReplay = 1
)

type Config struct {
	Source      string  // auto | live | openf1
	OpenF1Key   string  // OPENF1_API_KEY
	FileReplay  string  // fastf1 save file — devapp debugging
	ReplaySpeed float64 // virtual-clock factor for OpenF1 replays
	MinRedraw   time.Duration
	Year        int    // season preselected in the replay browser
	OnExit      func() // called when the user taps EXIT; nil disables the button action
}

type App struct {
	cfg  Config
	disp Display
	rend *ui.Renderer

	of1 *openf1.Client

	mu         sync.Mutex
	mode       mode
	prevMode   mode // mode to restore when the settings screen closes
	set        Settings
	tab        ui.Tab
	state      model.State
	live       *livetiming.Client
	liveCancel context.CancelFunc // stops the live client when leaving live mode
	anim       int                // loading/waiting animation frame
	waitSince  time.Time          // when the current wait started
	season     int                // season selected in the replay browser
	meetings   []model.MeetingOption
	meeting    model.MeetingOption
	sessions   []model.SessionOption
	listGen    int  // invalidates in-flight browser list fetches
	listBusy   bool // a browser list fetch is in flight
	replayer   *openf1.Replayer
	replayGen  int // invalidates a running replay loop on close
	trackMap   *model.TrackMap
	trackKey   int
	popup      int          // driver number of the open lap-history popup
	timePopup  bool         // the replay clock (seek) popup is open
	hidden     map[int]bool // car numbers hidden from the map
	loadNote   string

	dirty chan struct{}
	// urgent skips the redraw debounce for the next frame: a tap must show
	// its result at once, while feed updates stay rate-limited.
	urgent atomic.Bool
}

func New(cfg Config, disp Display) *App {
	certs.Install()
	dnsfix.Install()
	if cfg.ReplaySpeed <= 0 {
		cfg.ReplaySpeed = 60
	}
	if cfg.MinRedraw <= 0 {
		cfg.MinRedraw = time.Second
	}
	if cfg.Year == 0 {
		cfg.Year = time.Now().Year()
	}
	a := &App{
		cfg:    cfg,
		disp:   disp,
		rend:   ui.NewRenderer(),
		of1:    openf1.New("latest"),
		season: cfg.Year,
		hidden: map[int]bool{},
		dirty:  make(chan struct{}, 1),
	}
	a.of1.APIKey = cfg.OpenF1Key
	// -source openf1 starts in replay mode unless settings say otherwise.
	defMode := sessionLive
	if cfg.Source == "openf1" {
		defMode = sessionReplay
	}
	a.set = loadSettings(cfg.ReplaySpeed, defMode)
	a.mode = modeWaiting
	if a.set.SessionMode == sessionReplay {
		a.mode = modeSeasons
	}
	a.waitSince = time.Now()
	a.state = model.State{Source: "starting...", LastUpdate: time.Now()}
	return a
}

// Settings are the user-configurable options, persisted as JSON so they
// survive restarts (both on-device and in the simulator).
type Settings struct {
	SessionMode     int     `json:"sessionMode"`       // 0 = live feed, 1 = OpenF1 replay browser
	HeaderRC        bool    `json:"headerRaceControl"` // header line shows the newest race control message
	ReplaySpeed     float64 `json:"replaySpeed"`       // virtual-clock factor for OpenF1 replays
	ShowBestSectors bool    `json:"showBestSectors"`   // overall-best sector row on the timing tab
	DelaySeconds    int     `json:"delaySeconds"`      // live feed delay for TV sync (0 = realtime)
	MarkerStyle     int     `json:"markerStyle"`       // map driver marker style (ui.MarkerStyle)
	Tracking        int     `json:"tracking"`          // map driver tracking source (ui.TrackingMode)
	ShowTcam        bool    `json:"showTcam"`          // T-cam highlight (map border + timing dot)
	MonoMarkers     bool    `json:"monoMarkers"`       // grayscale map markers (faster e-ink)
	RedrawSeconds   int     `json:"redrawSeconds"`     // minimum interval between screen redraws
	FullBW          bool    `json:"fullBW"`            // grayscale the entire screen
}

// settingsPath is where the settings JSON persists.
func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "pitwall", "settings.json")
}

// loadSettings reads persisted settings, falling back to defaults (best
// sectors on, no delay, the given replay speed and session mode) for a
// missing/invalid file.
func loadSettings(defSpeed float64, defMode int) Settings {
	s := Settings{SessionMode: defMode, ReplaySpeed: defSpeed, ShowBestSectors: true, DelaySeconds: 0, ShowTcam: true, RedrawSeconds: 1}
	p := settingsPath()
	if p == "" {
		return s
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return s
	}
	json.Unmarshal(b, &s) // partial/invalid file keeps the defaults above
	if s.ReplaySpeed <= 0 {
		s.ReplaySpeed = defSpeed
	}
	if s.DelaySeconds < 0 {
		s.DelaySeconds = 0
	}
	if s.RedrawSeconds <= 0 {
		s.RedrawSeconds = 1
	}
	if s.SessionMode != sessionLive && s.SessionMode != sessionReplay {
		s.SessionMode = defMode
	}
	return s
}

// redrawInterval is the minimum time between screen redraws (from settings).
func (a *App) redrawInterval() time.Duration {
	a.mu.Lock()
	s := a.set.RedrawSeconds
	a.mu.Unlock()
	if s <= 0 {
		return a.cfg.MinRedraw
	}
	return time.Duration(s) * time.Second
}

// saveSettings persists the current settings. Caller need not hold a.mu.
func (a *App) saveSettings() {
	p := settingsPath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	a.mu.Lock()
	b, _ := json.MarshalIndent(a.set, "", "  ")
	a.mu.Unlock()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		log.Printf("save settings: %v", err)
	}
}

// markNow requests a frame that skips the redraw debounce — for taps and for
// the loading animation, which only repaints a narrow band.
func (a *App) markNow() {
	a.urgent.Store(true)
	a.markDirty()
}

func (a *App) markDirty() {
	select {
	case a.dirty <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is done.
func (a *App) Run(ctx context.Context) {
	if a.cfg.FileReplay != "" {
		// Debug path: a recorded feed file stands in for the live session.
		c := livetiming.New()
		a.mu.Lock()
		a.live = c
		a.mu.Unlock()
		a.setMode(modeLive)
		go func() {
			n, err := c.ReplayFileTimed(ctx, a.cfg.FileReplay, a.cfg.ReplaySpeed)
			log.Printf("file replay finished: %d updates, err=%v", n, err)
		}()
		go a.forwardLive(ctx, c)
	} else if a.set.SessionMode == sessionLive {
		a.startLive(ctx)
	}

	go a.animate(ctx)

	a.markDirty()
	a.renderLoop(ctx)
}

// startLive connects the live-timing client (idempotent) and forwards its
// events. The client runs on its own cancellable context so switching to
// replay mode can shut it down.
func (a *App) startLive(parent context.Context) {
	a.mu.Lock()
	if a.live != nil {
		a.mu.Unlock()
		return
	}
	c := livetiming.New()
	c.SetDelay(time.Duration(a.set.DelaySeconds) * time.Second)
	ctx, cancel := context.WithCancel(parent)
	a.live, a.liveCancel = c, cancel
	a.mu.Unlock()

	go c.Run(ctx)
	go a.forwardLive(ctx, c)
}

// stopLive disconnects the live-timing client, if any.
func (a *App) stopLive() {
	a.mu.Lock()
	cancel := a.liveCancel
	a.live, a.liveCancel = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// forwardLive turns feed events into redraws and lifts the waiting screen
// once the feed carries timing. It only flips the screen and marks dirty —
// the (heavier) Snapshot happens once per frame in render(), so a flood of
// feed events never triggers a Snapshot per event.
func (a *App) forwardLive(ctx context.Context, c *livetiming.Client) {
	for {
		select {
		case <-c.Updates():
		case <-ctx.Done():
			return
		}
		if !c.HasTiming() {
			continue
		}
		a.mu.Lock()
		// A live session takes over from the waiting screen, but never
		// interrupts the settings screen or a user-started replay.
		fresh := a.mode == modeWaiting
		if fresh {
			a.mode = modeLive
		} else if a.mode == modeSettings && a.prevMode == modeWaiting {
			a.prevMode = modeLive
		}
		a.mu.Unlock()
		if fresh {
			a.markNow() // first timing of the session: leave the waiting screen now
			continue
		}
		a.markDirty()
	}
}

// animate advances the marching-bar animation for as long as a screen is
// waiting on something: the live feed, a browser list, or a replay download.
func (a *App) animate(ctx context.Context) {
	for {
		// The bar steps about once a second even when the redraw interval is
		// longer: that setting limits full-screen data churn, while this is a
		// small band on a screen with nothing else moving.
		step := min(a.redrawInterval(), time.Second)
		select {
		case <-time.After(step):
		case <-ctx.Done():
			return
		}
		a.mu.Lock()
		animating := a.mode == modeWaiting || a.mode == modeLoading ||
			((a.mode == modeMeetings || a.mode == modeSessions) && a.listBusy)
		if animating {
			a.anim++
		}
		a.mu.Unlock()
		if animating {
			a.markNow()
		}
	}
}

func (a *App) setMode(m mode) {
	a.mu.Lock()
	a.mode = m
	a.mu.Unlock()
	a.markNow()
}

// openSettings shows the settings screen, remembering the screen to return
// to when it closes.
func (a *App) openSettings() {
	a.mu.Lock()
	if a.mode != modeSettings {
		a.prevMode = a.mode
	}
	a.mode = modeSettings
	a.mu.Unlock()
	a.markNow()
}

// closeSettings returns to the screen the settings were opened from.
func (a *App) closeSettings() {
	a.mu.Lock()
	a.mode = a.prevMode
	a.mu.Unlock()
	a.markNow()
}

// ---- session mode ----

// applySessionMode brings the data sources and the visible screen in line
// with the selected session mode. Called when the user flips the SESSION
// MODE chips; safe to call from the settings screen, where it retargets the
// screen the settings will close onto.
func (a *App) applySessionMode(ctx context.Context) {
	if a.cfg.FileReplay != "" {
		return // debug file replay owns the screen
	}
	a.mu.Lock()
	replay := a.set.SessionMode == sessionReplay
	a.mu.Unlock()

	if replay {
		a.stopLive()
		a.goTo(modeSeasons)
		a.loadMeetings(ctx)
		return
	}
	a.startLive(ctx)
	a.goTo(modeWaiting)
}

// closeReplay drops the running replay and returns to the weekend's session
// list — the back arrow on the settings screen.
func (a *App) closeReplay() {
	a.mu.Lock()
	a.replayGen++
	a.replayer = nil
	a.mode = modeSessions
	a.popup = 0
	a.timePopup = false
	a.loadNote = ""
	a.mu.Unlock()
	a.markNow()
}

// goTo switches to screen m, dropping any running replay. From the settings
// screen it retargets what CLOSE returns to instead.
func (a *App) goTo(m mode) {
	a.mu.Lock()
	a.replayGen++
	a.replayer = nil
	a.popup = 0
	a.timePopup = false
	a.loadNote = ""
	if m == modeWaiting {
		a.waitSince = time.Now()
		a.anim = 0
		if a.live != nil && a.live.HasTiming() {
			m = modeLive
		}
	}
	if a.mode == modeSettings {
		a.prevMode = m
	} else {
		a.mode = m
	}
	a.mu.Unlock()
	a.markNow()
}

// ---- replay browser ----

// loadMeetings fetches the selected season's race weekends.
func (a *App) loadMeetings(ctx context.Context) {
	a.mu.Lock()
	a.listGen++
	gen := a.listGen
	year := a.season
	a.meetings = nil
	a.listBusy = true
	a.loadNote = ""
	a.mu.Unlock()
	a.markNow()

	go func() {
		ms, err := a.of1.ListMeetings(ctx, year)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.listGen != gen {
			return // superseded by a newer navigation
		}
		a.listBusy = false
		if err != nil {
			a.loadNote = "OpenF1: " + err.Error()
		} else {
			a.meetings = ms
			a.loadNote = ""
		}
		a.markNow() // the list is in: show it without waiting out the interval
	}()
}

// loadSessions fetches one weekend's sessions.
func (a *App) loadSessions(ctx context.Context, m model.MeetingOption) {
	a.mu.Lock()
	a.listGen++
	gen := a.listGen
	a.meeting = m
	a.sessions = nil
	a.listBusy = true
	a.loadNote = ""
	a.mu.Unlock()
	a.markNow()

	go func() {
		ss, err := a.of1.ListMeetingSessions(ctx, m.MeetingKey)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.listGen != gen {
			return
		}
		a.listBusy = false
		if err != nil {
			a.loadNote = "OpenF1: " + err.Error()
		} else {
			a.sessions = ss
			a.loadNote = ""
		}
		a.markNow()
	}()
}

// renderLoop turns dirty signals into Present calls, rate-limited to one per
// redraw interval — except for urgent ones (taps, loading animation, state
// transitions), which are drawn at once. A signal arriving mid-wait is
// re-examined rather than ignored, so an urgent request never sits out the
// remainder of somebody else's interval.
func (a *App) renderLoop(ctx context.Context) {
	last := time.Time{}
	for {
		select {
		case <-a.dirty:
		case <-ctx.Done():
			return
		}
		for {
			wait := a.redrawInterval() - time.Since(last)
			if a.urgent.Swap(false) || wait <= 0 {
				break
			}
			select {
			case <-a.dirty: // may be urgent — loop round and re-check
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
		a.render(ctx)
		last = time.Now()
	}
}

func (a *App) render(ctx context.Context) {
	a.mu.Lock()
	var frame []uint8
	switch a.mode {
	case modeWaiting:
		note := a.loadNote
		status := "Connecting to the F1 live timing feed"
		if a.live != nil {
			if e := a.live.LastError(); e != "" {
				if note != "" {
					note += " · "
				}
				note += "live: " + e
			} else {
				status = "Connected — no session on air yet"
			}
		}
		frame = a.rend.RenderWaiting(ui.WaitingView{
			Tick:    a.anim,
			Elapsed: formatElapsed(time.Since(a.waitSince)),
			Status:  status,
			Note:    note,
			Buttons: a.headerButtons(modeWaiting),
		}).Pix
	case modeSeasons, modeMeetings, modeSessions:
		frame = a.rend.RenderBrowse(a.browseView()).Pix
	case modeLoading:
		frame = a.rend.RenderMessage("LOADING REPLAY", a.loadNote, a.anim).Pix
	case modeSettings:
		frame = a.rend.RenderSettings(ui.SettingsView{
			SessionMode:     a.set.SessionMode,
			HeaderRC:        a.set.HeaderRC,
			ShowBestSectors: a.set.ShowBestSectors,
			DelaySeconds:    a.set.DelaySeconds,
			ReplaySpeed:     a.set.ReplaySpeed,
			Marker:          ui.MarkerStyle(a.set.MarkerStyle),
			Tracking:        ui.TrackingMode(a.set.Tracking),
			ShowTcam:        a.set.ShowTcam,
			Mono:            a.set.MonoMarkers,
			RedrawSeconds:   a.set.RedrawSeconds,
			FullBW:          a.set.FullBW,
			LiveDelayShown:  a.live != nil && a.cfg.FileReplay == "",
			CanExit:         a.cfg.OnExit != nil,
			CanBack:         a.prevMode == modeReplay || a.prevMode == modeLoading,
		}).Pix
	default: // modeLive, modeReplay
		// Snapshot the live feed here (once per frame) rather than on every
		// feed event. Store it in a.state so touch handlers see fresh data too.
		if a.mode == modeLive && a.live != nil {
			a.state = a.live.Snapshot()
			if a.cfg.FileReplay != "" {
				a.state.Source = "Replay (file)"
			}
		}
		st := a.state
		if !a.set.ShowBestSectors {
			st.BestSectors = [3]string{}
		}
		a.maybeFetchTrackMap(ctx, st)
		frame = a.rend.Render(st, a.tab, a.trackMap, a.popup, ui.ViewOptions{
			Hidden:    a.hidden,
			Marker:    ui.MarkerStyle(a.set.MarkerStyle),
			Tracking:  ui.TrackingMode(a.set.Tracking),
			ShowTcam:  a.set.ShowTcam,
			Mono:      a.set.MonoMarkers,
			HeaderRC:  a.set.HeaderRC,
			TimePopup: a.timePopup,
		}).Pix
	}
	fullBW := a.set.FullBW
	a.mu.Unlock()
	if fullBW {
		ui.Grayscale(frame)
	}
	a.disp.Present(frame, ui.Width, ui.Height)
}

// formatElapsed renders a wait duration coarsely — minutes, not seconds, so
// the line changes once a minute instead of on every refresh. The marching
// bar is the waiting screen's only per-frame animation.
func formatElapsed(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	default:
		return fmt.Sprintf("%dh %02dmin", m/60, m%60)
	}
}

// headerButtons are the top-right buttons of screen m, listed right to left
// (index 0 = rightmost, matching ui.HitHeaderButton). Caller holds a.mu.
func (a *App) headerButtons(m mode) []string {
	var btns []string
	switch m {
	case modeWaiting:
		btns = []string{"RETRY"}
	case modeMeetings, modeSessions:
		btns = []string{"BACK"}
	}
	btns = append(btns, "SETTINGS")
	if a.cfg.OnExit != nil {
		btns = append(btns, "EXIT")
	}
	return btns
}

// browseView builds the current level of the replay browser. Caller holds a.mu.
func (a *App) browseView() ui.BrowseView {
	v := ui.BrowseView{
		Note:    a.loadNote,
		Buttons: a.headerButtons(a.mode),
		Loading: a.listBusy && (a.mode == modeMeetings || a.mode == modeSessions),
		Tick:    a.anim,
	}
	switch a.mode {
	case modeSeasons:
		v.Title = "SELECT SEASON"
		v.Sub = "Replay mode — pick a season, then a weekend and a session"
		for _, y := range openf1.Seasons(time.Now()) {
			row := ui.BrowseRow{Main: fmt.Sprintf("%d SEASON", y)}
			if y == a.season {
				row.Right = "last viewed"
			}
			v.Rows = append(v.Rows, row)
		}
	case modeMeetings:
		v.Title = fmt.Sprintf("%d SEASON", a.season)
		v.Sub = "Select a race weekend"
		v.Empty = "No weekends found for this season."
		if a.listBusy {
			v.Empty = "Loading weekends from OpenF1..."
		}
		for _, m := range a.meetings {
			name := m.Name
			if name == "" {
				name = m.Country
			}
			v.Rows = append(v.Rows, ui.BrowseRow{
				Left:  m.Date.Local().Format("Jan 02"),
				Main:  name,
				Right: m.Location,
			})
		}
	case modeSessions:
		name := a.meeting.Name
		if name == "" {
			name = a.meeting.Country
		}
		v.Title = strings.ToUpper(name)
		v.Sub = fmt.Sprintf("%s · %d — select a session to replay", a.meeting.Location, a.season)
		v.Empty = "No finished sessions in this weekend yet."
		if a.listBusy {
			v.Empty = "Loading sessions from OpenF1..."
		}
		for _, s := range a.sessions {
			// The weekend is already in the title, so the rows only carry
			// the session's day/time and name.
			v.Rows = append(v.Rows, ui.BrowseRow{
				Left: s.Date.Local().Format("Mon 15:04"),
				Main: s.SessionName,
			})
		}
	}
	if len(v.Rows) == 0 && v.Note == "" && v.Empty == "" {
		v.Empty = "Loading..."
	}
	return v
}

// maybeFetchTrackMap kicks off a circuit outline fetch. Caller holds a.mu.
func (a *App) maybeFetchTrackMap(ctx context.Context, st model.State) {
	key := st.CircuitKey
	year := a.cfg.Year
	if st.Session != nil && st.Session.Year != 0 {
		year = st.Session.Year
	}
	if key == 0 || a.trackKey == key {
		return
	}
	a.trackKey = key
	a.trackMap = nil
	go func() {
		tm, err := circuit.Fetch(ctx, key, year)
		if err != nil {
			log.Printf("circuit map: %v", err)
			return
		}
		a.mu.Lock()
		a.trackMap = tm
		a.mu.Unlock()
		a.markDirty()
	}()
}

// Touch handles a released touch/click in device coordinates. Anything it
// changes is drawn on the next frame without waiting out the redraw
// interval, so the UI answers the tap immediately.
func (a *App) Touch(ctx context.Context, x, y int) {
	a.urgent.Store(true)
	a.mu.Lock()
	m := a.mode
	a.mu.Unlock()

	switch m {
	case modeWaiting:
		switch a.hitHeaderButton(x, y, m) {
		case "RETRY":
			// Drop the connection and reconnect, in case the feed went stale.
			a.stopLive()
			a.startLive(ctx)
			a.mu.Lock()
			a.waitSince = time.Now()
			a.loadNote = ""
			a.mu.Unlock()
			a.markDirty()
		case "SETTINGS":
			a.openSettings()
		case "EXIT":
			a.cfg.OnExit()
		default:
			a.disp.DeepRefresh()
		}
	case modeSeasons, modeMeetings, modeSessions:
		a.touchBrowse(ctx, x, y, m)
	case modeSettings:
		if ui.HitClose(x, y) {
			a.closeSettings()
			return
		}
		if a.cfg.OnExit != nil && ui.HitSettingsExit(x, y) {
			a.cfg.OnExit()
			return
		}
		a.mu.Lock()
		inReplay := a.prevMode == modeReplay || a.prevMode == modeLoading
		a.mu.Unlock()
		if inReplay && ui.HitSettingsBack(x, y, a.cfg.OnExit != nil) {
			a.closeReplay()
			return
		}
		kind, idx := ui.HitSettingsControl(x, y)
		switch kind {
		case ui.SettingSessionMode:
			if idx >= 0 && idx < len(ui.SessionModeLabels) {
				a.mu.Lock()
				changed := a.set.SessionMode != idx
				a.set.SessionMode = idx
				a.mu.Unlock()
				a.saveSettings()
				if changed {
					a.applySessionMode(ctx)
				}
				a.markDirty()
			}
		case ui.SettingHeaderLine:
			a.mu.Lock()
			a.set.HeaderRC = idx == 1 // chip 1 = last race control message
			a.mu.Unlock()
			a.saveSettings()
			a.markDirty()
		case ui.SettingBestSectors:
			a.mu.Lock()
			a.set.ShowBestSectors = idx == 0 // chip 0 = ON, 1 = OFF
			a.mu.Unlock()
			a.saveSettings()
			a.markDirty()
		case ui.SettingDelay:
			if idx >= 0 && idx < len(ui.DelayOptions) {
				sec := ui.DelayOptions[idx]
				a.mu.Lock()
				a.set.DelaySeconds = sec
				live := a.live
				a.mu.Unlock()
				if live != nil {
					live.SetDelay(time.Duration(sec) * time.Second)
				}
				a.saveSettings()
				a.markDirty()
			}
		case ui.SettingSpeed:
			if idx >= 0 && idx < len(ui.SpeedOptions) {
				a.mu.Lock()
				a.set.ReplaySpeed = ui.SpeedOptions[idx]
				a.mu.Unlock()
				a.saveSettings()
				a.markDirty()
			}
		case ui.SettingMarker:
			if idx >= 0 && idx < len(ui.MarkerLabels) {
				a.mu.Lock()
				a.set.MarkerStyle = idx
				a.mu.Unlock()
				a.saveSettings()
				a.markDirty()
			}
		case ui.SettingTracking:
			if idx >= 0 && idx < len(ui.TrackingLabels) {
				a.mu.Lock()
				a.set.Tracking = idx
				a.mu.Unlock()
				a.saveSettings()
				a.markDirty()
			}
		case ui.SettingTcam:
			a.mu.Lock()
			a.set.ShowTcam = idx == 0 // chip 0 = ON
			a.mu.Unlock()
			a.saveSettings()
			a.markDirty()
		case ui.SettingMono:
			a.mu.Lock()
			a.set.MonoMarkers = idx == 1 // chip 1 = B&W
			a.mu.Unlock()
			a.saveSettings()
			a.markDirty()
		case ui.SettingRedraw:
			if idx >= 0 && idx < len(ui.RedrawOptions) {
				a.mu.Lock()
				a.set.RedrawSeconds = ui.RedrawOptions[idx]
				a.mu.Unlock()
				a.saveSettings()
				a.markDirty()
			}
		case ui.SettingFullBW:
			a.mu.Lock()
			a.set.FullBW = idx == 1 // chip 1 = On
			a.mu.Unlock()
			a.saveSettings()
			a.markDirty()
		}
	case modeReplay, modeLive:
		// The replay clock popup is modal: it swallows every tap until a
		// seek button is used or the tap lands outside the panel.
		if m == modeReplay {
			a.mu.Lock()
			open := a.timePopup
			a.mu.Unlock()
			if open {
				if i := ui.HitSeekButton(x, y); i >= 0 {
					a.mu.Lock()
					if a.replayer != nil {
						a.state = a.replayer.Seek(time.Duration(ui.SeekMinutes[i]) * time.Minute)
					}
					a.mu.Unlock()
					a.markDirty()
					return
				}
				if !ui.InTimePopup(x, y) {
					a.mu.Lock()
					a.timePopup = false
					a.mu.Unlock()
					a.markDirty()
				}
				return
			}
			if ui.HitReplayClock(x, y) {
				a.mu.Lock()
				a.timePopup = true
				a.popup = 0
				a.mu.Unlock()
				a.markDirty()
				return
			}
		}
		// The gear keeps the same top-right spot in both modes.
		if ui.HitSettingsGear(x, y) {
			a.openSettings()
			return
		}
		a.mu.Lock()
		if a.popup != 0 {
			a.popup = 0
			a.mu.Unlock()
			a.markDirty()
			return
		}
		a.mu.Unlock()
		if t := ui.HitTab(x, y); t >= 0 {
			a.mu.Lock()
			if t != a.tab {
				a.tab = t
				a.popup = 0
			}
			a.mu.Unlock()
			a.markDirty()
			return
		}
		a.mu.Lock()
		if a.tab == ui.TabTiming {
			if i := ui.HitTimingRow(x, y); i >= 0 && i < len(a.state.Standings) {
				a.popup = a.state.Standings[i].Number
				a.mu.Unlock()
				a.markDirty()
				return
			}
		}
		if a.tab == ui.TabMap {
			nums := ui.MapDriverNumbers(a.state)
			switch kind, i := ui.HitMapFilter(x, y, len(nums)); kind {
			case ui.MapFilterAll:
				a.hidden = map[int]bool{}
				a.mu.Unlock()
				a.markDirty()
				return
			case ui.MapFilterClear:
				a.hidden = map[int]bool{}
				for _, n := range nums {
					a.hidden[n] = true
				}
				a.mu.Unlock()
				a.markDirty()
				return
			case ui.MapFilterDriver:
				if i >= 0 && i < len(nums) {
					a.hidden[nums[i]] = !a.hidden[nums[i]]
					a.mu.Unlock()
					a.markDirty()
					return
				}
			}
		}
		a.mu.Unlock()
		a.disp.DeepRefresh()
	}
}

// hitHeaderButton returns the label of the header button at (x, y) on screen
// m, or "" for a miss.
func (a *App) hitHeaderButton(x, y int, m mode) string {
	i := ui.HitHeaderButton(x, y)
	if i < 0 {
		return ""
	}
	a.mu.Lock()
	btns := a.headerButtons(m)
	a.mu.Unlock()
	if i >= len(btns) {
		return ""
	}
	return btns[i]
}

// touchBrowse handles taps on the season / weekend / session screens.
func (a *App) touchBrowse(ctx context.Context, x, y int, m mode) {
	switch a.hitHeaderButton(x, y, m) {
	case "BACK":
		if m == modeSessions {
			a.setMode(modeMeetings)
		} else {
			a.setMode(modeSeasons)
		}
		return
	case "SETTINGS":
		a.openSettings()
		return
	case "EXIT":
		a.cfg.OnExit()
		return
	}

	a.mu.Lock()
	var n int
	switch m {
	case modeSeasons:
		n = len(openf1.Seasons(time.Now()))
	case modeMeetings:
		n = len(a.meetings)
	case modeSessions:
		n = len(a.sessions)
	}
	i := ui.HitBrowseRow(x, y, n)
	if i < 0 || i >= ui.BrowseMaxRows(n) {
		a.mu.Unlock()
		a.disp.DeepRefresh()
		return
	}
	switch m {
	case modeSeasons:
		a.season = openf1.Seasons(time.Now())[i]
		// Mark busy and drop the old list here, under the same lock as the
		// mode switch: a redraw in between would otherwise flash the previous
		// season's weekends, or "none found" before the fetch even starts.
		a.meetings, a.listBusy = nil, true
		a.mode = modeMeetings
		a.mu.Unlock()
		a.markDirty()
		a.loadMeetings(ctx)
	case modeMeetings:
		mt := a.meetings[i]
		a.sessions, a.listBusy = nil, true
		a.mode = modeSessions
		a.mu.Unlock()
		a.markDirty()
		a.loadSessions(ctx, mt)
	case modeSessions:
		ses := a.sessions[i]
		a.mu.Unlock()
		a.startReplay(ctx, ses)
	}
}

func (a *App) startReplay(ctx context.Context, ses model.SessionOption) {
	a.mu.Lock()
	a.mode = modeLoading
	a.loadNote = ses.MeetingName + " — " + ses.SessionName
	gen := a.replayGen
	a.mu.Unlock()
	a.markDirty()

	go func() {
		rep, err := a.of1.LoadReplay(ctx, ses.SessionKey)
		a.mu.Lock()
		if a.replayGen != gen || a.mode != modeLoading {
			a.mu.Unlock()
			return
		}
		if err != nil {
			a.mode = modeSessions
			a.loadNote = "replay: " + err.Error()
			a.mu.Unlock()
			a.markNow()
			return
		}
		a.replayer = rep
		a.mode = modeReplay
		a.state = rep.Advance(0)
		a.mu.Unlock()
		a.markNow() // replay is ready: swap off the loading screen at once

		// Virtual clock: tick once per second of wall time.
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			}
			a.mu.Lock()
			// Only a closed (or superseded) replay ends the loop — while the
			// settings screen is open the clock simply pauses.
			if a.replayGen != gen || a.replayer == nil {
				a.mu.Unlock()
				return
			}
			if a.mode == modeReplay && !a.replayer.Done() {
				a.state = a.replayer.Advance(time.Duration(a.set.ReplaySpeed * float64(time.Second)))
				a.markDirty()
			}
			// Once done, keep ticking idly: the user can still rewind
			// with the -1 MIN button, which un-does Done().
			a.mu.Unlock()
		}
	}()
}
