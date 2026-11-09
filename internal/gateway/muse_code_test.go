package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// museTail is the end of a real Muse Code reply: response.completed (cut
// short where it was saved) and the response.subscription_usage event.
func museTail(t *testing.T) (tail, usage []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "muse_stream_tail.txt"))
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte("event: "+museUsageEvent))
	if i < 0 {
		t.Fatal("no usage event in the saved tail")
	}
	return b, b[i:]
}

// heardWindows stands in for noteCodexLimits, keeping what it was told.
func heardWindows(t *testing.T) func() map[string][]provider.CodexLimit {
	var mu sync.Mutex
	heard := map[string][]provider.CodexLimit{}
	old := noteCodexLimits
	noteCodexLimits = func(agent, user string, ls []provider.CodexLimit) {
		mu.Lock()
		defer mu.Unlock()
		heard[agent+"/"+user] = append(heard[agent+"/"+user], ls...)
	}
	t.Cleanup(func() { noteCodexLimits = old })
	return func() map[string][]provider.CodexLimit {
		mu.Lock()
		defer mu.Unlock()
		out := map[string][]provider.CodexLimit{}
		for k, v := range heard {
			out[k] = append([]provider.CodexLimit(nil), v...)
		}
		return out
	}
}

func wantMuseWindows(t *testing.T, got []provider.CodexLimit) {
	t.Helper()
	want := []provider.CodexLimit{
		{Used: 0, Span: 5 * time.Hour, Resets: time.Unix(1794175835, 0)},
		{Used: 11, Span: 7 * 24 * time.Hour, Resets: time.Unix(1794182400, 0)},
	}
	if len(got) != len(want) {
		t.Fatalf("heard %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Used != want[i].Used || got[i].Span != want[i].Span || !got[i].Resets.Equal(want[i].Resets) {
			t.Errorf("window %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A Muse Code account is asked on Meta's Responses API with the CLI's
// api_key, streamed and stored nowhere; the usage its reply ends with is
// the account's, and the reply reaches the agent as it came.
func TestMuseCodeRequests(t *testing.T) {
	noRests()
	t.Cleanup(noRests)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("MUSE_AUTH_PATH", "")

	_, usage := museTail(t)
	reply := sse(
		`event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"r1","model":"muse-spark-1.3"}}`,
		`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"pong"}`,
		`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":7,"output_tokens":1}}}`) + string(usage)
	var mu sync.Mutex
	var got []byte
	var path, auth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r) // the model list, asked on the side
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, path, auth = b, r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, reply)
	}))
	t.Cleanup(up.Close)

	authFile := filepath.Join(home, ".config", "muse", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authFile), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"schema_version": 1, "providers": map[string]any{"meta": map[string]any{
		"api_key": "LLM|test-key", "access_token": "dca:not-this", "api_base_url": up.URL + "/v1",
		"mechanism": "oauth", "obtained_via": "device_code", "subs_active": true, "user_email": "me@example.com",
	}}})
	if err := os.WriteFile(authFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct{ name, path, body string }{
		{"responses", "/v1/responses", `{"model":"meta-muse/muse-spark-1.3","stream":true,"store":true,"input":"ping"}`},
		{"chat", "/v1/chat/completions", `{"model":"meta-muse/muse-spark-1.3","messages":[{"role":"user","content":"ping"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			heard := heardWindows(t)
			code, out := post(t, tt.path, tt.body)
			if code != 200 || !strings.Contains(out, "pong") {
				t.Fatalf("%d %s", code, out)
			}
			mu.Lock()
			defer mu.Unlock()
			if path != "/v1/responses" {
				t.Fatalf("went to %s", path)
			}
			if auth != "Bearer LLM|test-key" {
				t.Errorf("Authorization %q", auth)
			}
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("upstream %s", got)
			}
			if m["model"] != "muse-spark-1.3" || m["store"] != false || m["stream"] != true {
				t.Errorf("body %s", got)
			}
			if tt.name == "responses" && !strings.Contains(out, string(usage)) {
				t.Errorf("the reply's tail didn't reach the agent as it came:\n%s", out)
			}
			wantMuseWindows(t, heard()[provider.MuseID+"/me@example.com"])
		})
	}
}

// The tap holds one short line at a time: a response.completed longer than
// it can hold is passed over, the event after it still read, and every
// byte passed on as it came.
func TestMuseLimitsTapAfterALongLine(t *testing.T) {
	tail, _ := museTail(t)
	long := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":\"" + strings.Repeat("x", 3*museLineMax) + "\"}}\n\n"
	in := append([]byte(long), tail...)
	var heard []provider.CodexLimit
	tap := &museLimitsTap{ReadCloser: io.NopCloser(iotest.HalfReader(bytes.NewReader(in))), note: func(ls []provider.CodexLimit) {
		heard = append(heard, ls...)
	}}
	out, err := io.ReadAll(tap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Fatal("the reply changed on its way through")
	}
	if cap(tap.line) > 2*museLineMax {
		t.Errorf("held %d bytes", cap(tap.line))
	}
	wantMuseWindows(t, heard)
}
