package gui

import (
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// A Muse Code account says it is from Muse Code's sign-in, with Meta's
// logo, though no agent has its id (the Muse agent is "muse").
func TestMuseAccountLabel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	p := provider.Provider{ID: provider.MuseID, Name: "Muse Code", Account: &provider.Account{Agent: provider.MuseID, User: "me@example.com"}}
	if name, icon := accountLabel(p); name != "Muse Code" || icon != "meta-color" {
		t.Fatalf("labelled %q %q", name, icon)
	}
}
