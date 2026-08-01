// livedebug exercises the exact live-timing code path the app uses
// (livetiming.Client) and reports what it sees: connection health, feed
// errors, and periodic snapshot summaries. Run it during a live session to
// find where the pipeline breaks — connect, timing arrival, or snapshot
// content.
//
//	go run ./cmd/livedebug
//	go run ./cmd/livedebug -duration 2m -dump snapshot.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"time"

	"f1telemetry/certs"
	"f1telemetry/dnsfix"
	"f1telemetry/livetiming"
	"f1telemetry/model"
)

func main() {
	duration := flag.Duration("duration", 0, "stop after this long (0 = run until Ctrl-C)")
	interval := flag.Duration("interval", 5*time.Second, "status report interval")
	dump := flag.String("dump", "", "write the final model snapshot as JSON to this file")
	rows := flag.Int("rows", 5, "standings rows to show per report")
	flag.Parse()

	certs.Install()
	dnsfix.Install()

	fmt.Println("[ok]   using /signalrcore endpoint — no F1 token required")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	c := livetiming.New()
	go c.Run(ctx)

	// Count coalesced update signals between reports to show feed liveness.
	var updates atomic.Int64
	go func() {
		for {
			select {
			case <-c.Updates():
				updates.Add(1)
			case <-ctx.Done():
				return
			}
		}
	}()

	start := time.Now()
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	var last model.State
	for {
		select {
		case <-ctx.Done():
			if *dump != "" {
				b, _ := json.MarshalIndent(last, "", "  ")
				if err := os.WriteFile(*dump, b, 0o644); err != nil {
					fmt.Println("[err]  dump:", err)
				} else {
					fmt.Println("[ok]   snapshot written to", *dump)
				}
			}
			return
		case <-tick.C:
		}

		fmt.Printf("\n===== t=%s  updates=%d =====\n", time.Since(start).Round(time.Second), updates.Swap(0))
		if e := c.LastError(); e != "" {
			fmt.Println("[err]  connection:", e)
		} else {
			fmt.Println("[ok]   no connection error")
		}
		fmt.Println("[..]   HasTiming:", c.HasTiming())

		st := c.Snapshot()
		last = st
		if st.Session != nil {
			fmt.Printf("[..]   session: %q type=%q circuit=%q year=%d\n",
				st.Session.SessionName, st.SessionType, st.Session.Circuit, st.Session.Year)
		} else {
			fmt.Println("[..]   session: <none> (SessionInfo topic not received yet)")
		}
		fmt.Printf("[..]   trackStatus=%q leaderLap=%d/%d raceControlMsgs=%d carPositions=%d standings=%d\n",
			st.TrackStatus, st.LeaderLap, st.TotalLaps, len(st.RaceControl), len(st.CarPositions), len(st.Standings))

		for i, s := range st.Standings {
			if i >= *rows {
				fmt.Printf("       ... %d more rows\n", len(st.Standings)-*rows)
				break
			}
			fmt.Printf("       P%-2d %-4s laps=%-2d gap=%-8q int=%-8q last=%-10q best=%-10q pit=%v out=%v ko=%v\n",
				s.Position, s.Acronym, s.Laps, s.GapToLeader, s.Interval, s.LastLap, s.BestLap,
				s.InPit, s.Retired, s.KnockedOut)
		}
	}
}
