package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// acPower reports whether the machine is on wall power, so a multi-GiB
// pull never starts on battery. It scans sysfs the same way upower does:
//
//   - any Mains/UPS supply with online=1 means AC;
//   - otherwise a battery that is charging or full also means AC;
//   - a machine with no readable supplies (desktop, VM) counts as AC.
//
// The returned string is a warning for the "could not read sysfs at all"
// case, where the verdict defaulted to AC.
func acPower(root string) (bool, string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return true, fmt.Sprintf("cannot read %s: %v", root, err)
	}
	sawMains, mainsOnline := false, false
	sawBattery, batteryCharging := false, false
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		kind, err := readTrimmed(filepath.Join(dir, "type"))
		if err != nil {
			continue
		}
		switch kind {
		case "Mains", "UPS":
			sawMains = true
			if v, err := readTrimmed(filepath.Join(dir, "online")); err == nil && v == "1" {
				mainsOnline = true
			}
		case "Battery":
			sawBattery = true
			switch v, _ := readTrimmed(filepath.Join(dir, "status")); v {
			case "Charging", "Full", "Not charging":
				batteryCharging = true
			}
		}
	}
	switch {
	case mainsOnline:
		return true, ""
	case sawMains || sawBattery:
		return batteryCharging, ""
	default:
		return true, "no power supply entries under " + root
	}
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
