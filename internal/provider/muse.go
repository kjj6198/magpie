package provider

// Muse Code is Meta's coding plan, used through the muse CLI's own
// sign-in: `muse login` leaves a Meta key (api_key) in Muse's auth.json,
// and magpie asks Meta's Responses API with it as Muse does. The file's
// access_token is refused there. magpie never writes the file and signs
// in nowhere itself: Remove hides the account until Muse signs in anew
// (ownMark). Meta's terms keep the key to Muse Code; the add sheet says so
// before the account is used (riskNote in app.js).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/proc"
)

// MuseID is the subscription's id, as a provider and in logins.json; the
// Muse agent itself is "muse".
const MuseID = "meta-muse"

const (
	museDefaultAPI = "https://api.meta.ai/v1"
	// museUsageTimeout bounds `muse usage --json`, which asks Meta and
	// runs no model turn
	museUsageTimeout = 15 * time.Second
)

var museClient = &http.Client{Timeout: 30 * time.Second, CheckRedirect: KeepsCredentials}

var errMuseSignedOut = errors.New("Muse Code is signed out: run `muse login`")

// museCreds is what Muse's auth.json holds of its Meta sign-in.
type museCreds struct {
	Key   string
	Base  string
	Email string
}

// museAuthPath is where Muse keeps its sign-in.
func museAuthPath() string {
	if p := appdir.Getenv("MUSE_AUTH_PATH"); p != "" {
		return p
	}
	cfg := appdir.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "muse", "auth.json")
}

// museRead is the last good read of Muse's auth.json, kept while the file
// is unchanged: every request signs with it.
var museRead struct {
	sync.Mutex
	path string
	mod  time.Time
	size int64
	c    museCreds
}

// museSignIn is Muse's Meta sign-in; false when there is none to use:
// signed out, or the key kept in the macOS keychain, which magpie doesn't
// read.
func museSignIn() (museCreds, bool) {
	path := museAuthPath()
	fi, err := os.Stat(path)
	if err != nil {
		return museCreds{}, false
	}
	museRead.Lock()
	defer museRead.Unlock()
	if museRead.path == path && museRead.mod.Equal(fi.ModTime()) && museRead.size == fi.Size() {
		return museRead.c, museRead.c.Key != ""
	}
	b, err := readAccountFile(path)
	if err == nil {
		var c museCreds
		if c, err = parseMuseAuth(b); err == nil {
			museRead.path, museRead.mod, museRead.size, museRead.c = path, fi.ModTime(), fi.Size(), c
			return c, c.Key != ""
		}
	}
	// half-written by Muse, or unreadable: the last good read stands, and
	// the file is read again next time
	if museRead.path == path {
		return museRead.c, museRead.c.Key != ""
	}
	return museCreds{}, false
}

func parseMuseAuth(b []byte) (museCreds, error) {
	var f struct {
		Providers struct {
			Meta struct {
				Key   string `json:"api_key"`
				Base  string `json:"api_base_url"`
				Email string `json:"user_email"`
			} `json:"meta"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return museCreds{}, err
	}
	m := f.Providers.Meta
	c := museCreds{Key: strings.TrimSpace(m.Key), Base: strings.TrimRight(strings.TrimSpace(m.Base), "/"), Email: strings.TrimSpace(m.Email)}
	if u, err := url.Parse(c.Base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		c.Base = museDefaultAPI
	}
	return c, nil
}

// museModelsURL is where Muse lists its models: /muse-code/models on its
// API's host.
func museModelsURL(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		u, _ = url.Parse(museDefaultAPI)
	}
	return u.Scheme + "://" + u.Host + "/muse-code/models"
}

// museOwnUser is who Muse is signed in to, "" for no one.
func museOwnUser() string {
	c, ok := museSignIn()
	if !ok {
		return ""
	}
	return firstNonEmpty(c.Email, "Meta")
}

// museSide lists Muse's one account: its own sign-in, none of magpie's.
func museSide() []sideLogin {
	return sideLogins(MuseID, museOwnUser(), func(savedLogin) bool { return false })
}

func museLoginList() []Login { return loginsOf(museSide()) }

func switchMuseLogin(user string) error { return switchSideLogin(MuseID, user, museSide()) }

func setMuseLoginOn(user string, on bool) error {
	return setSideLoginOn(MuseID, user, on, museSide())
}

func forgetMuseLogin(user string) error { return forgetSideLogin(MuseID, user, museSide(), nil) }

func museAccount() (Provider, bool) {
	c, ok := museSignIn()
	ls := museLoginList()
	if !ok || len(ls) == 0 {
		return Provider{}, false
	}
	return museProvider(ls[0], c), true
}

func museProvider(l Login, c museCreds) Provider {
	a := &Account{Agent: MuseID, User: l.User, Plan: l.Plan, Stream: true}
	a.sign = func(_ context.Context, req *http.Request, _ []byte) error {
		c, ok := museSignIn()
		if !ok {
			return errMuseSignedOut
		}
		req.Header.Set("Authorization", "Bearer "+c.Key)
		return nil
	}
	a.body = museBody
	a.models = museFallbackModels
	a.fetch = func(ctx context.Context) ([]catalog.Model, error) {
		ms, err := museFetchModels(ctx)
		if err != nil {
			return nil, err
		}
		return ms, catalog.SaveLive(MuseID, c.Base, ms)
	}
	a.explain = museExplain
	return Provider{ID: MuseID, Name: "Muse Code", Icon: "meta-color", Responses: c.Base,
		Website: "https://dev.meta.ai", Account: a}
}

// museBody is a Responses request as Muse sends it: streamed, stored
// nowhere.
func museBody(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil {
		return body
	}
	m["stream"], m["store"] = true, false
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// museFallbackModels are served before Muse's list is first fetched.
func museFallbackModels() []catalog.Model {
	yes := true
	var out []catalog.Model
	for _, id := range []string{"muse-spark-1.3", "muse-wonder-1.0"} {
		out = append(out, catalog.Model{ID: id, Name: id, Provider: "meta", APIs: []string{string(Responses)},
			Context: 1007997, Output: 128000, Images: true, ImageInput: &yes, Reasoning: true,
			Efforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}})
	}
	return out
}

func museFetchModels(ctx context.Context) ([]catalog.Model, error) {
	c, ok := museSignIn()
	if !ok {
		return nil, errMuseSignedOut
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, museModelsURL(c.Base), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	res, err := museClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		msg := "Muse Code models: " + APIError(b, res.Status)
		if more := museExplain(res.StatusCode, b); more != "" {
			msg += " — " + more
		}
		return nil, errors.New(msg)
	}
	ms := parseMuseModels(b)
	if len(ms) == 0 {
		return nil, errors.New("Muse Code lists no models for this account")
	}
	return ms, nil
}

// parseMuseModels reads /muse-code/models: each model's metadata
// ["muse-code"] says what Muse's picker shows, and a hidden one
// (muse-image-1.0) is left out.
func parseMuseModels(b []byte) []catalog.Model {
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				Muse *struct {
					Hidden      bool   `json:"is_hidden"`
					Released    string `json:"release_date"`
					Reasoning   bool   `json:"reasoning"`
					Temperature *bool  `json:"temperature"`
					Modalities  struct {
						Input  []string `json:"input"`
						Output []string `json:"output"`
					} `json:"modalities"`
					Limit struct {
						Context int `json:"context"`
						Output  int `json:"output"`
					} `json:"limit"`
					Variants map[string]json.RawMessage `json:"variants"`
				} `json:"muse-code"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &list) != nil {
		return nil
	}
	var out []catalog.Model
	for _, d := range list.Data {
		mc := d.Meta.Muse
		if d.ID == "" || mc == nil || mc.Hidden {
			continue
		}
		// named by its id: the list's names aren't apart
		// (muse-wonder-full-1.0-contributor is named muse-wonder-1.0-contributor)
		m := catalog.Model{ID: d.ID, Name: d.ID, Provider: "meta", Released: mc.Released, APIs: []string{string(Responses)},
			Context: mc.Limit.Context, Output: mc.Limit.Output, Reasoning: mc.Reasoning, Temperature: mc.Temperature,
			InputModalities: mc.Modalities.Input, OutputModalities: mc.Modalities.Output}
		if mc.Modalities.Input != nil {
			yes := slices.Contains(mc.Modalities.Input, "image")
			m.ImageInput, m.Images = &yes, yes
		}
		m.Efforts = museEfforts(mc.Variants)
		out = append(out, m)
	}
	return out
}

// museEfforts are a model's variants, mildest first.
func museEfforts(variants map[string]json.RawMessage) []string {
	var out []string
	for _, e := range catalog.EffortRank {
		if _, ok := variants[e]; ok {
			out = append(out, e)
		}
	}
	var rest []string
	for e := range variants {
		if !slices.Contains(catalog.EffortRank, e) {
			rest = append(rest, e)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

// museExplain says what the user can do about a refusal of Meta's.
func museExplain(status int, _ []byte) string {
	if status == http.StatusUnauthorized {
		return "Meta turned Muse Code's sign-in away; run `muse login` again"
	}
	return ""
}

// museQuota reads the account's windows from `muse usage --json`, which
// Muse answers without a model turn; Meta has no usage endpoint of its own
// to ask.
func museQuota(ctx context.Context, l Login) SubscriptionQuota {
	q := SubscriptionQuota{Provider: MuseID, Plan: l.Plan, Windows: []QuotaWindow{}}
	exe := proc.FindTool("muse")
	if exe == "" {
		q.Error = "Muse Code's CLI (muse) isn't installed, and its usage is read with `muse usage --json`"
		return q
	}
	ctx, cancel := context.WithTimeout(ctx, museUsageTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := proc.CommandContext(ctx, exe, "usage", "--json")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		q.Error = "muse usage: " + firstNonEmpty(strings.TrimSpace(stderr.String()), err.Error())
		return q
	}
	return museUsage(q, out)
}

// museUsage is q with what `muse usage --json` printed: the plan's tier,
// its five hours and its week.
func museUsage(q SubscriptionQuota, out []byte) SubscriptionQuota {
	type window struct {
		ResetsAtMs  int64    `json:"resetsAtMs"`
		UsedPercent *float64 `json:"usedPercent"`
	}
	var r struct {
		Allowance *struct {
			Tier   string  `json:"tier"`
			Window *window `json:"window"`
			Weekly *window `json:"weekly"`
		} `json:"allowance"`
		Hint   string `json:"hint"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil {
		q.Error = "muse usage printed what magpie can't read: " + err.Error()
		return q
	}
	if r.Allowance == nil {
		// before the first request in a window Muse is told nothing
		// (server_sent_no_usage)
		q.Error = "Muse Code told no usage yet"
		if r.Reason != "" {
			q.Error += " (" + r.Reason + ")"
		}
		q.Error += "; it does once a request has been made"
		if r.Hint != "" {
			q.Error += ": " + r.Hint
		}
		return q
	}
	if r.Allowance.Tier != "" {
		q.Plan = r.Allowance.Tier
	}
	for _, w := range []struct {
		name string
		span time.Duration
		w    *window
	}{{"5 hours", 5 * time.Hour, r.Allowance.Window}, {"7 days", 7 * 24 * time.Hour, r.Allowance.Weekly}} {
		if w.w == nil || w.w.UsedPercent == nil {
			continue
		}
		qw := QuotaWindow{Name: w.name, Used: *w.w.UsedPercent, Span: w.span}
		if w.w.ResetsAtMs > 0 {
			t := time.UnixMilli(w.w.ResetsAtMs)
			qw.ResetsAt = &t
		}
		q.Windows = append(q.Windows, qw)
	}
	if len(q.Windows) == 0 {
		q.Error = fmt.Sprintf("Muse Code's %s plan told no window", firstNonEmpty(q.Plan, "Muse Code"))
	}
	return q
}
