package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseJsEvent(t *testing.T) {
	tests := []struct {
		name    string
		buf     []byte
		want    JsEvent
		wantErr bool
	}{
		{
			name: "valid button press",
			buf: func() []byte {
				b := make([]byte, 8)
				binary.LittleEndian.PutUint32(b[0:4], 12345)
				binary.LittleEndian.PutUint16(b[4:6], 1)
				b[6] = jsEventButton
				b[7] = 0
				return b
			}(),
			want: JsEvent{Time: 12345, Value: 1, Type: jsEventButton, Number: 0},
		},
		{
			name: "negative axis value",
			buf: func() []byte {
				b := make([]byte, 8)
				binary.LittleEndian.PutUint32(b[0:4], 100)
				v := int16(-20000)
				binary.LittleEndian.PutUint16(b[4:6], uint16(v)) //nolint:gosec // G115: intentional test for negative value encoding
				b[6] = jsEventAxis
				b[7] = 1
				return b
			}(),
			want: JsEvent{Time: 100, Value: -20000, Type: jsEventAxis, Number: 1},
		},
		{
			name: "max int16 value",
			buf: func() []byte {
				b := make([]byte, 8)
				binary.LittleEndian.PutUint16(b[4:6], uint16(int16(32767)))
				b[6] = jsEventAxis
				return b
			}(),
			want: JsEvent{Value: 32767, Type: jsEventAxis},
		},
		{
			name: "min int16 value",
			buf: func() []byte {
				b := make([]byte, 8)
				v := int16(-32768)
				binary.LittleEndian.PutUint16(b[4:6], uint16(v)) //nolint:gosec // G115: intentional test for min int16 encoding
				b[6] = jsEventAxis
				return b
			}(),
			want: JsEvent{Value: -32768, Type: jsEventAxis},
		},
		{
			name:    "wrong length short",
			buf:     make([]byte, 4),
			wantErr: true,
		},
		{
			name:    "wrong length long",
			buf:     make([]byte, 16),
			wantErr: true,
		},
		{
			name:    "empty buffer",
			buf:     []byte{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJsEvent(tt.buf)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseJsEvent() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseJsEvent() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestIsSignificantEvent(t *testing.T) {
	tests := []struct {
		name     string
		ev       JsEvent
		deadzone int16
		want     bool
	}{
		{
			name:     "button press always significant",
			ev:       JsEvent{Type: jsEventButton, Value: 1},
			deadzone: 6000,
			want:     true,
		},
		{
			name:     "button release always significant",
			ev:       JsEvent{Type: jsEventButton, Value: 0},
			deadzone: 6000,
			want:     true,
		},
		{
			name:     "axis above deadzone",
			ev:       JsEvent{Type: jsEventAxis, Value: 10000},
			deadzone: 6000,
			want:     true,
		},
		{
			name:     "axis below deadzone",
			ev:       JsEvent{Type: jsEventAxis, Value: 3000},
			deadzone: 6000,
			want:     false,
		},
		{
			name:     "axis at deadzone",
			ev:       JsEvent{Type: jsEventAxis, Value: 6000},
			deadzone: 6000,
			want:     true,
		},
		{
			name:     "negative axis above deadzone",
			ev:       JsEvent{Type: jsEventAxis, Value: -10000},
			deadzone: 6000,
			want:     true,
		},
		{
			name:     "negative axis below deadzone",
			ev:       JsEvent{Type: jsEventAxis, Value: -3000},
			deadzone: 6000,
			want:     false,
		},
		{
			name:     "init button ignored",
			ev:       JsEvent{Type: jsEventButton | jsEventInit, Value: 1},
			deadzone: 6000,
			want:     false,
		},
		{
			name:     "init axis ignored",
			ev:       JsEvent{Type: jsEventAxis | jsEventInit, Value: 32767},
			deadzone: 0,
			want:     false,
		},
		{
			name:     "unknown type ignored",
			ev:       JsEvent{Type: 0x04, Value: 1},
			deadzone: 0,
			want:     false,
		},
		{
			name:     "zero deadzone all axis significant",
			ev:       JsEvent{Type: jsEventAxis, Value: 1},
			deadzone: 0,
			want:     true,
		},
		{
			name:     "zero value axis not significant",
			ev:       JsEvent{Type: jsEventAxis, Value: 0},
			deadzone: 1,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSignificantEvent(tt.ev, tt.deadzone)
			if got != tt.want {
				t.Errorf("isSignificantEvent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseInputDevicesFromReader(t *testing.T) {
	procDevices := `I: Bus=0005 Vendor=054c Product=0268 Version=0000
N: Name="Sony PLAYSTATION(R)3 Controller"
P: Phys=
S: Sysfs=/devices/virtual/input/input42
U: Uniq=AA:BB:CC:DD:EE:FF
H: Handlers=event5 js0
B: PROP=0

I: Bus=0003 Vendor=046d Product=c077 Version=0111
N: Name="Logitech USB Optical Mouse"
P: Phys=usb-0000:00:14.0-1/input0
S: Sysfs=/devices/pci0000:00/input/input1
U: Uniq=
H: Handlers=mouse0 event1
B: PROP=0

I: Bus=0005 Vendor=054c Product=09cc Version=0000
N: Name="DualSense Wireless Controller"
P: Phys=
S: Sysfs=/devices/virtual/input/input43
U: Uniq=11:22:33:44:55:66
H: Handlers=event6 js1
B: PROP=0

`

	tests := []struct {
		name      string
		input     string
		names     []string
		wantCount int
		wantNames []string
		wantUniqs []string
	}{
		{
			name:      "match single device",
			input:     procDevices,
			names:     []string{"Sony PLAYSTATION(R)3 Controller"},
			wantCount: 1,
			wantNames: []string{"Sony PLAYSTATION(R)3 Controller"},
			wantUniqs: []string{"AA:BB:CC:DD:EE:FF"},
		},
		{
			name:      "match multiple devices",
			input:     procDevices,
			names:     []string{"Sony PLAYSTATION(R)3 Controller", "DualSense Wireless Controller"},
			wantCount: 2,
			wantUniqs: []string{"AA:BB:CC:DD:EE:FF", "11:22:33:44:55:66"},
		},
		{
			name:      "no match",
			input:     procDevices,
			names:     []string{"Xbox Wireless Controller"},
			wantCount: 0,
		},
		{
			name:      "skip non-js devices",
			input:     procDevices,
			names:     []string{"Logitech USB Optical Mouse"},
			wantCount: 0,
		},
		{
			name:      "empty input",
			input:     "",
			names:     []string{"anything"},
			wantCount: 0,
		},
		{
			name:      "empty names list",
			input:     procDevices,
			names:     []string{},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			devices, err := parseInputDevicesFromReader(strings.NewReader(tt.input), tt.names)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(devices) != tt.wantCount {
				t.Errorf("got %d devices, want %d", len(devices), tt.wantCount)
			}
			for i, wantUniq := range tt.wantUniqs {
				if i < len(devices) && devices[i].Uniq != wantUniq {
					t.Errorf("device[%d].Uniq = %q, want %q", i, devices[i].Uniq, wantUniq)
				}
			}
			for i, wantName := range tt.wantNames {
				if i < len(devices) && devices[i].Name != wantName {
					t.Errorf("device[%d].Name = %q, want %q", i, devices[i].Name, wantName)
				}
			}
		})
	}
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantMaxIdle  int
		wantDeadzone int
		wantNames    []string
	}{
		{
			name: "full config",
			input: `[settings]
maxidle = 1800
deadzone = 8000

[devices]
Sony PLAYSTATION(R)3 Controller
Xbox Wireless Controller`,
			wantMaxIdle:  1800,
			wantDeadzone: 8000,
			wantNames:    []string{"Sony PLAYSTATION(R)3 Controller", "Xbox Wireless Controller"},
		},
		{
			name:         "empty config uses defaults",
			input:        "",
			wantMaxIdle:  3600,
			wantDeadzone: 6000,
			wantNames:    nil,
		},
		{
			name: "settings only",
			input: `[settings]
maxidle = 900`,
			wantMaxIdle:  900,
			wantDeadzone: 6000,
			wantNames:    nil,
		},
		{
			name: "devices only",
			input: `[devices]
My Controller`,
			wantMaxIdle:  3600,
			wantDeadzone: 6000,
			wantNames:    []string{"My Controller"},
		},
		{
			name: "comments and blank lines",
			input: `# Top comment
[settings]
# maxidle comment
maxidle = 2000

[devices]
# This is a comment
Controller One

Controller Two`,
			wantMaxIdle:  2000,
			wantDeadzone: 6000,
			wantNames:    []string{"Controller One", "Controller Two"},
		},
		{
			name: "inline comments in settings",
			input: `[settings]
maxidle = 500   # half the default
deadzone = 3000 # low deadzone`,
			wantMaxIdle:  500,
			wantDeadzone: 3000,
			wantNames:    nil,
		},
		{
			name: "unknown section ignored",
			input: `[future]
newkey = value

[settings]
maxidle = 100`,
			wantMaxIdle:  100,
			wantDeadzone: 6000,
			wantNames:    nil,
		},
		{
			name: "invalid number keeps default",
			input: `[settings]
maxidle = abc
deadzone = 6000`,
			wantMaxIdle:  3600,
			wantDeadzone: 6000,
			wantNames:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseConfig(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.MaxIdle != tt.wantMaxIdle {
				t.Errorf("MaxIdle = %d, want %d", cfg.MaxIdle, tt.wantMaxIdle)
			}
			if cfg.Deadzone != tt.wantDeadzone {
				t.Errorf("Deadzone = %d, want %d", cfg.Deadzone, tt.wantDeadzone)
			}
			if len(cfg.Names) != len(tt.wantNames) {
				t.Errorf("Names count = %d, want %d", len(cfg.Names), len(tt.wantNames))
			} else {
				for i, want := range tt.wantNames {
					if cfg.Names[i] != want {
						t.Errorf("Names[%d] = %q, want %q", i, cfg.Names[i], want)
					}
				}
			}
		})
	}
}

func TestMigrateConfig(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, ".jstimeout.devices")
	newPath := filepath.Join(dir, "config")

	// Write legacy device file
	legacyContent := "Sony PLAYSTATION(R)3 Controller\n# A comment\nXbox Wireless Controller\n"
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0600); err != nil {
		t.Fatal(err)
	}

	// Migrate
	result, err := migrateConfig(legacyPath, newPath)
	if err != nil {
		t.Fatalf("migrateConfig() error: %v", err)
	}
	if result != newPath {
		t.Errorf("returned path = %q, want %q", result, newPath)
	}

	// Verify new config was written and is parseable
	cfg, err := loadConfig(newPath)
	if err != nil {
		t.Fatalf("loadConfig on migrated file: %v", err)
	}
	if cfg.MaxIdle != 3600 {
		t.Errorf("MaxIdle = %d, want 3600", cfg.MaxIdle)
	}
	if cfg.Deadzone != 6000 {
		t.Errorf("Deadzone = %d, want 6000", cfg.Deadzone)
	}
	wantNames := []string{"Sony PLAYSTATION(R)3 Controller", "Xbox Wireless Controller"}
	if len(cfg.Names) != len(wantNames) {
		t.Fatalf("Names count = %d, want %d", len(cfg.Names), len(wantNames))
	}
	for i, w := range wantNames {
		if cfg.Names[i] != w {
			t.Errorf("Names[%d] = %q, want %q", i, cfg.Names[i], w)
		}
	}

	// Verify legacy file renamed to .v1.bak
	bakPath := legacyPath + ".v1.bak"
	if _, err := os.Stat(bakPath); os.IsNotExist(err) {
		t.Error("legacy file should be renamed to .v1.bak")
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Error("original legacy file should no longer exist")
	}

	// Re-running migration is a no-op (new config already exists)
	// Write a new legacy file to test that migrateConfig doesn't overwrite
	if err := os.WriteFile(legacyPath, []byte("New Controller\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The new config already exists, so resolveConfig should find it first
	// migrateConfig itself doesn't check - that's resolveConfig's job
	// Just verify the existing config is unchanged
	cfg2, err := loadConfig(newPath)
	if err != nil {
		t.Fatalf("second loadConfig: %v", err)
	}
	if len(cfg2.Names) != 2 {
		t.Errorf("config should be unchanged, got %d names", len(cfg2.Names))
	}
}

func TestParseConfigNoSections(t *testing.T) {
	// Lines outside any section should be ignored
	input := "Sony PLAYSTATION(R)3 Controller\nXbox Wireless Controller\n"
	cfg, err := parseConfig(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Names) != 0 {
		t.Errorf("parseConfig without sections should not parse device names, got %d", len(cfg.Names))
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "valid defaults",
			cfg:     Config{MaxIdle: 3600, Deadzone: 6000, Names: []string{"test"}},
			wantErr: false,
		},
		{
			name:    "min values",
			cfg:     Config{MaxIdle: 1, Deadzone: 0, Names: []string{"test"}},
			wantErr: false,
		},
		{
			name:    "max values",
			cfg:     Config{MaxIdle: 10800, Deadzone: 32767, Names: []string{"test"}},
			wantErr: false,
		},
		{
			name:    "maxidle too low",
			cfg:     Config{MaxIdle: 0, Deadzone: 6000, Names: []string{"test"}},
			wantErr: true,
		},
		{
			name:    "maxidle too high",
			cfg:     Config{MaxIdle: 10801, Deadzone: 6000, Names: []string{"test"}},
			wantErr: true,
		},
		{
			name:    "deadzone too low",
			cfg:     Config{MaxIdle: 3600, Deadzone: -1, Names: []string{"test"}},
			wantErr: true,
		},
		{
			name:    "deadzone too high",
			cfg:     Config{MaxIdle: 3600, Deadzone: 32768, Names: []string{"test"}},
			wantErr: true,
		},
		{
			name:    "empty names warns but no error",
			cfg:     Config{MaxIdle: 3600, Deadzone: 6000, Names: []string{}},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(&tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResolveConfig(t *testing.T) {
	// Test that explicit --config flag is returned as-is
	path, found := resolveConfig("/some/explicit/path")
	if !found {
		t.Error("explicit config flag should report found=true")
	}
	if path != "/some/explicit/path" {
		t.Errorf("path = %q, want /some/explicit/path", path)
	}

	// Note: resolveConfig("") has side effects (auto-migration, file writes)
	// so we only test the explicit-flag path here. Migration is tested in TestMigrateConfig.
}
