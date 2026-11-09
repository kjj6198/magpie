package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

// museHome is a temp home with nothing signed in, Muse's own variable
// cleared.
func museHome(t *testing.T) string {
	t.Helper()
	isolate(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("MUSE_AUTH_PATH", "")
	return home
}

// writeMuseAuth writes auth.json as `muse login` leaves it (Muse Code
// 1.4, a device-code sign-in), with key as its api_key ("" for none).
func writeMuseAuth(t *testing.T, path, key, base string) {
	t.Helper()
	meta := map[string]any{
		"access_token": "dca:not-for-the-model-api", "api_base_url": base, "mechanism": "oauth",
		"obtained_via": "device_code", "subs_active": true, "user_email": "me@example.com", "user_full_name": "Me",
	}
	if key != "" {
		meta["api_key"] = key
	}
	writeFile(t, path, map[string]any{"schema_version": 1, "providers": map[string]any{"meta": meta}})
}

func museListed(t *testing.T) (Provider, bool) {
	t.Helper()
	return find(Accounts(), MuseID)
}

func TestMuseReadsTheCLIsSignIn(t *testing.T) {
	for _, tt := range []struct {
		name string
		at   func(home string) string
	}{
		{"MUSE_AUTH_PATH", func(home string) string {
			p := filepath.Join(home, "elsewhere", "muse-auth.json")
			t.Setenv("MUSE_AUTH_PATH", p)
			return p
		}},
		{"XDG_CONFIG_HOME", func(home string) string {
			cfg := filepath.Join(home, "xdg")
			t.Setenv("XDG_CONFIG_HOME", cfg)
			return filepath.Join(cfg, "muse", "auth.json")
		}},
		{"home", func(home string) string {
			t.Setenv("XDG_CONFIG_HOME", "")
			return filepath.Join(home, ".config", "muse", "auth.json")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := museHome(t)
			writeMuseAuth(t, tt.at(home), "LLM|key-1", "https://api.meta.ai/v1")
			p, ok := museListed(t)
			if !ok {
				t.Fatalf("no %s among %v", MuseID, Accounts())
			}
			if p.Account.User != "me@example.com" || p.Responses != "https://api.meta.ai/v1" || p.Name != "Muse Code" {
				t.Errorf("listed as %+v at %s", p.Account, p.Responses)
			}
			req, _ := http.NewRequest(http.MethodPost, p.Responses+"/responses", nil)
			if err := p.Account.sign(context.Background(), req, nil); err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer LLM|key-1" {
				t.Errorf("signed with %q, not the api_key", got)
			}
		})
	}
}

// On macOS Muse may keep its key in the keychain: auth.json without an
// api_key is no account to serve, and nothing fails.
func TestMuseWithoutAKeyIsSignedOut(t *testing.T) {
	home := museHome(t)
	path := filepath.Join(home, ".config", "muse", "auth.json")
	if _, ok := museListed(t); ok {
		t.Fatal("listed with no auth.json")
	}
	writeMuseAuth(t, path, "", "https://api.meta.ai/v1")
	if p, ok := museListed(t); ok {
		t.Fatalf("listed with no api_key: %+v", p.Account)
	}
	if ls := Logins(MuseID); len(ls) != 0 {
		t.Errorf("accounts %v", ls)
	}
}

// A sign-in Muse rewrote is read again; one it is halfway through writing
// leaves the last good one in use.
func TestMuseSignInFollowsTheFile(t *testing.T) {
	home := museHome(t)
	path := filepath.Join(home, ".config", "muse", "auth.json")
	writeMuseAuth(t, path, "LLM|key-1", "https://api.meta.ai/v1")
	if c, ok := museSignIn(); !ok || c.Key != "LLM|key-1" {
		t.Fatalf("read %+v %v", c, ok)
	}
	writeMuseAuth(t, path, "LLM|key-2-longer", "https://api.meta.ai/v1")
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(path, later, later)
	if c, _ := museSignIn(); c.Key != "LLM|key-2-longer" {
		t.Fatalf("still %q after Muse signed in again", c.Key)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"providers":{"me`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, ok := museSignIn(); !ok || c.Key != "LLM|key-2-longer" {
		t.Fatalf("a half-written file read as %+v %v", c, ok)
	}
}

// Removing the account hides it and leaves Muse's file as Muse wrote it;
// a fresh `muse login` brings it back.
func TestMuseRemoveLeavesMusesFile(t *testing.T) {
	home := museHome(t)
	path := filepath.Join(home, ".config", "muse", "auth.json")
	writeMuseAuth(t, path, "LLM|key-1", "https://api.meta.ai/v1")
	before, _ := os.ReadFile(path)
	if _, ok := museListed(t); !ok {
		t.Fatal("not listed")
	}
	if err := ForgetLogin(MuseID, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatalf("auth.json changed: %s (%v)", after, err)
	}
	if _, ok := museListed(t); ok {
		t.Fatal("still listed after Remove")
	}
	writeMuseAuth(t, path, "LLM|key-after-login", "https://api.meta.ai/v1")
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(path, later, later)
	if _, ok := museListed(t); !ok {
		t.Fatal("not listed after a new muse login")
	}
}

func TestMuseModelsFromItsList(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "muse_code_models.json"))
	if err != nil {
		t.Fatal(err)
	}
	ms := parseMuseModels(b)
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	want := []string{"muse-wonder-full-1.0-contributor", "muse-wonder-full-1.0", "muse-wonder-1.0-contributor", "muse-wonder-1.0",
		"muse-spark-1.3-contributor", "muse-spark-1.3", "muse-spark-1.2-contributor", "muse-spark-1.2"}
	if !slices.Equal(ids, want) {
		t.Fatalf("models %v, want %v (muse-image-1.0 is hidden)", ids, want)
	}
	names := map[string]bool{}
	for _, m := range ms {
		if names[m.Name] {
			t.Errorf("two models named %q", m.Name)
		}
		names[m.Name] = true
	}
	spark := ms[slices.Index(ids, "muse-spark-1.3")]
	if spark.Context != 1007997 || spark.Output != 128000 || !spark.Images || spark.ImageInput == nil || !*spark.ImageInput || !spark.Reasoning {
		t.Errorf("muse-spark-1.3: %+v", spark)
	}
	if !slices.Equal(spark.Efforts, []string{"minimal", "low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("muse-spark-1.3's efforts %v", spark.Efforts)
	}
	if old := ms[slices.Index(ids, "muse-spark-1.2")]; !slices.Equal(old.Efforts, []string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Errorf("muse-spark-1.2's efforts %v (it has no max)", old.Efforts)
	}
	if len(spark.APIs) != 1 || spark.APIs[0] != string(Responses) {
		t.Errorf("APIs %v", spark.APIs)
	}
}

// The list is asked at /muse-code/models on the API's host, with the
// api_key.
func TestMuseFetchesItsList(t *testing.T) {
	home := museHome(t)
	b, _ := os.ReadFile(filepath.Join("testdata", "muse_code_models.json"))
	var asked, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked, auth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	writeMuseAuth(t, filepath.Join(home, ".config", "muse", "auth.json"), "LLM|key-1", srv.URL+"/v1")
	p, ok := museListed(t)
	if !ok {
		t.Fatal("not listed")
	}
	ms, err := p.Account.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if asked != "/muse-code/models" || auth != "Bearer LLM|key-1" || len(ms) != 8 {
		t.Fatalf("asked %s with %q: %d models", asked, auth, len(ms))
	}
}

func TestMuseRefusalSaysRunMuseLogin(t *testing.T) {
	if got := museExplain(http.StatusUnauthorized, nil); !strings.Contains(got, "muse login") {
		t.Fatalf("401 explained as %q", got)
	}
	if got := museExplain(http.StatusTooManyRequests, nil); got != "" {
		t.Fatalf("429 explained as %q", got)
	}
}

// fakeMuse puts a muse on PATH that prints out for `muse usage --json`,
// and records what it was run with.
func fakeMuse(t *testing.T, out string) (args string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for muse")
	}
	bin := t.TempDir()
	args = filepath.Join(bin, "args")
	if err := os.WriteFile(filepath.Join(bin, "out"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	testenv.Program(t, filepath.Join(bin, "muse"), "#!/bin/sh\necho \"$@\" > '"+args+"'\ncat '"+filepath.Join(bin, "out")+"'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return args
}

func TestMuseUsageFromTheCLI(t *testing.T) {
	museHome(t)
	b, err := os.ReadFile(filepath.Join("testdata", "muse_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	args := fakeMuse(t, string(b))
	q := loginQuota(context.Background(), Login{Agent: MuseID, User: "me@example.com", Active: true, On: true})
	if q.Error != "" {
		t.Fatal(q.Error)
	}
	if ran, _ := os.ReadFile(args); strings.TrimSpace(string(ran)) != "usage --json" {
		t.Errorf("ran muse %q", ran)
	}
	if q.Plan != "Muse Code Power Usage" || len(q.Windows) != 2 {
		t.Fatalf("plan %q, windows %+v", q.Plan, q.Windows)
	}
	five, week := q.Windows[0], q.Windows[1]
	if five.Name != "5 hours" || five.Span != 5*time.Hour || five.Used != 0 || five.ResetsAt == nil || !five.ResetsAt.Equal(time.UnixMilli(1794175835000)) {
		t.Errorf("5 hours: %+v", five)
	}
	if week.Name != "7 days" || week.Span != 7*24*time.Hour || week.Used != 11 || week.ResetsAt == nil || !week.ResetsAt.Equal(time.Unix(1794182400, 0)) {
		t.Errorf("7 days: %+v", week)
	}
}

// Before the first request in a window Muse is told no usage: the card
// says so, with Muse's reason.
func TestMuseUsageNotToldYet(t *testing.T) {
	museHome(t)
	fakeMuse(t, `{"allowance":null,"hint":null,"reason":"server_sent_no_usage","schemaVersion":1}`+"\n")
	q := loginQuota(context.Background(), Login{Agent: MuseID, User: "me@example.com", Active: true, On: true})
	if !strings.Contains(q.Error, "server_sent_no_usage") || len(q.Windows) != 0 {
		t.Fatalf("error %q, windows %+v", q.Error, q.Windows)
	}
}

func TestMuseUsageWithoutTheCLI(t *testing.T) {
	museHome(t)
	t.Setenv("PATH", t.TempDir())
	q := loginQuota(context.Background(), Login{Agent: MuseID, User: "me@example.com", Active: true, On: true})
	if !strings.Contains(q.Error, "muse") || len(q.Windows) != 0 {
		t.Fatalf("error %q, windows %+v", q.Error, q.Windows)
	}
}

// The Usage page has a card for the account, read as LoginUsage reads it.
func TestMuseHasAUsageCard(t *testing.T) {
	home := museHome(t)
	b, _ := os.ReadFile(filepath.Join("testdata", "muse_usage.json"))
	fakeMuse(t, string(b))
	writeMuseAuth(t, filepath.Join(home, ".config", "muse", "auth.json"), "LLM|key-1", "https://api.meta.ai/v1")
	var card *SubscriptionQuota
	for _, q := range fetchSubscriptionUsage(context.Background()) {
		if q.Provider == MuseID {
			card = &q
		}
	}
	if card == nil || card.Name != "Muse Code" || card.User != "me@example.com" || len(card.Windows) != 2 {
		raw, _ := json.Marshal(card)
		t.Fatalf("card %s", raw)
	}
}
