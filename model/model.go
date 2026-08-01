// Package model holds the data-source-independent leaderboard state that the
// UI renders. Both data sources (F1 live timing, OpenF1) produce this.
package model

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Session struct {
	Circuit     string // e.g. "Spa-Francorchamps"
	SessionName string // e.g. "Race"
	Location    string
	Country     string
	Year        int
}

// Sector is one sector time of the current (or, if Stale, previous) lap.
type Sector struct {
	Value        string // e.g. "28.453", "" = no time yet
	PersonalBest bool
	OverallBest  bool
	Stale        bool // carried over from the previous lap
}

type Standing struct {
	Position       int
	Number         int
	Acronym        string
	Team           string
	TeamColour     string // hex without '#', e.g. "3671C6"
	GapToLeader    string
	Interval       string
	LastLap        string // "1:49.298", "" = none yet
	BestLap        string
	PersonalBest   bool
	OverallBestLap bool
	Laps           int
	KnockedOut     bool // eliminated in Q1/Q2
	Sectors        [3]Sector
	Compound       string
	TyreLaps       int
	Pits           int
	InPit          bool
	Retired        bool
	// CompletedSegments is the number of consecutive completed micro-segments
	// on the current lap — used to place the car on the map without GPS.
	CompletedSegments int
}

// LapRecord is one completed lap in a driver's history.
type LapRecord struct {
	Lap          int
	Time         string
	PersonalBest bool
	OverallBest  bool
	Sectors      [3]Sector
}

type RaceControl struct {
	Date    time.Time
	Flag    string // "YELLOW", "DOUBLE YELLOW", "CLEAR", "RED", ...
	Scope   string // "Track", "Sector", "Driver"
	Sector  int    // marshal sector the flag applies to (Scope == "Sector")
	Message string
}

// CarPos is one car's position on the circuit, in the F1 timing system's
// coordinate space (the same space as TrackMap points).
type CarPos struct {
	Number     int
	Acronym    string
	TeamColour string
	X, Y       float64
	OnTrack    bool
}

// TrackMap is a circuit outline (from the MultiViewer API).
type TrackMap struct {
	Name        string
	Rotation    float64      // degrees
	Points      [][2]float64 // closed track outline
	Corners     []Corner
	MiniSectors []float64 // F1 mini-sector boundaries as fractions [0,1) of lap length
	// MarshalSectors are the flag (marshal) sectors, each positioned at the
	// point where it starts; a sector runs to where the next one starts.
	MarshalSectors []Corner
}

type Corner struct {
	Number int
	X, Y   float64
}

// MeetingOption is one race weekend in the replay browser.
type MeetingOption struct {
	MeetingKey int
	Name       string // e.g. "Japanese Grand Prix"
	Location   string
	Country    string
	CircuitKey int
	Date       time.Time
}

// SessionOption is one entry in the past-session browser.
type SessionOption struct {
	SessionKey  int
	MeetingName string // e.g. "Japanese Grand Prix"
	SessionName string // e.g. "Race"
	Country     string
	CircuitKey  int
	Date        time.Time
}

type State struct {
	Session        *Session
	SessionType    string // "Race", "Qualifying", "Practice"
	Standings      []Standing
	LeaderLap      int
	TotalLaps      int
	TrackStatus    string              // "AllClear", "Yellow", "SCDeployed", "Red", "VSCDeployed", ...
	BestSectors    [3]string           // session overall-best S1/S2/S3 times ("" = none yet)
	BestLap        string              // session overall-best lap time ("" = none yet)
	BestLapBy      int                 // car number holding BestLap (0 = none yet)
	SectorSegments [3]int              // mini-segments per timing sector; relative S1/S2/S3 lengths for the map
	RaceControl    []RaceControl       // newest first
	LapHistory     map[int][]LapRecord // per driver number, oldest first
	CarPositions   []CarPos
	YellowSectors  map[int]bool // marshal sectors currently under (double) yellow
	CircuitKey     int          // MultiViewer circuit key from SessionInfo, 0 = unknown
	LastUpdate     time.Time
	Source         string // "F1 Live Timing" or "OpenF1"
	MeetingName    string // Grand Prix name, shown as the title during replays
	IsReplay       bool   // enables the REPLAY badge and CLOSE button
	Err            string
}

// sectorRe pulls the marshal sector out of a race control message for feeds
// that leave the structured field empty.
var sectorRe = regexp.MustCompile(`(?i)TRACK SECTOR\s+(\d+)`)

// MessageSector returns the marshal sector a race control message refers to:
// the structured field when set, else the one named in the text (0 = none).
func (rc RaceControl) MessageSector() int {
	if rc.Sector > 0 {
		return rc.Sector
	}
	if m := sectorRe.FindStringSubmatch(rc.Message); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// YellowSectorsFrom folds race control messages — newest first, as State
// carries them — into the set of marshal sectors currently under yellow or
// double yellow. A CLEAR for a sector lifts that sector; a green light, a
// track-wide clear, a red flag or the chequered flag lift everything.
func YellowSectorsFrom(msgs []RaceControl) map[int]bool {
	out := map[int]bool{}
	for i := len(msgs) - 1; i >= 0; i-- { // oldest first
		rc := msgs[i]
		flag := strings.ToUpper(strings.TrimSpace(rc.Flag))
		sector := rc.MessageSector()
		switch flag {
		case "YELLOW", "DOUBLE YELLOW":
			if sector > 0 {
				out[sector] = true
			}
		case "CLEAR":
			if sector > 0 {
				delete(out, sector)
				continue
			}
			clear(out) // track-wide clear
		case "GREEN", "RED", "CHEQUERED":
			clear(out)
		}
	}
	return out
}

// ParseLapSeconds reads a lap time back into seconds, accepting both the
// "M:SS.mmm" the feeds use for laps and the bare "SS.mmm" they use for
// sectors. It returns 0 for anything it cannot read, which callers treat the
// same as "no time set".
func ParseLapSeconds(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mins := 0.0
	if i := strings.IndexByte(s, ':'); i >= 0 {
		m, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0
		}
		mins, s = float64(m), s[i+1:]
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return mins*60 + sec
}

// SessionBestLap returns the fastest lap of the session and the car number
// that set it, derived from each driver's personal best. Ties go to the
// driver who appears first, matching the feed's own ordering. An empty time
// and 0 mean nobody has set a lap yet.
func SessionBestLap(rows []Standing) (string, int) {
	best, num := "", 0
	bestSec := 0.0
	for _, r := range rows {
		sec := ParseLapSeconds(r.BestLap)
		if sec <= 0 {
			continue
		}
		if bestSec == 0 || sec < bestSec {
			best, num, bestSec = r.BestLap, r.Number, sec
		}
	}
	return best, num
}

// FormatLapSeconds renders seconds as "M:SS.mmm".
func FormatLapSeconds(sec float64) string {
	if sec <= 0 {
		return ""
	}
	m := int(sec) / 60
	return fmt.Sprintf("%d:%06.3f", m, sec-float64(m*60))
}
