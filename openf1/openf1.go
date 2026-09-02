// Package openf1 talks to the OpenF1 API (https://openf1.org). Its free
// tier is historical/delayed data, which serves two purposes here: listing
// past sessions when nothing is live, and replaying a chosen session by
// walking its records on a virtual clock. The truly-live source is the
// livetiming package.
package openf1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sijokun/PitWall/model"
)

const baseURL = "https://api.openf1.org/v1"

type Session struct {
	SessionKey       int     `json:"session_key"`
	MeetingKey       int     `json:"meeting_key"`
	SessionName      string  `json:"session_name"`
	SessionType      string  `json:"session_type"`
	CircuitShortName string  `json:"circuit_short_name"`
	CircuitKey       int     `json:"circuit_key"`
	Location         string  `json:"location"`
	CountryName      string  `json:"country_name"`
	Year             int     `json:"year"`
	GmtOffset        string  `json:"gmt_offset"`
	DateStart        apiTime `json:"date_start"`
	DateEnd          apiTime `json:"date_end"`
}

type meetingRec struct {
	MeetingKey  int     `json:"meeting_key"`
	MeetingName string  `json:"meeting_name"`
	Location    string  `json:"location"`
	CountryName string  `json:"country_name"`
	CircuitKey  int     `json:"circuit_key"`
	Year        int     `json:"year"`
	DateStart   apiTime `json:"date_start"`
}

type driverRec struct {
	DriverNumber int    `json:"driver_number"`
	NameAcronym  string `json:"name_acronym"`
	FullName     string `json:"full_name"`
	TeamName     string `json:"team_name"`
	TeamColour   string `json:"team_colour"`
}

type positionRec struct {
	Date         apiTime `json:"date"`
	DriverNumber int     `json:"driver_number"`
	Position     int     `json:"position"`
}

type intervalRec struct {
	Date         apiTime         `json:"date"`
	DriverNumber int             `json:"driver_number"`
	GapToLeader  json.RawMessage `json:"gap_to_leader"`
	Interval     json.RawMessage `json:"interval"`
}

type lapRec struct {
	DateStart    apiTime  `json:"date_start"`
	DriverNumber int      `json:"driver_number"`
	LapNumber    int      `json:"lap_number"`
	LapDuration  *float64 `json:"lap_duration"`
	Sector1      *float64 `json:"duration_sector_1"`
	Sector2      *float64 `json:"duration_sector_2"`
	Sector3      *float64 `json:"duration_sector_3"`
	Seg1         []int    `json:"segments_sector_1"`
	Seg2         []int    `json:"segments_sector_2"`
	Seg3         []int    `json:"segments_sector_3"`
}

// segments returns the lap's mini-segment statuses per sector (same codes as
// the live feed: 2048 completed, 2049 PB, 2051 overall best, 2064 pit lane).
func (l lapRec) segments() [3][]int { return [3][]int{l.Seg1, l.Seg2, l.Seg3} }

type stintRec struct {
	DriverNumber   int    `json:"driver_number"`
	StintNumber    int    `json:"stint_number"`
	Compound       string `json:"compound"`
	TyreAgeAtStart int    `json:"tyre_age_at_start"`
	LapStart       int    `json:"lap_start"`
	LapEnd         int    `json:"lap_end"`
}

type pitRec struct {
	Date         apiTime `json:"date"`
	DriverNumber int     `json:"driver_number"`
}

type rcRec struct {
	Date    apiTime `json:"date"`
	Flag    string  `json:"flag"`
	Scope   string  `json:"scope"`
	Sector  int     `json:"sector"`
	Message string  `json:"message"`
}

// apiTime tolerates the timestamp variants OpenF1 emits.
type apiTime struct{ time.Time }

func (t *apiTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04:05.999999", "2006-01-02T15:04:05",
	} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v.UTC()
			return nil
		}
	}
	return fmt.Errorf("openf1: unparseable time %q", s)
}

// ---- accumulator: merged per-session state, shared by poller and replayer ----

type acc struct {
	drivers   map[int]driverRec
	pos       map[int]positionRec
	gaps      map[int]intervalRec
	lastLap   map[int]lapRec
	bestLap   map[int]float64
	stints    map[int][]stintRec // all stints, selected by lap in snapshot
	pitCount  map[int]int
	rc        []rcRec
	bestSec   [3]map[int]float64  // per-driver best sector times
	minSec    [3]float64          // session-best sector times
	minLap    float64             // session-best lap
	curSec    map[int][3]*float64 // in-progress lap sectors (replay staging)
	curSeg    map[int][3][]int    // in-progress lap mini-segments (replay staging)
	segCounts [3]int              // mini-segments per sector (max seen)
	curLap    map[int]int         // lap number the staged sectors belong to
	history   map[int][]model.LapRecord
}

func newAcc() acc {
	a := acc{
		drivers:  map[int]driverRec{},
		pos:      map[int]positionRec{},
		gaps:     map[int]intervalRec{},
		lastLap:  map[int]lapRec{},
		bestLap:  map[int]float64{},
		stints:   map[int][]stintRec{},
		pitCount: map[int]int{},
	}
	for i := range a.bestSec {
		a.bestSec[i] = map[int]float64{}
	}
	a.curSec = map[int][3]*float64{}
	a.curSeg = map[int][3][]int{}
	a.curLap = map[int]int{}
	a.history = map[int][]model.LapRecord{}
	return a
}

func (a *acc) applyPosition(p positionRec) {
	if p.Date.After(a.pos[p.DriverNumber].Date.Time) || a.pos[p.DriverNumber].Position == 0 {
		a.pos[p.DriverNumber] = p
	}
}

func (a *acc) applyInterval(g intervalRec) {
	if g.Date.After(a.gaps[g.DriverNumber].Date.Time) || a.gaps[g.DriverNumber].DriverNumber == 0 {
		a.gaps[g.DriverNumber] = g
	}
}

func (a *acc) applyLap(l lapRec) {
	if l.LapNumber >= a.lastLap[l.DriverNumber].LapNumber {
		a.lastLap[l.DriverNumber] = l
	}
	for i, segs := range l.segments() {
		if len(segs) > a.segCounts[i] {
			a.segCounts[i] = len(segs)
		}
	}
	if l.LapDuration != nil && (*l.LapDuration < a.bestLap[l.DriverNumber] || a.bestLap[l.DriverNumber] == 0) {
		a.bestLap[l.DriverNumber] = *l.LapDuration
	}
	if l.LapDuration != nil && (*l.LapDuration < a.minLap || a.minLap == 0) {
		a.minLap = *l.LapDuration
	}
	for i, sec := range []*float64{l.Sector1, l.Sector2, l.Sector3} {
		if sec == nil {
			continue
		}
		if *sec < a.bestSec[i][l.DriverNumber] || a.bestSec[i][l.DriverNumber] == 0 {
			a.bestSec[i][l.DriverNumber] = *sec
		}
		if *sec < a.minSec[i] || a.minSec[i] == 0 {
			a.minSec[i] = *sec
		}
	}

	if l.LapDuration != nil {
		num := l.DriverNumber
		rec := model.LapRecord{
			Lap:          l.LapNumber,
			Time:         model.FormatLapSeconds(*l.LapDuration),
			PersonalBest: *l.LapDuration == a.bestLap[num],
			OverallBest:  *l.LapDuration == a.minLap,
		}
		for i, sec := range []*float64{l.Sector1, l.Sector2, l.Sector3} {
			if sec != nil {
				rec.Sectors[i] = model.Sector{
					Value:        fmt.Sprintf("%.3f", *sec),
					PersonalBest: *sec == a.bestSec[i][num],
					OverallBest:  *sec == a.minSec[i],
					Segments:     l.segments()[i],
				}
			}
		}
		h := a.history[num]
		if n := len(h); n > 0 && h[n-1].Lap == rec.Lap {
			h[n-1] = rec
		} else {
			h = append(h, rec)
			if len(h) > 40 {
				h = h[len(h)-40:]
			}
		}
		a.history[num] = h
	}
}

// stageLapStart resets the staged lap at its start (replay only): sector
// times cleared and every known mini-segment shown as a not-yet-run
// placeholder, like the live feed's per-lap segment reset.
func (a *acc) stageLapStart(num, lapNumber int, counts [3]int) {
	if lapNumber <= a.curLap[num] {
		return
	}
	a.curLap[num] = lapNumber
	a.curSec[num] = [3]*float64{}
	var cg [3][]int
	for i, n := range counts {
		if n > 0 {
			cg[i] = make([]int, n)
		}
		if n > a.segCounts[i] {
			a.segCounts[i] = n
		}
	}
	a.curSeg[num] = cg
}

// stageSegments reveals the mini-segments of an in-progress sector (replay
// only) — a prefix of the sector's final statuses, the rest zero-padded.
func (a *acc) stageSegments(num, lapNumber, idx int, segs []int) {
	if lapNumber < a.curLap[num] {
		return
	}
	if lapNumber > a.curLap[num] {
		a.curLap[num] = lapNumber
		a.curSec[num] = [3]*float64{}
		a.curSeg[num] = [3][]int{}
	}
	cg := a.curSeg[num]
	cg[idx] = segs
	a.curSeg[num] = cg
}

// stageSector reveals one sector of an in-progress lap (replay only) and
// keeps the best-time tables current.
func (a *acc) stageSector(num, lapNumber, idx int, v float64, segs []int) {
	if lapNumber < a.curLap[num] {
		return
	}
	if lapNumber > a.curLap[num] {
		a.curLap[num] = lapNumber
		a.curSec[num] = [3]*float64{}
		a.curSeg[num] = [3][]int{}
	}
	cs := a.curSec[num]
	cs[idx] = &v
	a.curSec[num] = cs
	cg := a.curSeg[num]
	cg[idx] = segs
	a.curSeg[num] = cg
	if v < a.bestSec[idx][num] || a.bestSec[idx][num] == 0 {
		a.bestSec[idx][num] = v
	}
	if v < a.minSec[idx] || a.minSec[idx] == 0 {
		a.minSec[idx] = v
	}
}

func (a *acc) setStints(all []stintRec) {
	a.stints = map[int][]stintRec{}
	for _, s := range all {
		a.stints[s.DriverNumber] = append(a.stints[s.DriverNumber], s)
	}
	for _, list := range a.stints {
		sort.Slice(list, func(i, j int) bool { return list[i].StintNumber < list[j].StintNumber })
	}
}

func (a *acc) applyRC(r rcRec) {
	a.rc = append(a.rc, r)
}

func gapString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f == 0 {
			return ""
		}
		return fmt.Sprintf("+%.3f", f)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// snapshot builds the renderable state from the accumulated records.
func (a *acc) snapshot(session *Session, now time.Time) model.State {
	st := model.State{LastUpdate: now, Source: "OpenF1"}
	if session != nil {
		st.SessionType = session.SessionType
		st.Session = &model.Session{
			Circuit:     session.CircuitShortName,
			SessionName: session.SessionName,
			Location:    session.Location,
			Country:     session.CountryName,
			Year:        session.Year,
		}
		st.CircuitKey = session.CircuitKey
		st.TrackOffset = model.ParseGmtOffset(session.GmtOffset)
	}

	rc := append([]rcRec(nil), a.rc...)
	sort.SliceStable(rc, func(i, j int) bool { return rc[i].Date.After(rc[j].Date.Time) })
	if len(rc) > 200 {
		rc = rc[:200]
	}
	for _, r := range rc {
		st.RaceControl = append(st.RaceControl, model.RaceControl{
			Date: r.Date.Time, Flag: r.Flag, Message: r.Message,
		})
	}

	st.SectorSegments = a.segCounts
	st.LapHistory = map[int][]model.LapRecord{}
	for num, h := range a.history {
		if len(h) > 0 {
			st.LapHistory[num] = append([]model.LapRecord(nil), h...)
		}
	}

	for num, p := range a.pos {
		d := a.drivers[num]
		row := model.Standing{
			Position:   p.Position,
			Number:     num,
			Acronym:    d.NameAcronym,
			Team:       d.TeamName,
			TeamColour: d.TeamColour,
			Pits:       a.pitCount[num],
		}
		if row.Acronym == "" {
			row.Acronym = fmt.Sprintf("#%d", num)
		}
		if g, ok := a.gaps[num]; ok {
			row.GapToLeader = gapString(g.GapToLeader)
			row.Interval = gapString(g.Interval)
		}
		row.BestLap = model.FormatLapSeconds(a.bestLap[num])
		// Quali/practice: OpenF1 has no interval data, so derive the gap
		// to the fastest lap from lap times.
		if session != nil && !strings.EqualFold(session.SessionType, "Race") &&
			row.GapToLeader == "" && a.bestLap[num] > 0 && a.minLap > 0 && a.bestLap[num] != a.minLap {
			row.GapToLeader = fmt.Sprintf("+%.3f", a.bestLap[num]-a.minLap)
		}
		lapNumber := 0
		if l, ok := a.lastLap[num]; ok {
			lapNumber = l.LapNumber
			row.Laps = l.LapNumber
			if l.LapDuration != nil {
				row.LastLap = model.FormatLapSeconds(*l.LapDuration)
				row.PersonalBest = *l.LapDuration == a.bestLap[num]
				row.OverallBestLap = *l.LapDuration == a.minLap
			}
			lapSecs := []*float64{l.Sector1, l.Sector2, l.Sector3}
			if cs, mid := a.curSec[num]; mid && a.curLap[num] > l.LapNumber {
				// A new lap is in progress: show its staged sectors as they
				// arrive; carry the previous lap's others over, dimmed.
				for i := range row.Sectors {
					switch {
					case cs[i] != nil:
						row.Sectors[i] = model.Sector{
							Value:        fmt.Sprintf("%.3f", *cs[i]),
							PersonalBest: *cs[i] == a.bestSec[i][num],
							OverallBest:  *cs[i] == a.minSec[i],
						}
					case lapSecs[i] != nil:
						row.Sectors[i] = model.Sector{
							Value: fmt.Sprintf("%.3f", *lapSecs[i]),
							Stale: true,
						}
					}
					// Dots always show the lap in progress, matching the live
					// feed: partly-run sectors have a zero-padded tail and
					// unreached ones are all placeholders.
					row.Sectors[i].Segments = a.curSeg[num][i]
				}
			} else {
				for i, sec := range lapSecs {
					if sec != nil {
						row.Sectors[i] = model.Sector{
							Value:        fmt.Sprintf("%.3f", *sec),
							PersonalBest: *sec == a.bestSec[i][num],
							OverallBest:  *sec == a.minSec[i],
							Segments:     l.segments()[i],
						}
					}
				}
			}
			if lapNumber > st.LeaderLap {
				st.LeaderLap = lapNumber
			}
			// Consecutive completed micro-segments from S1 on — the same
			// counter the live feed carries, so the map can place cars from
			// sector timing during replays. Mid-lap the staged segments give
			// live progress; right after a lap completes the full count puts
			// the car back at the line.
			segsNow := l.segments()
			if cg, ok := a.curSeg[num]; ok && a.curLap[num] > l.LapNumber {
				segsNow = cg
			}
			// Segments complete in order, so the car is past the LAST nonzero
			// one — scanning this way rides over data holes (OpenF1 reports
			// the first S1 segment as null on most laps).
			idx := 0
			for i := 0; i < 3; i++ {
				for _, status := range segsNow[i] {
					idx++
					if status != 0 {
						row.CompletedSegments = idx
					}
				}
			}
		}
		for _, s := range a.stints[num] {
			if s.LapStart <= lapNumber || row.Compound == "" {
				row.Compound = s.Compound
				row.TyreLaps = s.TyreAgeAtStart + (lapNumber - s.LapStart + 1)
			}
		}
		if row.TyreLaps < 0 {
			row.TyreLaps = 0
		}
		st.Standings = append(st.Standings, row)
	}
	sort.Slice(st.Standings, func(i, j int) bool {
		x, y := st.Standings[i], st.Standings[j]
		if x.Position != y.Position {
			if x.Position == 0 {
				return false
			}
			if y.Position == 0 {
				return true
			}
			return x.Position < y.Position
		}
		return x.Number < y.Number
	})
	st.BestLap, st.BestLapBy = model.SessionBestLap(st.Standings)
	return st
}

// ---- HTTP client ----

type Client struct {
	HTTP       *http.Client
	APIKey     string
	SessionKey string // "latest" or a numeric key

	mu      sync.Mutex
	session *Session
	a       acc
	nextReq time.Time

	posSince, gapSince, lapSince, rcSince time.Time
}

func New(sessionKey string) *Client {
	if sessionKey == "" {
		sessionKey = "latest"
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		SessionKey: sessionKey,
		a:          newAcc(),
	}
}

func (c *Client) get(ctx context.Context, endpoint string, params map[string]string, out any) error {
	// OpenF1's free tier allows 30 requests/minute (and 3/second bursts);
	// pace calls ~2.1s apart and back off once on a 429.
	for attempt := 0; ; attempt++ {
		if wait := time.Until(c.nextReq); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		c.nextReq = time.Now().Add(2100 * time.Millisecond)
		err := c.getOnce(ctx, endpoint, params, out)
		if err != nil && strings.Contains(err.Error(), "HTTP 429") && attempt < 2 {
			select {
			case <-time.After(20 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		return err
	}
}

func (c *Client) getOnce(ctx context.Context, endpoint string, params map[string]string, out any) error {
	var q []string
	for k, v := range params {
		q = append(q, url.QueryEscape(k)+"="+url.QueryEscape(v))
	}
	sort.Strings(q)
	u := baseURL + "/" + endpoint
	if len(q) > 0 {
		u += "?" + strings.Join(q, "&")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == 404 {
		return nil // OpenF1 404s on empty result sets — leave out zero-valued
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d: %.200s", endpoint, resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}

// dateFilter returns params filtering rows newer than t (when set).
func dateFilter(sessionKey string, field string, t time.Time) map[string]string {
	p := map[string]string{"session_key": sessionKey}
	if !t.IsZero() {
		p[field+">"] = t.Format("2006-01-02T15:04:05.000000")
	}
	return p
}

// Refresh performs one incremental polling cycle and returns the merged
// state — used for showing the most recent session's data.
func (c *Client) Refresh(ctx context.Context) model.State {
	c.mu.Lock()
	defer c.mu.Unlock()

	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if c.session == nil {
		var sessions []Session
		record(c.get(ctx, "sessions", map[string]string{"session_key": c.SessionKey}, &sessions))
		if len(sessions) > 0 {
			s := sessions[len(sessions)-1]
			c.session = &s
			c.SessionKey = fmt.Sprint(s.SessionKey)
		}
	}
	key := c.SessionKey

	if c.session != nil && len(c.a.drivers) == 0 {
		var drivers []driverRec
		record(c.get(ctx, "drivers", map[string]string{"session_key": key}, &drivers))
		for _, d := range drivers {
			c.a.drivers[d.DriverNumber] = d
		}
	}

	if c.session != nil {
		var positions []positionRec
		record(c.get(ctx, "position", dateFilter(key, "date", c.posSince), &positions))
		for _, p := range positions {
			c.a.applyPosition(p)
			if p.Date.After(c.posSince) {
				c.posSince = p.Date.Time
			}
		}

		var gaps []intervalRec
		record(c.get(ctx, "intervals", dateFilter(key, "date", c.gapSince), &gaps))
		for _, g := range gaps {
			c.a.applyInterval(g)
			if g.Date.After(c.gapSince) {
				c.gapSince = g.Date.Time
			}
		}

		var laps []lapRec
		record(c.get(ctx, "laps", dateFilter(key, "date_start", c.lapSince), &laps))
		for _, l := range laps {
			c.a.applyLap(l)
			if l.DateStart.After(c.lapSince) {
				c.lapSince = l.DateStart.Time
			}
		}

		var stints []stintRec
		record(c.get(ctx, "stints", map[string]string{"session_key": key}, &stints))
		if len(stints) > 0 {
			c.a.setStints(stints)
		}
		var pits []pitRec
		record(c.get(ctx, "pit", map[string]string{"session_key": key}, &pits))
		counts := map[int]int{}
		for _, p := range pits {
			counts[p.DriverNumber]++
		}
		if len(pits) > 0 {
			c.a.pitCount = counts
		}

		var rcs []rcRec
		record(c.get(ctx, "race_control", dateFilter(key, "date", c.rcSince), &rcs))
		for _, r := range rcs {
			c.a.applyRC(r)
			if r.Date.After(c.rcSince) {
				c.rcSince = r.Date.Time
			}
		}
	}

	st := c.a.snapshot(c.session, time.Now())
	if firstErr != nil {
		st.Err = firstErr.Error()
	}
	return st
}

// ---- replay browser: season → weekend → session ----

// FirstSeason is the earliest season OpenF1 carries data for.
const FirstSeason = 2023

// Seasons lists the selectable seasons, newest first.
func Seasons(now time.Time) []int {
	last := now.Year()
	if last < FirstSeason {
		last = FirstSeason
	}
	out := make([]int, 0, last-FirstSeason+1)
	for y := last; y >= FirstSeason; y-- {
		out = append(out, y)
	}
	return out
}

// ListMeetings returns a season's race weekends, newest first. Weekends that
// have not started yet are left out — there is nothing to replay in them.
func (c *Client) ListMeetings(ctx context.Context, year int) ([]model.MeetingOption, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var meetings []meetingRec
	if err := c.get(ctx, "meetings", map[string]string{"year": fmt.Sprint(year)}, &meetings); err != nil {
		return nil, err
	}
	now := time.Now()
	var out []model.MeetingOption
	for _, m := range meetings {
		if !m.DateStart.IsZero() && m.DateStart.After(now) {
			continue
		}
		out = append(out, model.MeetingOption{
			MeetingKey: m.MeetingKey,
			Name:       m.MeetingName,
			Location:   m.Location,
			Country:    m.CountryName,
			CircuitKey: m.CircuitKey,
			Date:       m.DateStart.Time,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

// ListMeetingSessions returns one weekend's finished sessions in the order
// they ran (practice first, race last).
func (c *Client) ListMeetingSessions(ctx context.Context, meetingKey int) ([]model.SessionOption, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var sessions []Session
	if err := c.get(ctx, "sessions", map[string]string{"meeting_key": fmt.Sprint(meetingKey)}, &sessions); err != nil {
		return nil, err
	}
	var name string
	var meetings []meetingRec
	if err := c.get(ctx, "meetings", map[string]string{"meeting_key": fmt.Sprint(meetingKey)}, &meetings); err == nil && len(meetings) > 0 {
		name = meetings[0].MeetingName
	}

	now := time.Now()
	var out []model.SessionOption
	for _, s := range sessions {
		// The calendar includes sessions still to come; only finished ones
		// can be replayed.
		if s.DateEnd.IsZero() || s.DateEnd.After(now) {
			continue
		}
		out = append(out, model.SessionOption{
			SessionKey:  s.SessionKey,
			MeetingName: name,
			SessionName: s.SessionName,
			Country:     s.CountryName,
			CircuitKey:  s.CircuitKey,
			Date:        s.DateStart.Time,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}

// ---- replayer ----

type event struct {
	t     time.Time
	apply func(*acc)
}

// Replayer replays a finished session by advancing a virtual clock through
// its time-stamped records.
type Replayer struct {
	Session     Session
	MeetingName string
	TotalLaps   int // scheduled distance, from the full lap data

	events  []event
	cursor  int
	a       acc
	virtual time.Time
	start   time.Time // initial clock position; rewind floor
	end     time.Time

	// Base state needed to rebuild the accumulator when rewinding.
	baseDrivers []driverRec
	baseStints  []stintRec
}

// LoadReplay fetches all records of a session (a handful of larger,
// rate-paced requests) and prepares a Replayer starting at session start.
func (c *Client) LoadReplay(ctx context.Context, sessionKey int) (*Replayer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := fmt.Sprint(sessionKey)

	var sessions []Session
	if err := c.get(ctx, "sessions", map[string]string{"session_key": key}, &sessions); err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("session %d not found", sessionKey)
	}
	r := &Replayer{Session: sessions[0], a: newAcc()}

	var meetings []meetingRec
	if err := c.get(ctx, "meetings", map[string]string{"meeting_key": fmt.Sprint(r.Session.MeetingKey)}, &meetings); err == nil && len(meetings) > 0 {
		r.MeetingName = meetings[0].MeetingName
	}

	var drivers []driverRec
	if err := c.get(ctx, "drivers", map[string]string{"session_key": key}, &drivers); err != nil {
		return nil, err
	}
	r.baseDrivers = drivers
	for _, d := range drivers {
		r.a.drivers[d.DriverNumber] = d
	}
	var stints []stintRec
	if err := c.get(ctx, "stints", map[string]string{"session_key": key}, &stints); err == nil {
		r.baseStints = stints
		r.a.setStints(stints)
	}

	var positions []positionRec
	if err := c.get(ctx, "position", map[string]string{"session_key": key}, &positions); err != nil {
		return nil, err
	}
	for _, p := range positions {
		p := p
		r.events = append(r.events, event{p.Date.Time, func(a *acc) { a.applyPosition(p) }})
	}
	var gaps []intervalRec
	if err := c.get(ctx, "intervals", map[string]string{"session_key": key}, &gaps); err == nil {
		for _, g := range gaps {
			g := g
			r.events = append(r.events, event{g.Date.Time, func(a *acc) { a.applyInterval(g) }})
		}
	}
	var laps []lapRec
	if err := c.get(ctx, "laps", map[string]string{"session_key": key}, &laps); err != nil {
		return nil, err
	}
	// The whole session is in hand, so the race distance is simply the
	// highest lap anyone reached — the live feed's LapCount equivalent.
	for _, l := range laps {
		if l.LapNumber > r.TotalLaps {
			r.TotalLaps = l.LapNumber
		}
	}
	for _, l := range laps {
		l := l
		// Reveal sectors one by one as they are set (OpenF1 has no
		// per-sector events, but lap start + sector durations give the
		// exact moments), then the full lap at completion. Mini-segments
		// within a sector are revealed on an even split of its duration —
		// the F1 mini-sector grid is ~equal-time, so that lands close to
		// when the car actually crossed each one.
		num, lap := l.DriverNumber, l.LapNumber
		counts := [3]int{len(l.Seg1), len(l.Seg2), len(l.Seg3)}
		r.events = append(r.events, event{l.DateStart.Time, func(a *acc) { a.stageLapStart(num, lap, counts) }})
		cum := l.DateStart.Time
		for i, sp := range []*float64{l.Sector1, l.Sector2, l.Sector3} {
			if sp == nil {
				break
			}
			secStart, secDur := cum, time.Duration(*sp*float64(time.Second))
			cum = cum.Add(secDur)
			i, v := i, *sp
			segs := l.segments()[i]
			for j := 1; j < len(segs); j++ {
				partial := make([]int, len(segs))
				copy(partial, segs[:j])
				t := secStart.Add(secDur * time.Duration(j) / time.Duration(len(segs)))
				r.events = append(r.events, event{t, func(a *acc) { a.stageSegments(num, lap, i, partial) }})
			}
			r.events = append(r.events, event{cum, func(a *acc) { a.stageSector(num, lap, i, v, segs) }})
		}
		end := l.DateStart.Time
		if l.LapDuration != nil {
			end = end.Add(time.Duration(*l.LapDuration * float64(time.Second)))
		} else if cum.After(end) {
			end = cum
		}
		r.events = append(r.events, event{end, func(a *acc) { a.applyLap(l) }})
	}
	var pits []pitRec
	if err := c.get(ctx, "pit", map[string]string{"session_key": key}, &pits); err == nil {
		for _, p := range pits {
			p := p
			r.events = append(r.events, event{p.Date.Time, func(a *acc) { a.pitCount[p.DriverNumber]++ }})
		}
	}
	var rcs []rcRec
	if err := c.get(ctx, "race_control", map[string]string{"session_key": key}, &rcs); err == nil {
		for _, m := range rcs {
			m := m
			r.events = append(r.events, event{m.Date.Time, func(a *acc) { a.applyRC(m) }})
		}
	}

	if len(r.events) == 0 {
		return nil, fmt.Errorf("no data for session %d", sessionKey)
	}
	sort.SliceStable(r.events, func(i, j int) bool { return r.events[i].t.Before(r.events[j].t) })
	// Recorded data (grid positions, pre-race messages) starts well before
	// lights-out; begin the clock shortly before the official start and
	// treat everything earlier as setup, applied immediately.
	r.virtual = r.events[0].t
	if s := r.Session.DateStart.Time; s.After(r.virtual) {
		r.virtual = s.Add(-2 * time.Minute)
	}
	r.start = r.virtual
	for r.cursor < len(r.events) && !r.events[r.cursor].t.After(r.virtual) {
		r.events[r.cursor].apply(&r.a)
		r.cursor++
	}
	r.end = r.events[len(r.events)-1].t
	return r, nil
}

// Advance moves the virtual clock forward and returns the state at the new
// time. The returned state carries the Grand Prix name and replay flags.
func (r *Replayer) Advance(d time.Duration) model.State {
	r.virtual = r.virtual.Add(d)
	for r.cursor < len(r.events) && !r.events[r.cursor].t.After(r.virtual) {
		r.events[r.cursor].apply(&r.a)
		r.cursor++
	}
	st := r.a.snapshot(&r.Session, r.virtual)
	st.MeetingName = r.MeetingName
	st.TotalLaps = r.TotalLaps
	st.IsReplay = true
	st.Source = "OpenF1 replay"
	return st
}

// Seek moves the virtual clock by d, which may be negative. Rewinding
// rebuilds the accumulator by replaying events from the beginning (all
// in-memory, fast). The clock never goes before the replay's start.
func (r *Replayer) Seek(d time.Duration) model.State {
	target := r.virtual.Add(d)
	if target.Before(r.start) {
		target = r.start
	}
	if target.Before(r.virtual) {
		r.a = newAcc()
		for _, dr := range r.baseDrivers {
			r.a.drivers[dr.DriverNumber] = dr
		}
		r.a.setStints(r.baseStints)
		r.cursor = 0
	}
	return r.Advance(target.Sub(r.virtual))
}

// Done reports whether the replay has consumed all events.
func (r *Replayer) Done() bool { return r.cursor >= len(r.events) }
