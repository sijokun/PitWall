package livetiming

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"f1telemetry/certs"
)

// RawEvent is one decoded feed message as it came off the SignalR socket,
// before any merge/snapshot logic. It's the ground truth for debugging: if
// the data is here but the app shows nothing, the bug is downstream in
// merge/Snapshot, not in the connection.
type RawEvent struct {
	Kind      string    // "negotiate", "snapshot" (R reply), "feed", "other"
	Topic     string    // for feed messages
	Payload   any       // decoded JSON (decompressed for .z topics)
	Raw       []byte    // the original frame bytes
	Timestamp time.Time // when received (wall clock)
}

// DumpRaw connects to the live timing feed exactly like the real client
// (same /signalrcore negotiate, handshake and Subscribe) and invokes fn for
// every record it receives, until ctx is cancelled. It never merges state —
// it's a pure wire tap for debugging. Returns the connection error, if any.
// It subscribes to the app's default topics; use DumpRawTopics for a custom set.
func DumpRaw(ctx context.Context, fn func(RawEvent)) error {
	return DumpRawTopics(ctx, topics, fn)
}

// DumpRawTopics is DumpRaw with an explicit channel subscription list.
func DumpRawTopics(ctx context.Context, subTopics []string, fn func(RawEvent)) error {
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
	fn(RawEvent{Kind: "negotiate", Payload: fmt.Sprintf("HTTP %d", resp.StatusCode), Raw: body, Timestamp: time.Now()})
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

	var writeMu sync.Mutex
	send := func(v any) error {
		p, _ := json.Marshal(v)
		p = append(p, recordSep...)
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, p)
	}
	if err := send(map[string]any{"protocol": "json", "version": 1}); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if err := send(map[string]any{
		"arguments": []any{subTopics}, "invocationId": "1", "target": "Subscribe", "type": msgInvocation,
	}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
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

	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ws read: %w", err)
		}
		now := time.Now()
		for _, rec := range strings.Split(string(data), recordSep) {
			if len(rec) < 3 {
				continue
			}
			var frame struct {
				Type      int               `json:"type"`
				Target    string            `json:"target"`
				Arguments []json.RawMessage `json:"arguments"`
				Result    map[string]any    `json:"result"`
			}
			if json.Unmarshal([]byte(rec), &frame) != nil {
				fn(RawEvent{Kind: "other", Raw: []byte(rec), Timestamp: now})
				continue
			}
			switch frame.Type {
			case msgInvocation:
				if frame.Target != "feed" || len(frame.Arguments) < 2 {
					fn(RawEvent{Kind: "other", Raw: []byte(rec), Timestamp: now})
					continue
				}
				var topic string
				var patch any
				if json.Unmarshal(frame.Arguments[0], &topic) != nil || json.Unmarshal(frame.Arguments[1], &patch) != nil {
					continue
				}
				fn(RawEvent{Kind: "feed", Topic: topic, Payload: decodeIfCompressed(topic, patch), Raw: []byte(rec), Timestamp: now})
			case msgCompletion:
				for topic, v := range frame.Result {
					fn(RawEvent{Kind: "snapshot", Topic: topic, Payload: decodeIfCompressed(topic, v), Raw: []byte(rec), Timestamp: now})
				}
			default:
				fn(RawEvent{Kind: "other", Raw: []byte(rec), Timestamp: now})
			}
		}
	}
}

// decodeIfCompressed decompresses ".z" topic payloads so the dumper shows
// their real JSON; non-compressed payloads pass through unchanged.
func decodeIfCompressed(topic string, v any) any {
	if !strings.HasSuffix(topic, ".z") {
		return v
	}
	enc, ok := v.(string)
	if !ok {
		return v
	}
	decoded, err := decompress(enc)
	if err != nil {
		return fmt.Sprintf("<decompress failed: %v>", err)
	}
	var payload any
	if json.Unmarshal(decoded, &payload) != nil {
		return string(decoded)
	}
	return payload
}
