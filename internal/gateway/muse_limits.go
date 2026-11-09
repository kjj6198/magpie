package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// Meta tells what a Muse Code account has used at the end of each streamed
// reply, after response.completed:
//
//	event: response.subscription_usage
//	data: {"subscription":{"tier":…,"weekly":{"resets_at":…,"used_percent":11},
//	       "window":{"resets_at":…,"used_percent":0,"window_duration_mins":300}},
//	       "type":"response.subscription_usage"}
//
// It is kept as the account's usage, as a Codex reply's rate limits are
// (noteCodexLimits), so routing and the Usage page know it without
// running `muse usage` again.

const museUsageEvent = "response.subscription_usage"

// museLineMax is the longest line the tap holds while it looks for the
// event, which is a short one; a longer line (a whole response.completed)
// is passed over.
const museLineMax = 64 << 10

// heardMuseLimits keeps what res says of p's Muse Code account.
func heardMuseLimits(p provider.Provider, res *http.Response) {
	if p.Account == nil || p.Account.Agent != provider.MuseID || res == nil || res.Body == nil || res.StatusCode >= 300 {
		return
	}
	agent, user := p.Account.UsageAgent(), p.Account.User
	res.Body = &museLimitsTap{ReadCloser: res.Body, note: func(ls []provider.CodexLimit) {
		noteCodexLimits(agent, user, ls)
	}}
}

// museEventLimits are the windows a response.subscription_usage event's
// data tells, nil for any other event. The week's length isn't said.
func museEventLimits(data []byte) []provider.CodexLimit {
	type window struct {
		ResetsAt int64    `json:"resets_at"`
		Used     *float64 `json:"used_percent"`
	}
	var ev struct {
		Type         string `json:"type"`
		Subscription *struct {
			Window *window `json:"window"`
			Weekly *window `json:"weekly"`
		} `json:"subscription"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Type != museUsageEvent || ev.Subscription == nil {
		return nil
	}
	var out []provider.CodexLimit
	for _, w := range []struct {
		w    *window
		span time.Duration
	}{{ev.Subscription.Window, 5 * time.Hour}, {ev.Subscription.Weekly, 7 * 24 * time.Hour}} {
		if w.w == nil || w.w.Used == nil {
			continue
		}
		l := provider.CodexLimit{Used: *w.w.Used, Span: w.span}
		if w.w.ResetsAt > 0 {
			l.Resets = time.Unix(w.w.ResetsAt, 0)
		}
		out = append(out, l)
	}
	return out
}

// museLimitsTap passes a reply on as it comes, reading each of its lines
// for the event.
type museLimitsTap struct {
	io.ReadCloser
	note func([]provider.CodexLimit)
	line []byte
	long bool // the line under way is past museLineMax
}

func (t *museLimitsTap) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.look(p[:n])
	return n, err
}

func (t *museLimitsTap) look(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		part := b
		if i >= 0 {
			part = b[:i]
		}
		if !t.long {
			if len(t.line)+len(part) > museLineMax {
				t.long, t.line = true, t.line[:0]
			} else {
				t.line = append(t.line, part...)
			}
		}
		if i < 0 {
			return
		}
		if !t.long {
			t.heard(t.line)
		}
		t.line, t.long = t.line[:0], false
		b = b[i+1:]
	}
}

func (t *museLimitsTap) heard(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"`+museUsageEvent+`"`)) {
		return
	}
	if ls := museEventLimits(bytes.TrimSpace(data)); len(ls) > 0 {
		t.note(ls)
	}
}
