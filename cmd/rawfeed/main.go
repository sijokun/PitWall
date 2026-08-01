// rawfeed is a wire tap on the F1 live timing SignalR feed. It reuses the
// app's own connection code (livetiming.DumpRaw) but performs NO merge or
// snapshot logic, so it shows exactly what arrives off the socket. Use it to
// answer: are we connecting? is the token accepted? is data flowing? which
// topics? — before blaming the app's parsing.
//
//	go run ./cmd/rawfeed                              # print every message
//	go run ./cmd/rawfeed -topics TimingData,TrackStatus
//	go run ./cmd/rawfeed -full                        # print full payloads
//	go run ./cmd/rawfeed -record session.txt          # capture for offline replay
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"f1telemetry/certs"
	"f1telemetry/dnsfix"
	"f1telemetry/livetiming"
)

func main() {
	topicsFlag := flag.String("topics", "", "comma-separated topics to show (default: all)")
	full := flag.Bool("full", false, "print full JSON payloads (default: one-line summary)")
	record := flag.String("record", "", "append each feed message to this file in fastf1 replay format")
	duration := flag.Duration("duration", 0, "stop after this long (0 = until Ctrl-C)")
	flag.Parse()

	certs.Install()
	dnsfix.Install()

	var wanted map[string]bool
	if *topicsFlag != "" {
		wanted = map[string]bool{}
		for _, t := range strings.Split(*topicsFlag, ",") {
			wanted[strings.TrimSpace(t)] = true
		}
	}

	var recMu sync.Mutex
	var recFile *os.File
	if *record != "" {
		f, err := os.Create(*record)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[err] open record file:", err)
			os.Exit(1)
		}
		recFile = f
		defer recFile.Close()
		fmt.Fprintln(os.Stderr, "[ok] recording to", *record)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	counts := map[string]int{}
	var mu sync.Mutex

	fn := func(ev livetiming.RawEvent) {
		mu.Lock()
		counts[ev.Topic+"|"+ev.Kind]++
		mu.Unlock()

		ts := ev.Timestamp.UTC().Format("15:04:05.000")
		switch ev.Kind {
		case "negotiate":
			fmt.Printf("%s  NEGOTIATE  %v\n", ts, ev.Payload)
			return
		case "other":
			// SignalR keepalives ({}) and control frames — show length only.
			fmt.Printf("%s  ·keepalive/control· (%d bytes)\n", ts, len(ev.Raw))
			return
		}

		if wanted != nil && !wanted[ev.Topic] {
			return
		}

		// Record feed messages in fastf1's ['Topic', payload, 'timestamp']
		// python-literal line format, so they replay through ReplayFileTimed.
		if recFile != nil && ev.Kind == "feed" {
			writeReplayLine(recFile, &recMu, ev)
		}

		tag := strings.ToUpper(ev.Kind)
		if *full {
			b, _ := json.MarshalIndent(ev.Payload, "", "  ")
			fmt.Printf("%s  %-8s %-22s\n%s\n", ts, tag, ev.Topic, b)
		} else {
			fmt.Printf("%s  %-8s %-22s %s\n", ts, tag, ev.Topic, summarize(ev.Payload))
		}
	}

	err := livetiming.DumpRaw(ctx, fn)

	fmt.Println("\n===== message counts (topic|kind) =====")
	mu.Lock()
	for k, v := range counts {
		fmt.Printf("  %-40s %d\n", k, v)
	}
	mu.Unlock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n[err]", err)
		os.Exit(1)
	}
}

// summarize gives a compact one-line view of a payload for scanning.
func summarize(v any) string {
	switch p := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		s := strings.Join(keys, ",")
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		return "{" + s + "}"
	case string:
		if len(p) > 120 {
			return p[:120] + "…"
		}
		return p
	default:
		b, _ := json.Marshal(v)
		s := string(b)
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		return s
	}
}

// writeReplayLine appends one message in the fastf1 replay literal format.
// The payload is JSON (a valid subset of python literal syntax for these
// structures), which ReplayFileTimed's pyLiteralToJSON reads back fine.
func writeReplayLine(f *os.File, mu *sync.Mutex, ev livetiming.RawEvent) {
	// ".z" topics (Position.z) are decoded in RawEvent.Payload; the replay
	// path expects them as the raw base64 string, so a decoded line would be
	// unreplayable. Skip them — they only drive the track map, not timing.
	if strings.HasSuffix(ev.Topic, ".z") {
		return
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return
	}
	topic, _ := json.Marshal(ev.Topic)
	tsStr, _ := json.Marshal(ev.Timestamp.UTC().Format(time.RFC3339Nano))
	line := fmt.Sprintf("[%s, %s, %s]\n", topic, payload, tsStr)
	mu.Lock()
	f.WriteString(line)
	mu.Unlock()
}
