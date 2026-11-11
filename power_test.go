package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSupply creates a fake sysfs power_supply entry, e.g.
// writeSupply(t, dir, "Mains", map[string]string{"type": "Mains", "online": "1"}).
func writeSupply(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	d := filepath.Join(root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range files {
		if err := os.WriteFile(filepath.Join(d, k), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAcPower(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		ac    bool
	}{
		{
			"mains online (this laptop plugged in)",
			func(t *testing.T, root string) {
				writeSupply(t, root, "Mains", map[string]string{"type": "Mains", "online": "1"})
				writeSupply(t, root, "ADP0", map[string]string{"type": "Mains", "online": "1"})
			},
			true,
		},
		{
			"mains offline, battery discharging",
			func(t *testing.T, root string) {
				writeSupply(t, root, "Mains", map[string]string{"type": "Mains", "online": "0"})
				writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Discharging"})
			},
			false,
		},
		{
			"mains offline, battery full",
			func(t *testing.T, root string) {
				writeSupply(t, root, "Mains", map[string]string{"type": "Mains", "online": "0"})
				writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Full"})
			},
			true,
		},
		{
			"battery only, discharging",
			func(t *testing.T, root string) {
				writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Discharging"})
			},
			false,
		},
		{
			"battery only, charging",
			func(t *testing.T, root string) {
				writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Charging"})
			},
			true,
		},
		{
			"no entries (desktop or VM) counts as AC",
			func(t *testing.T, root string) {},
			true,
		},
		{
			"entry without a type file is ignored",
			func(t *testing.T, root string) {
				writeSupply(t, root, "BAT0", map[string]string{"status": "Discharging"})
			},
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			c.setup(t, root)
			ac, warn := acPower(root)
			if ac != c.ac {
				t.Errorf("acPower = %v, want %v (warn %q)", ac, c.ac, warn)
			}
		})
	}
}

func TestAcPowerUnreadableRootDefaultsToAC(t *testing.T) {
	ac, warn := acPower(filepath.Join(t.TempDir(), "does-not-exist"))
	if !ac {
		t.Errorf("acPower = %v on unreadable root, want true", ac)
	}
	if warn == "" {
		t.Error("expected a warning for the unreadable root")
	}
}
