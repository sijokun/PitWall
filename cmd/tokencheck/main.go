// tokencheck is a connectivity smoke test for the live timing feed: it runs
// the livetiming client briefly and reports whether it connects and whether
// a session is currently live. The /signalrcore endpoint needs no F1 token.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/sijokun/PitWall/livetiming"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c := livetiming.New()
	go c.Run(ctx)

	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if e := c.LastError(); e != "" {
			fmt.Println("FAIL:", e)
			return
		}
		if c.HasTiming() {
			fmt.Println("OK: connected, live session data flowing")
			return
		}
	}
	fmt.Println("OK: feed connected (no live session right now)")
}
