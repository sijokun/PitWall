// Package livetiming is a client for Formula 1's public live timing feed
// (livetiming.formula1.com) — the same free SignalR stream used by the F1
// app and community libraries like FastF1/livef1. No authentication needed;
// data is real-time during sessions.
package livetiming

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sijokun/PitWall/certs"
	"github.com/sijokun/PitWall/model"
)

const (
	host      = "livetiming.formula1.com"
	basePath  = "/signalrcore" // modern ASP.NET Core SignalR endpoint (no auth)
	userAgent = "BestHTTP"
	originURL = "https://www.formula1.com"

	// SignalR Core frames every JSON record with a 0x1E record separator.
	recordSep = "\x1e"

	// SignalR Core message types.
	msgInvocation = 1 // feed events + our Subscribe call
	msgCompletion = 3 // Subscribe reply carrying the full snapshot
	msgPing       = 6
	msgClose      = 7
)

// Topics we subscribe to. Position.z (compressed, ~3.7 Hz) feeds the track
// map; CarData.z (RPM/throttle etc.) is excluded as nothing renders it.
var topics = []string{
	"Heartbeat", "DriverList", "TimingData", "TimingAppData", "TimingStats",
	"TrackStatus", "SessionInfo", "LapCount", "RaceControlMessages",
	"Position.z",
}

// fallbackSeason is the season fallbackDrivers describes. Car numbers are
// reassigned every year (the champion takes #1, drivers move teams), so the
// table is only safe for a feed we have confirmed is from this season.
const fallbackSeason = 2026

// fallbackDrivers maps car numbers to driver/team info for the current
// (2026) grid — used only until the feed's DriverList snapshot arrives
// (e.g. replays recorded mid-session). Live data always wins.
var fallbackDrivers = map[int]struct{ Tla, Team, Colour string }{
	12: {"ANT", "Mercedes", "27F4D2"},
	63: {"RUS", "Mercedes", "27F4D2"},
	44: {"HAM", "Ferrari", "E8002D"},
	16: {"LEC", "Ferrari", "E8002D"},
	3:  {"VER", "Red Bull Racing", "0000FF"},
	6:  {"HAD", "Red Bull Racing", "0000FF"},
	81: {"PIA", "McLaren", "FF8000"},
	1:  {"NOR", "McLaren", "FF8000"},
	14: {"ALO", "Aston Martin", "229971"},
	18: {"STR", "Aston Martin", "229971"},
	10: {"GAS", "Alpine", "8BFFFF"},
	43: {"COL", "Alpine", "8BFFFF"},
	30: {"LAW", "Racing Bulls", "6692FF"},
	41: {"LIN", "Racing Bulls", "6692FF"},
	31: {"OCO", "Haas", "E6AEB1"},
	87: {"BEA", "Haas", "E6AEB1"},
	27: {"HUL", "Audi", "FF5555"},
	5:  {"BOR", "Audi", "FF5555"},
	55: {"SAI", "Williams", "1868DB"},
	23: {"ALB", "Williams", "1868DB"},
	11: {"PER", "Cadillac", "CCC8CB"},
	77: {"BOT", "Cadillac", "CCC8CB"},
}

type posEntry struct {
	x, y    float64
	onTrack bool
}

type Client struct {
	mu        sync.Mutex
	state     map[string]any // merged per-topic state
	positions map[string]posEntry
	secSeq    int64
	secSet    map[string][3]int64 // per driver: seq at which each sector was last set
	history   map[string][]model.LapRecord
	err       string
	up        bool
	updates   chan struct{}

	// raw carries websocket messages from the reader goroutine to the
	// processor, decoupling socket reads from the (heavier) merge so a burst
	// never backs up in the kernel socket buffer.
	raw chan []byte

	// Live delay (TV sync): feed events are timestamped on arrival and held
	// in pending until delay has elapsed, then applied to the visible state.
	delay   time.Duration
	pending []timedEvent
}

// timedEvent is one feed update awaiting its delayed application.
type timedEvent struct {
	at    time.Time
	topic string
	patch any
}

func New() *Client {
	return &Client{
		state:     map[string]any{},
		positions: map[string]posEntry{},
		secSet:    map[string][3]int64{},
		history:   map[string][]model.LapRecord{},
		updates:   make(chan struct{}, 1),
		raw:       make(chan []byte, 1024),
	}
}

// Updates signals (coalesced) whenever new feed data has been applied —
// consume it to redraw on events instead of polling.
func (c *Client) Updates() <-chan struct{} { return c.updates }

// LastError returns the most recent connection error ("" while healthy).
func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) notify() {
	select {
	case c.updates <- struct{}{}:
	default:
	}
}

// SetDelay sets the live delay (0 = realtime). Increasing it holds new feed
// data back; decreasing it (or 0) flushes anything already due on the next
// pump tick. Safe to call at any time.
func (c *Client) SetDelay(d time.Duration) {
	if d < 0 {
		d = 0
	}
	c.mu.Lock()
	c.delay = d
	c.mu.Unlock()
}

// pump applies queued websocket messages. It batches everything currently
// buffered into a single locked merge followed by one notify, so a burst of
// messages collapses into one state update instead of one merge+render per
// message — that's what stops the display from falling seconds behind.
func (c *Client) pump(ctx context.Context) {
	for {
		var data []byte
		select {
		case <-ctx.Done():
			return
		case data = <-c.raw:
		}
		c.mu.Lock()
		c.applyMessage(data)
		// Drain whatever else is already queued into the same batch.
	drain:
		for {
			select {
			case more := <-c.raw:
				c.applyMessage(more)
			default:
				break drain
			}
		}
		c.up = true
		c.err = ""
		c.mu.Unlock()
		c.notify()
	}
}

// applyMessage parses one websocket message (which may carry several
// 0x1E-separated records) and applies each. Caller holds c.mu.
func (c *Client) applyMessage(data []byte) {
	for _, rec := range strings.Split(string(data), recordSep) {
		if len(rec) < 3 { // empty trailing segment or the "{}" handshake ack
			continue
		}
		c.handleFrame([]byte(rec))
	}
}

// ingest applies a feed update, or buffers it when a delay is set. Caller
// holds c.mu.
func (c *Client) ingest(topic string, patch any) {
	if c.delay <= 0 {
		c.applyTopic(topic, patch)
		return
	}
	c.pending = append(c.pending, timedEvent{at: time.Now(), topic: topic, patch: patch})
}

// delayPump applies buffered feed events once their delay has elapsed. It's a
// no-op while delay is 0 (events are applied inline by ingest).
func (c *Client) delayPump(ctx context.Context) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		c.mu.Lock()
		applied := 0
		for len(c.pending) > 0 {
			ev := c.pending[0]
			if c.delay > 0 && now.Sub(ev.at) < c.delay {
				break
			}
			c.applyTopic(ev.topic, ev.patch)
			c.pending = c.pending[1:]
			applied++
		}
		c.mu.Unlock()
		if applied > 0 {
			c.notify()
		}
	}
}

// Run connects and pumps the feed until ctx is cancelled, reconnecting with
// backoff on errors.
func (c *Client) Run(ctx context.Context) {
	go c.delayPump(ctx)
	go c.pump(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.connectAndPump(ctx)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		c.up = false
		if err != nil {
			c.err = err.Error()
		}
		c.mu.Unlock()
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) connectAndPump(ctx context.Context) error {
	// SignalR Core negotiate is a POST; it yields a connectionToken and a
	// session cookie that must be echoed back on the websocket connect.
	// The /signalrcore endpoint needs no F1 account token.
	nurl := fmt.Sprintf("https://%s%s/negotiate?negotiateVersion=1", host, basePath)
	req, err := http.NewRequestWithContext(ctx, "POST", nurl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Origin", originURL)
	req.Header.Set("Referer", originURL+"/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("negotiate: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("negotiate: HTTP %d: %.200s", resp.StatusCode, body)
	}
	var neg struct {
		ConnectionToken string `json:"connectionToken"`
	}
	if err := json.Unmarshal(body, &neg); err != nil {
		return fmt.Errorf("negotiate parse: %w", err)
	}
	if neg.ConnectionToken == "" {
		return fmt.Errorf("negotiate: missing connectionToken")
	}
	var cookies []string
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if i := strings.IndexByte(sc, ';'); i > 0 {
			sc = sc[:i]
		}
		cookies = append(cookies, sc)
	}

	wsURL := fmt.Sprintf("wss://%s%s?id=%s", host, basePath, url.QueryEscape(neg.ConnectionToken))
	hdr := http.Header{"User-Agent": {userAgent}, "Origin": {originURL}}
	if len(cookies) > 0 {
		hdr.Set("Cookie", strings.Join(cookies, "; "))
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{RootCAs: certs.Pool()},
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return fmt.Errorf("ws connect: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(32 << 20)

	// gorilla forbids concurrent writes; the ping goroutine and this loop
	// both write, so serialise them.
	var writeMu sync.Mutex
	send := func(v any) error {
		payload, _ := json.Marshal(v)
		payload = append(payload, recordSep...)
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, payload)
	}

	// SignalR Core requires the JSON protocol handshake before any hub call.
	if err := send(map[string]any{"protocol": "json", "version": 1}); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if err := send(map[string]any{
		"arguments": []any{topics}, "invocationId": "1", "target": "Subscribe", "type": msgInvocation,
	}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// Close the socket when ctx ends so ReadMessage unblocks.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	// Keep-alive: the server drops silent clients after ~30s.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if send(map[string]any{"type": msgPing}) != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Read loop: do nothing but drain the socket into c.raw so a burst can
	// never back up in the kernel buffer. The processor (pump) does the merge.
	// gorilla returns a fresh slice per ReadMessage, so handing it off is safe.
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("ws read: %w", err)
		}
		select {
		case c.raw <- data:
		case <-ctx.Done():
			return nil
		}
	}
}

// handleFrame routes one SignalR Core record. Caller holds c.mu.
func (c *Client) handleFrame(rec []byte) {
	var frame struct {
		Type      int               `json:"type"`
		Target    string            `json:"target"`
		Arguments []json.RawMessage `json:"arguments"`
		Result    map[string]any    `json:"result"`
	}
	if json.Unmarshal(rec, &frame) != nil {
		return
	}
	switch frame.Type {
	case msgInvocation:
		// Live updates: target "feed", arguments = [channel, payload, ts].
		if frame.Target != "feed" || len(frame.Arguments) < 2 {
			return
		}
		var topic string
		var patch any
		if json.Unmarshal(frame.Arguments[0], &topic) != nil || json.Unmarshal(frame.Arguments[1], &patch) != nil {
			return
		}
		c.ingest(topic, patch)
	case msgCompletion:
		// Subscribe reply: the full initial snapshot keyed by channel.
		for topic, v := range frame.Result {
			c.ingest(topic, v)
		}
	}
}

// merge applies a SignalR delta patch: maps merge recursively, everything
// else replaces. A map patch applied onto an array updates/appends elements
// by numeric key (how the feed extends lists like race control messages).
func merge(dst, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	switch d := dst.(type) {
	case map[string]any:
		for k, v := range pm {
			d[k] = merge(d[k], v)
		}
		return d
	case []any:
		for k, v := range pm {
			idx, err := strconv.Atoi(k)
			if err != nil || idx < 0 {
				continue
			}
			for idx >= len(d) {
				d = append(d, nil)
			}
			d[idx] = merge(d[idx], v)
		}
		return d
	default:
		return patch
	}
}

// ---- snapshot extraction ----

func getMap(v any, keys ...string) map[string]any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	m, _ := v.(map[string]any)
	return m
}

func getStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func getInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return 0
}

func getFloat(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	}
	return 0
}

func getBool(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "True"
	}
	return false
}

// applyTopic routes one topic update into state. Caller holds c.mu.
// Topics ending in ".z" carry a base64+zlib compressed JSON payload.
func (c *Client) applyTopic(topic string, patch any) {
	if strings.HasSuffix(topic, ".z") {
		enc, ok := patch.(string)
		if !ok {
			return
		}
		decoded, err := decompress(enc)
		if err != nil {
			return
		}
		var payload any
		if json.Unmarshal(decoded, &payload) != nil {
			return
		}
		if strings.TrimSuffix(topic, ".z") == "Position" {
			c.applyPosition(payload)
		}
		return
	}
	if topic == "TimingData" {
		c.trackSectorUpdates(patch)
	}
	c.state[topic] = merge(c.state[topic], patch)
	if topic == "TimingData" {
		c.recordLaps(patch)
	}
}

// recordLaps appends to per-driver lap history whenever a patch delivers a
// new LastLapTime value. Reads the merged state, so the lap's sectors and
// best-flags are all present.
func (c *Client) recordLaps(patch any) {
	for num, lv := range getMap(patch, "Lines") {
		pl, ok := lv.(map[string]any)
		if !ok {
			continue
		}
		if getStr(getMap(pl["LastLapTime"]), "Value") == "" {
			continue
		}
		line := getMap(c.state["TimingData"], "Lines", num)
		if line == nil {
			continue
		}
		last := getMap(line["LastLapTime"])
		rec := model.LapRecord{
			Lap:          getInt(line, "NumberOfLaps"),
			Time:         getStr(last, "Value"),
			PersonalBest: getBool(last, "PersonalFastest"),
			OverallBest:  getBool(last, "OverallFastest"),
		}
		for i, sv := range indexedList(line["Sectors"]) {
			if i >= 3 {
				break
			}
			sm, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			rec.Sectors[i] = model.Sector{
				Value:        getStr(sm, "Value"),
				PersonalBest: getBool(sm, "PersonalFastest"),
				OverallBest:  getBool(sm, "OverallFastest"),
			}
		}
		h := c.history[num]
		if n := len(h); n > 0 && h[n-1].Lap == rec.Lap {
			h[n-1] = rec
		} else {
			h = append(h, rec)
			if len(h) > 40 {
				h = h[len(h)-40:]
			}
		}
		c.history[num] = h
	}
}

// trackSectorUpdates records the order in which sector times arrive, so the
// snapshot can dim sectors carried over from the previous lap: once a new
// S1 lands, S2/S3 values older than it belong to the last lap.
func (c *Client) trackSectorUpdates(patch any) {
	for num, lv := range getMap(patch, "Lines") {
		line, ok := lv.(map[string]any)
		if !ok {
			continue
		}
		mark := func(idx int, v any) {
			sm, ok := v.(map[string]any)
			if !ok || idx < 0 || idx >= 3 {
				return
			}
			if getStr(sm, "Value") == "" {
				return
			}
			c.secSeq++
			set := c.secSet[num]
			set[idx] = c.secSeq
			c.secSet[num] = set
		}
		switch sv := line["Sectors"].(type) {
		case []any:
			for i, v := range sv {
				mark(i, v)
			}
		case map[string]any:
			for k, v := range sv {
				if idx, err := strconv.Atoi(k); err == nil {
					mark(idx, v)
				}
			}
		}
	}
}

// applyPosition ingests a decompressed Position batch:
// {"Position":[{"Timestamp":...,"Entries":{"1":{"Status":"OnTrack","X":..,"Y":..}}}]}
func (c *Client) applyPosition(payload any) {
	batches, ok := getMap(payload)["Position"].([]any)
	if !ok {
		return
	}
	for _, b := range batches {
		for num, e := range getMap(b, "Entries") {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			c.positions[num] = posEntry{
				x:       getFloat(em, "X"),
				y:       getFloat(em, "Y"),
				onTrack: getStr(em, "Status") == "OnTrack",
			}
		}
	}
}

func decompress(encoded string) ([]byte, error) {
	switch len(encoded) % 4 {
	case 2:
		encoded += "=="
	case 3:
		encoded += "="
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	// The feed uses a raw deflate stream (no zlib header) for some payloads
	// and zlib for others; try zlib first, then raw deflate via zlib shim.
	if r, err := zlib.NewReader(bytes.NewReader(b)); err == nil {
		defer r.Close()
		return io.ReadAll(r)
	}
	r := flate.NewReader(bytes.NewReader(b))
	defer r.Close()
	return io.ReadAll(r)
}

// indexedList normalizes a feed list that may arrive either as a JSON array
// (full snapshot) or as an object keyed by numeric index (delta patches,
// e.g. {"1": {...}} for Stints or RaceControlMessages.Messages). Returns
// elements in index order.
func indexedList(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case map[string]any:
		type kv struct {
			idx int
			val any
		}
		var items []kv
		for k, val := range l {
			idx, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			items = append(items, kv{idx, val})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].idx < items[j].idx })
		out := make([]any, len(items))
		for i, it := range items {
			out[i] = it.val
		}
		return out
	}
	return nil
}

// sectorSegments returns the ordered Segments list of timing sector i from a
// driver line's Sectors value (an array in snapshots, an index-keyed map in
// deltas).
func sectorSegments(sectorsVal any, i int) []any {
	var sec any
	switch sv := sectorsVal.(type) {
	case []any:
		if i >= 0 && i < len(sv) {
			sec = sv[i]
		}
	case map[string]any:
		sec = sv[strconv.Itoa(i)]
	}
	sm, ok := sec.(map[string]any)
	if !ok {
		return nil
	}
	return indexedList(sm["Segments"])
}

// ApplyFeed merges one feed update into the client state — used by the
// replay tooling; the live connection calls merge directly.
func (c *Client) ApplyFeed(topic string, patch any) {
	c.mu.Lock()
	c.applyTopic(topic, patch)
	c.up = true
	c.mu.Unlock()
	c.notify()
}

// HasTiming reports whether live per-driver timing has arrived — the signal
// that a session is actually running.
func (c *Client) HasTiming() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.up && len(getMap(c.state["TimingData"], "Lines")) > 0
}

func (c *Client) Snapshot() model.State {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := model.State{LastUpdate: time.Now(), Source: "F1 Live Timing", Err: c.err}

	if si := getMap(c.state["SessionInfo"]); si != nil {
		st.SessionType = getStr(si, "Type")
		s := &model.Session{
			SessionName: getStr(si, "Name"),
			Circuit:     getStr(getMap(si["Meeting"], "Circuit"), "ShortName"),
			Country:     getStr(getMap(si["Meeting"], "Country"), "Name"),
			Location:    getStr(getMap(si, "Meeting"), "Location"),
		}
		if sd := getStr(si, "StartDate"); len(sd) >= 4 {
			s.Year, _ = strconv.Atoi(sd[:4])
		}
		st.Session = s
		st.CircuitKey = getInt(getMap(si["Meeting"], "Circuit"), "Key")
	}
	if lc := getMap(c.state["LapCount"]); lc != nil {
		st.LeaderLap = getInt(lc, "CurrentLap")
		st.TotalLaps = getInt(lc, "TotalLaps")
	}
	if ts := getMap(c.state["TrackStatus"]); ts != nil {
		st.TrackStatus = getStr(ts, "Message")
	}

	// Overall-best sector times: the feed's TimingStats gives each driver a
	// BestSectors[i] (Value + Position, 1 = session fastest). Take the fastest
	// value seen across all drivers for each of the three sectors.
	var bestSec [3]float64
	for _, lv := range getMap(c.state["TimingStats"], "Lines") {
		line, ok := lv.(map[string]any)
		if !ok {
			continue
		}
		for i, sv := range indexedList(line["BestSectors"]) {
			if i >= 3 {
				break
			}
			sm, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			v := getStr(sm, "Value")
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 {
				continue
			}
			if bestSec[i] == 0 || f < bestSec[i] {
				bestSec[i] = f
				st.BestSectors[i] = v
			}
		}
	}

	if msgs := indexedList(getMap(c.state["RaceControlMessages"])["Messages"]); msgs != nil {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			rc := model.RaceControl{
				Flag:    getStr(mm, "Flag"),
				Scope:   getStr(mm, "Scope"),
				Sector:  int(getFloat(mm, "Sector")),
				Message: getStr(mm, "Message"),
			}
			for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
				if t, err := time.Parse(layout, getStr(mm, "Utc")); err == nil {
					rc.Date = t.UTC()
					break
				}
			}
			st.RaceControl = append(st.RaceControl, rc)
		}
		sort.SliceStable(st.RaceControl, func(i, j int) bool {
			return st.RaceControl[i].Date.After(st.RaceControl[j].Date)
		})
		// Fold the full log — a yellow raised long ago may sit outside the
		// 30 messages kept for display.
		st.YellowSectors = model.YellowSectorsFrom(st.RaceControl)
		if len(st.RaceControl) > 30 {
			st.RaceControl = st.RaceControl[:30]
		}
	}

	st.LapHistory = map[int][]model.LapRecord{}
	for num, h := range c.history {
		n, err := strconv.Atoi(num)
		if err != nil || len(h) == 0 {
			continue
		}
		st.LapHistory[n] = append([]model.LapRecord(nil), h...)
	}

	drivers := getMap(c.state["DriverList"])

	// Only guess from the hardcoded grid once SessionInfo has confirmed the
	// feed is from fallbackSeason. A recording from an earlier year reuses the
	// same car numbers for different drivers (#1 is whoever won last season),
	// so guessing there labels the wrong driver rather than leaving it blank.
	useFallback := st.Session != nil && st.Session.Year == fallbackSeason

	for num, p := range c.positions {
		cp := model.CarPos{X: p.x, Y: p.y, OnTrack: p.onTrack}
		cp.Number, _ = strconv.Atoi(num)
		if d := getMap(drivers[num]); d != nil {
			cp.Acronym = getStr(d, "Tla")
			cp.TeamColour = getStr(d, "TeamColour")
		}
		if fb, ok := fallbackDrivers[cp.Number]; ok && useFallback {
			if cp.Acronym == "" {
				cp.Acronym = fb.Tla
			}
			if cp.TeamColour == "" {
				cp.TeamColour = fb.Colour
			}
		}
		st.CarPositions = append(st.CarPositions, cp)
	}
	sort.Slice(st.CarPositions, func(i, j int) bool {
		return st.CarPositions[i].Number < st.CarPositions[j].Number
	})

	appLines := getMap(c.state["TimingAppData"], "Lines")
	var segCounts [3]int // mini-segments per timing sector (max across drivers)
	for num, lv := range getMap(c.state["TimingData"], "Lines") {
		line, ok := lv.(map[string]any)
		if !ok {
			continue
		}
		row := model.Standing{
			Position:   getInt(line, "Position"),
			Pits:       getInt(line, "NumberOfPitStops"),
			Laps:       getInt(line, "NumberOfLaps"),
			InPit:      getBool(line, "InPit"),
			Retired:    getBool(line, "Retired") || getBool(line, "Stopped"),
			KnockedOut: getBool(line, "KnockedOut"),
		}
		row.Number, _ = strconv.Atoi(num)

		if d := getMap(drivers[num]); d != nil {
			row.Acronym = getStr(d, "Tla")
			row.Team = getStr(d, "TeamName")
			row.TeamColour = getStr(d, "TeamColour")
		}
		if fb, ok := fallbackDrivers[row.Number]; ok && useFallback {
			if row.Acronym == "" {
				row.Acronym = fb.Tla
			}
			if row.Team == "" {
				row.Team = fb.Team
			}
			if row.TeamColour == "" {
				row.TeamColour = fb.Colour
			}
		}
		if row.Acronym == "" {
			row.Acronym = "#" + num
		}

		// Race sessions carry GapToLeader/IntervalToPositionAhead; practice
		// and qualifying carry TimeDiffToFastest/TimeDiffToPositionAhead.
		row.GapToLeader = getStr(line, "GapToLeader")
		if row.GapToLeader == "" {
			row.GapToLeader = getStr(line, "TimeDiffToFastest")
		}
		row.Interval = getStr(getMap(line["IntervalToPositionAhead"]), "Value")
		if row.Interval == "" {
			row.Interval = getStr(line, "TimeDiffToPositionAhead")
		}
		if last := getMap(line["LastLapTime"]); last != nil {
			row.LastLap = getStr(last, "Value")
			row.PersonalBest = getBool(last, "PersonalFastest")
			row.OverallBestLap = getBool(last, "OverallFastest")
		}
		if best := getMap(line["BestLapTime"]); best != nil {
			row.BestLap = getStr(best, "Value")
		}

		// Sectors arrive as an array (snapshot) or a sparse index-keyed map
		// (deltas) — assign by real index either way.
		assignSector := func(idx int, v any) {
			sm, ok := v.(map[string]any)
			if !ok || idx < 0 || idx >= 3 {
				return
			}
			s := model.Sector{Value: getStr(sm, "Value")}
			if s.Value == "" {
				s.Value = getStr(sm, "PreviousValue")
				s.Stale = true
			} else {
				s.PersonalBest = getBool(sm, "PersonalFastest")
				s.OverallBest = getBool(sm, "OverallFastest")
			}
			row.Sectors[idx] = s
		}
		switch sv := line["Sectors"].(type) {
		case []any:
			for i, v := range sv {
				assignSector(i, v)
			}
		case map[string]any:
			for k, v := range sv {
				if idx, err := strconv.Atoi(k); err == nil {
					assignSector(idx, v)
				}
			}
		}
		// Dim sectors set before the current lap's S1 — they belong to the
		// previous lap until the car sets them again.
		if set, ok := c.secSet[num]; ok {
			for i := 1; i < 3; i++ {
				if set[i] < set[0] && row.Sectors[i].Value != "" {
					row.Sectors[i].Stale = true
					row.Sectors[i].PersonalBest = false
					row.Sectors[i].OverallBest = false
				}
			}
		}

		// Micro-segments per sector (max across drivers → relative S1/S2/S3
		// lengths for the map) and, per driver, how many consecutive segments
		// are completed (Status 2048/2049/2051) from S1 on — i.e. how far
		// around the lap the car is, for placing it on the map without GPS.
		var segs [3][]any
		for si := 0; si < 3; si++ {
			segs[si] = sectorSegments(line["Sectors"], si)
			if len(segs[si]) > segCounts[si] {
				segCounts[si] = len(segs[si])
			}
		}
	countCompleted:
		for si := 0; si < 3; si++ {
			for _, sv := range segs[si] {
				switch getInt(getMap(sv), "Status") {
				case 2048, 2049, 2051:
					row.CompletedSegments++
				default:
					break countCompleted
				}
			}
		}

		if app := getMap(appLines[num]); app != nil {
			if stints := indexedList(app["Stints"]); len(stints) > 0 {
				if s := getMap(stints[len(stints)-1]); s != nil {
					row.Compound = getStr(s, "Compound")
					row.TyreLaps = getInt(s, "TotalLaps")
				}
			}
		}
		st.Standings = append(st.Standings, row)
	}
	st.SectorSegments = segCounts
	inferMissingPositions(st.Standings)
	sort.Slice(st.Standings, func(i, j int) bool {
		a, b := st.Standings[i], st.Standings[j]
		if a.Position != b.Position {
			if a.Position == 0 {
				return false
			}
			if b.Position == 0 {
				return true
			}
			return a.Position < b.Position
		}
		return a.Number < b.Number
	})
	return st
}

// inferMissingPositions fills Position for rows that lack one (a replay
// started mid-session only sees position *changes*) by slotting them into
// the unused position numbers ordered by gap to the leader.
func inferMissingPositions(rows []model.Standing) {
	used := map[int]bool{}
	var unknown []int
	for i := range rows {
		if rows[i].Position > 0 {
			used[rows[i].Position] = true
		} else if rows[i].GapToLeader != "" {
			unknown = append(unknown, i)
		}
	}
	if len(unknown) == 0 {
		return
	}
	sort.SliceStable(unknown, func(a, b int) bool {
		return gapSeconds(rows[unknown[a]].GapToLeader) < gapSeconds(rows[unknown[b]].GapToLeader)
	})
	var free []int
	for p := 1; p <= len(rows); p++ {
		if !used[p] {
			free = append(free, p)
		}
	}
	for i, idx := range unknown {
		if i < len(free) {
			rows[idx].Position = free[i]
		}
	}
}

// gapSeconds orders gap-to-leader strings: the leader ("LAP n" or empty
// interval marker) first, then time gaps, then lapped cars.
func gapSeconds(v string) float64 {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "LAP") {
		return 0
	}
	if strings.HasSuffix(v, "L") {
		var laps float64
		fmt.Sscanf(v, "%f", &laps)
		return 1e6 * laps
	}
	f, err := strconv.ParseFloat(strings.TrimPrefix(v, "+"), 64)
	if err != nil {
		return 1e9
	}
	return f
}
