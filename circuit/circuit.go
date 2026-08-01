// Package circuit fetches track outlines from the MultiViewer API
// (api.multiviewer.app), which serves the circuit coordinate data of the F1
// timing system — the same coordinate space the live Position feed uses.
package circuit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"f1telemetry/model"
)

const baseURL = "https://api.multiviewer.app/api/v1"

var (
	mu    sync.Mutex
	cache = map[string]*model.TrackMap{}
)

// Fetch returns the outline for the given MultiViewer circuit key and year,
// caching results in-memory.
func Fetch(ctx context.Context, key, year int) (*model.TrackMap, error) {
	id := fmt.Sprintf("%d/%d", key, year)
	mu.Lock()
	if tm, ok := cache[id]; ok {
		mu.Unlock()
		return tm, nil
	}
	mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/circuits/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("circuit API: HTTP %d", resp.StatusCode)
	}

	var raw struct {
		CircuitName string    `json:"circuitName"`
		Rotation    float64   `json:"rotation"`
		X           []float64 `json:"x"`
		Y           []float64 `json:"y"`
		Corners     []struct {
			Number        int `json:"number"`
			TrackPosition struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
			} `json:"trackPosition"`
		} `json:"corners"`
		MarshalSectors []struct {
			Number        int `json:"number"`
			TrackPosition struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
			} `json:"trackPosition"`
		} `json:"marshalSectors"`
		MiniSectorsIndexes []int `json:"miniSectorsIndexes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if len(raw.X) < 2 || len(raw.X) != len(raw.Y) {
		return nil, fmt.Errorf("circuit API: no outline data")
	}

	tm := &model.TrackMap{Name: raw.CircuitName, Rotation: raw.Rotation}
	tm.Points = make([][2]float64, len(raw.X))
	for i := range raw.X {
		tm.Points[i] = [2]float64{raw.X[i], raw.Y[i]}
	}
	for _, c := range raw.Corners {
		tm.Corners = append(tm.Corners, model.Corner{
			Number: c.Number, X: c.TrackPosition.X, Y: c.TrackPosition.Y,
		})
	}

	// marshalSectors are the flag sectors race control refers to ("YELLOW IN
	// TRACK SECTOR 7"); each is positioned where it starts.
	for _, ms := range raw.MarshalSectors {
		tm.MarshalSectors = append(tm.MarshalSectors, model.Corner{
			Number: ms.Number, X: ms.TrackPosition.X, Y: ms.TrackPosition.Y,
		})
	}
	sort.Slice(tm.MarshalSectors, func(i, j int) bool {
		return tm.MarshalSectors[i].Number < tm.MarshalSectors[j].Number
	})

	// miniSectorsIndexes marks the F1 mini-sector boundaries as indices into
	// the outline. Store them as fractions [0,1) of lap length — the timing
	// sectors align to this grid, so it places sector lines accurately.
	arc := make([]float64, len(tm.Points))
	for i := 1; i < len(tm.Points); i++ {
		dx := tm.Points[i][0] - tm.Points[i-1][0]
		dy := tm.Points[i][1] - tm.Points[i-1][1]
		arc[i] = arc[i-1] + math.Hypot(dx, dy)
	}
	if total := arc[len(arc)-1]; total > 0 {
		for _, idx := range raw.MiniSectorsIndexes {
			if idx >= 0 && idx < len(arc) {
				tm.MiniSectors = append(tm.MiniSectors, arc[idx]/total)
			}
		}
		sort.Float64s(tm.MiniSectors)
	}
	mu.Lock()
	cache[id] = tm
	mu.Unlock()
	return tm, nil
}
