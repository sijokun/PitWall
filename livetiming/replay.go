package livetiming

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ReplayFile feeds a recorded live timing session (the FastF1
// `python -m fastf1.livetiming save` format — one Python-literal
// ['Topic', {payload}, 'timestamp'] per line) through the client's normal
// merge pipeline all at once. Returns the number of applied updates.
func (c *Client) ReplayFile(path string) (int, error) {
	return c.ReplayFileTimed(context.Background(), path, 0)
}

// ReplayFileTimed replays the file paced by its own timestamps, sped up by
// the given factor (e.g. 60 = one recorded minute per second); 0 or less
// applies everything immediately.
func (c *Client) ReplayFileTimed(ctx context.Context, path string, speed float64) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	applied := 0
	var recBase time.Time
	var wallBase time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		jsonLine, err := pyLiteralToJSON(line)
		if err != nil {
			continue
		}
		var entry []any
		if json.Unmarshal([]byte(jsonLine), &entry) != nil || len(entry) < 2 {
			continue
		}
		topic, ok := entry[0].(string)
		if !ok {
			continue
		}
		// The recorded payload of ".z" topics is the raw base64 string, so
		// applyTopic handles them the same way as the live feed. ApplyFeed
		// takes the lock for us.
		if speed > 0 && len(entry) >= 3 {
			if tsStr, ok := entry[2].(string); ok {
				if ts, err := time.Parse(time.RFC3339, tsStr); err == nil {
					if recBase.IsZero() {
						recBase, wallBase = ts, time.Now()
					}
					target := wallBase.Add(time.Duration(float64(ts.Sub(recBase)) / speed))
					if d := time.Until(target); d > 0 {
						select {
						case <-time.After(d):
						case <-ctx.Done():
							return applied, ctx.Err()
						}
					}
				}
			}
		}
		c.ApplyFeed(topic, entry[1])
		applied++
	}
	if applied == 0 {
		return 0, fmt.Errorf("no parseable feed lines in %s", path)
	}
	return applied, sc.Err()
}

// pyLiteralToJSON converts a Python literal (single-quoted strings, True/
// False/None) to JSON. Handles escapes and quotes inside strings.
func pyLiteralToJSON(s string) (string, error) {
	var out strings.Builder
	out.Grow(len(s) + 16)
	i := 0
	for i < len(s) {
		ch := s[i]
		switch {
		case ch == '\'' || ch == '"':
			quote := ch
			i++
			out.WriteByte('"')
			for i < len(s) && s[i] != quote {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					next := s[i+1]
					if next == quote {
						// \' inside a single-quoted string → plain char
						if quote == '\'' {
							out.WriteByte('\'')
						} else {
							out.WriteString(`\"`)
						}
					} else {
						out.WriteByte('\\')
						out.WriteByte(next)
					}
					i += 2
					continue
				}
				if c == '"' {
					out.WriteString(`\"`)
				} else {
					out.WriteByte(c)
				}
				i++
			}
			if i >= len(s) {
				return "", fmt.Errorf("unterminated string")
			}
			i++ // closing quote
			out.WriteByte('"')
		case strings.HasPrefix(s[i:], "True"):
			out.WriteString("true")
			i += 4
		case strings.HasPrefix(s[i:], "False"):
			out.WriteString("false")
			i += 5
		case strings.HasPrefix(s[i:], "None"):
			out.WriteString("null")
			i += 4
		default:
			out.WriteByte(ch)
			i++
		}
	}
	return out.String(), nil
}
