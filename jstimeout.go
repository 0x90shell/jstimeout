// Package main implements jstimeout, a daemon that auto-disconnects idle
// Bluetooth gamepads after a configurable timeout.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	jsEventSize = 8 // sizeof(struct js_event): __u32 + __s16 + __u8 + __u8

	// Event type constants from linux/joystick.h
	jsEventButton = 0x01 // button pressed/released
	jsEventAxis   = 0x02 // joystick moved
	jsEventInit   = 0x80 // OR'd with type for synthetic initial state events
)

var version = "2.0.0"
var verbose bool

// Config holds runtime settings parsed from config file and CLI flags.
type Config struct {
	MaxIdle  int
	Deadzone int
	Names    []string
}

func defaultConfig() Config {
	return Config{MaxIdle: 3600, Deadzone: 6000}
}

// JsEvent represents the Linux js_event struct from /dev/input/jsX.
// Layout: { __u32 time; __s16 value; __u8 type; __u8 number; }
type JsEvent struct {
	Time   uint32
	Value  int16
	Type   uint8
	Number uint8
}

// Device represents a matched input device from /proc/bus/input/devices.
type Device struct {
	Name     string
	Uniq     string
	Handlers []string
}

// --- Color helpers (TTY-aware) ---

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func colorRed(s string) string {
	if isTTY() {
		return "\033[1;31m" + s + "\033[0m"
	}
	return s
}

//nolint:unparam // called with constant now, but designed for general use
func colorGreen(s string) string {
	if isTTY() {
		return "\033[1;32m" + s + "\033[0m"
	}
	return s
}

//nolint:unparam // called with constant now, but designed for general use
func colorYellow(s string) string {
	if isTTY() {
		return "\033[1;33m" + s + "\033[0m"
	}
	return s
}

func colorDim(s string) string {
	if isTTY() {
		return "\033[2m" + s + "\033[0m"
	}
	return s
}

func debugf(format string, args ...any) {
	if verbose {
		fmt.Printf(format+"\n", args...)
	}
}

// --- INI config parser ---

const (
	systemConfigExample = "/usr/share/jstimeout/config.example"
	legacySystemExample = "/usr/share/jstimeout/devices.example"
)

// parseConfig reads an INI-style config file into a Config.
// [settings] section has key=value pairs; [devices] section has one name per line.
func parseConfig(r io.Reader) (Config, error) {
	cfg := defaultConfig()
	scanner := bufio.NewScanner(r)
	section := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Section header
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line[1 : len(line)-1])
			continue
		}

		switch section {
		case "settings":
			// Strip inline comments
			if idx := strings.Index(line, "#"); idx > 0 {
				line = strings.TrimSpace(line[:idx])
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			switch key {
			case "maxidle":
				if n, err := strconv.Atoi(val); err == nil {
					cfg.MaxIdle = n
				}
			case "deadzone":
				if n, err := strconv.Atoi(val); err == nil {
					cfg.Deadzone = n
				}
			default:
				debugf("Warning: unknown config key: %s", key)
			}
		case "devices":
			cfg.Names = append(cfg.Names, line)
		default:
			// Unknown section - skip silently for forward-compat
		}
	}

	if err := scanner.Err(); err != nil {
		return cfg, fmt.Errorf("reading config: %w", err)
	}
	return cfg, nil
}

// loadConfig reads config from the given path.
func loadConfig(path string) (Config, error) {
	file, err := os.Open(path) //nolint:gosec // G304: path from config resolution, not user-controlled
	if err != nil {
		return defaultConfig(), fmt.Errorf("opening config: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only
	return parseConfig(file)
}

// migrateConfig reads a legacy device file, writes a new INI config, and
// renames the legacy file to .v1.bak. Returns the new config path.
func migrateConfig(legacyPath string, newPath string) (string, error) {
	file, err := os.Open(legacyPath) //nolint:gosec // G304: path from known legacy locations
	if err != nil {
		return "", fmt.Errorf("opening legacy file: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only

	var names []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading legacy file: %w", err)
	}

	// Build new config content
	var b strings.Builder
	b.WriteString("# jstimeout configuration (migrated from legacy device file)\n")
	b.WriteString("# CLI flags override these values\n\n")
	b.WriteString("[settings]\n")
	b.WriteString("maxidle = 3600      # idle timeout in seconds (1-10800)\n")
	b.WriteString("deadzone = 6000     # axis deadzone threshold (0-32767)\n\n")
	b.WriteString("[devices]\n")
	for _, name := range names {
		b.WriteString(name + "\n")
	}

	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil { //nolint:gosec // G301: XDG config dir
		return "", fmt.Errorf("creating config dir: %w", err)
	}
	if err := os.WriteFile(newPath, []byte(b.String()), 0644); err != nil { //nolint:gosec // G306: user config, world-readable
		return "", fmt.Errorf("writing config: %w", err)
	}

	bakPath := legacyPath + ".v1.bak"
	if err := os.Rename(legacyPath, bakPath); err != nil {
		fmt.Printf("Warning: could not rename %s to %s: %v\n", legacyPath, bakPath, err)
	}

	fmt.Printf("Migrated %s → %s (old file renamed to %s)\n", legacyPath, newPath, bakPath)
	return newPath, nil
}

// resolveConfig finds the config file. Returns (path, found).
func resolveConfig(configFlag string) (string, bool) {
	// Explicit flag overrides everything
	if configFlag != "" {
		return configFlag, true
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}

	xdgConfig := filepath.Join(home, ".config", "jstimeout", "config")
	etcConfig := "/etc/jstimeout/config"

	// Check new INI config locations
	for _, p := range []string{xdgConfig, etcConfig} {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}

	// Auto-copy system example
	if _, err := os.Stat(systemConfigExample); err == nil {
		if err := os.MkdirAll(filepath.Dir(xdgConfig), 0755); err == nil { //nolint:gosec // G301: XDG config dir
			if src, err := os.ReadFile(systemConfigExample); err == nil {
				if err := os.WriteFile(xdgConfig, src, 0644); err == nil { //nolint:gosec // G306: user config, world-readable
					fmt.Printf("Created default config at %s\n", xdgConfig)
					return xdgConfig, true
				}
			}
		}
	}

	// Legacy auto-migration: find old device file, convert to new config
	legacyPaths := []string{".jstimeout.devices", filepath.Join(home, ".config", "jstimeout", "devices")}
	for _, p := range legacyPaths {
		if _, err := os.Stat(p); err == nil {
			newPath, err := migrateConfig(p, xdgConfig)
			if err != nil {
				fmt.Printf("Warning: migration failed for %s: %v\n", p, err)
				continue
			}
			return newPath, true
		}
	}

	// Legacy auto-copy from system example (for fresh installs with only old-format example)
	if _, err := os.Stat(legacySystemExample); err == nil {
		legacyXDG := filepath.Join(home, ".config", "jstimeout", "devices")
		if err := os.MkdirAll(filepath.Dir(legacyXDG), 0755); err == nil { //nolint:gosec // G301: XDG config dir
			if src, err := os.ReadFile(legacySystemExample); err == nil {
				if err := os.WriteFile(legacyXDG, src, 0644); err == nil { //nolint:gosec // G306: user config
					// Immediately migrate the freshly copied legacy file
					newPath, err := migrateConfig(legacyXDG, xdgConfig)
					if err != nil {
						fmt.Printf("Warning: migration failed: %v\n", err)
						return "", false
					}
					return newPath, true
				}
			}
		}
	}

	return "", false
}

func validateConfig(cfg *Config) error {
	if cfg.MaxIdle < 1 || cfg.MaxIdle > 10800 {
		return fmt.Errorf("maxidle must be between 1 and 10,800 seconds (3 hours), got %d", cfg.MaxIdle)
	}
	if cfg.Deadzone < 0 || cfg.Deadzone > 32767 {
		return fmt.Errorf("deadzone must be between 0 and 32,767, got %d", cfg.Deadzone)
	}
	if len(cfg.Names) == 0 {
		fmt.Println("Warning: no device names configured - daemon will idle with no matches")
	}
	return nil
}

// --- Event parsing ---

// parseJsEvent parses 8 bytes from /dev/input/jsX into a JsEvent.
func parseJsEvent(buf []byte) (JsEvent, error) {
	if len(buf) != jsEventSize {
		return JsEvent{}, fmt.Errorf("expected %d bytes, got %d", jsEventSize, len(buf))
	}
	return JsEvent{
		Time:   binary.LittleEndian.Uint32(buf[0:4]),
		Value:  int16(binary.LittleEndian.Uint16(buf[4:6])), //nolint:gosec // G115: joystick axis value, always fits int16
		Type:   buf[6],
		Number: buf[7],
	}, nil
}

// isSignificantEvent returns true if the event represents genuine user input.
// Init events (type & 0x80) are always ignored.
// Button events always count. Axis events only count if |value| >= deadzone.
func isSignificantEvent(ev JsEvent, deadzone int16) bool {
	if ev.Type&jsEventInit != 0 {
		return false
	}

	switch ev.Type {
	case jsEventButton:
		return true
	case jsEventAxis:
		v := int32(ev.Value)
		if v < 0 {
			v = -v
		}
		return v >= int32(deadzone)
	default:
		return false
	}
}

// --- Device parsing ---

// parseInputDevicesFromReader parses the /proc/bus/input/devices format.
func parseInputDevicesFromReader(r io.Reader, names []string) ([]Device, error) {
	var devices []Device
	var currentDevice Device
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		switch {
		case strings.HasPrefix(line, "N: Name="):
			currentDevice.Name = strings.Trim(line[len("N: Name="):], `"`)
		case strings.HasPrefix(line, "U: Uniq="):
			currentDevice.Uniq = strings.TrimSpace(line[len("U: Uniq="):])
		case strings.HasPrefix(line, "H: Handlers="):
			currentDevice.Handlers = strings.Fields(line[len("H: Handlers="):])
		case line == "" && currentDevice.Name != "":
			for _, handler := range currentDevice.Handlers {
				if strings.HasPrefix(handler, "js") {
					if slices.Contains(names, currentDevice.Name) {
						devices = append(devices, currentDevice)
					}
					break
				}
			}
			currentDevice = Device{}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %w", err)
	}
	return devices, nil
}

func parseInputDevices(names []string) ([]Device, error) {
	file, err := os.Open("/proc/bus/input/devices")
	if err != nil {
		return nil, fmt.Errorf("failed to open devices: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only
	return parseInputDevicesFromReader(file, names)
}

// --- Device monitoring ---

func inputChecker(devPath string, uniq string, deviceEvent chan struct{}, quit chan bool, deadzone int16) {
	debugf("Checking input on device: %s (%s)", uniq, devPath)

	file, err := os.Open(devPath) //nolint:gosec // G304: path from /proc/bus/input, not user-controlled
	if err != nil {
		fmt.Printf("Failed to open device %s: %v\n", uniq, err)
		return
	}
	defer file.Close() //nolint:errcheck // read-only

	buf := make([]byte, jsEventSize)

	for {
		select {
		case <-quit:
			debugf("Stopping input check for device %s", uniq)
			return
		default:
			n, err := file.Read(buf)
			if err != nil {
				fmt.Printf("Error reading event from device %s: %v\n", uniq, err)
				return
			}
			if n != jsEventSize {
				continue
			}

			ev, err := parseJsEvent(buf)
			if err != nil {
				continue
			}

			if isSignificantEvent(ev, deadzone) {
				deviceEvent <- struct{}{}
			}
		}
	}
}

func monitorDevice(devPath string, uniq string, maxIdle time.Duration, wg *sync.WaitGroup, quit chan bool, deadzone int16) {
	defer wg.Done()
	fmt.Printf("Monitoring device: %s (%s)\n", uniq, devPath)

	idleSince := time.Now()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	deviceEvent := make(chan struct{})
	go inputChecker(devPath, uniq, deviceEvent, quit, deadzone)

	for {
		select {
		case <-quit:
			debugf("Stopping monitoring for device %s", uniq)
			return
		case <-deviceEvent:
			idleSince = time.Now()
		case <-ticker.C:
			idleDuration := time.Since(idleSince)
			if idleDuration >= maxIdle {
				fmt.Printf("Device %s idle for %v, disconnecting...\n", uniq, idleDuration)
				disconnectDevice(uniq)
				return
			}
		}
	}
}

func disconnectDevice(uniq string) {
	cmd := exec.Command("bluetoothctl", "disconnect", uniq) //nolint:gosec,noctx // G204: uniq is BT MAC from /proc; no context needed for short-lived disconnect
	err := cmd.Run()
	if err != nil {
		fmt.Printf("Failed to disconnect %s: %v\n", uniq, err)
	} else {
		fmt.Printf("Disconnected device %s\n", uniq)
	}
}

// --- Setup diagnostics ---

func isUserInGroup(groupName string) bool {
	file, err := os.Open("/etc/group")
	if err != nil {
		return false
	}
	defer file.Close() //nolint:errcheck // read-only

	user := os.Getenv("USER")
	if user == "" {
		return false
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, ":")
		if len(parts) >= 4 && parts[0] == groupName {
			members := strings.Split(parts[3], ",")
			return slices.Contains(members, user)
		}
	}
	return false
}

func isGroupActiveInSession(groupName string) bool {
	file, err := os.Open("/etc/group")
	if err != nil {
		return false
	}
	defer file.Close() //nolint:errcheck // read-only

	var targetGID int
	found := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ":")
		if len(parts) >= 3 && parts[0] == groupName {
			if gid, err := strconv.Atoi(parts[2]); err == nil {
				targetGID = gid
				found = true
			}
			break
		}
	}
	if !found {
		return false
	}

	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	return slices.Contains(groups, targetGID)
}

func runSetup(cfg Config, configPath string) {
	fmt.Printf("\njstimeout v%s - diagnostics\n\n", version)
	issues := 0

	// Check js devices
	jsDevices, _ := filepath.Glob("/dev/input/js*")
	if len(jsDevices) == 0 {
		fmt.Printf("%s No /dev/input/js* devices found (no controllers connected)\n", colorDim("[~]"))
	} else {
		anyDenied := false
		for _, dev := range jsDevices {
			f, err := os.Open(dev) //nolint:gosec // G304: glob of /dev/input/js*
			if err != nil {
				if os.IsPermission(err) {
					fmt.Printf("%s %s - permission denied\n", colorRed("[✗]"), dev)
					anyDenied = true
					issues++
				} else {
					fmt.Printf("%s %s - %v\n", colorYellow("[!]"), dev, err)
				}
			} else {
				fmt.Printf("%s %s readable\n", colorGreen("[✓]"), dev)
				f.Close() //nolint:errcheck,gosec // read-only probe
			}
		}

		// Only check group membership if access failed
		if anyDenied {
			if isUserInGroup("input") {
				fmt.Printf("%s User in 'input' group (persistent)\n", colorGreen("[✓]"))
			} else {
				fmt.Printf("%s User not in 'input' group\n", colorRed("[✗]"))
				fmt.Printf("    Run: sudo usermod -aG input $USER && re-login\n")
				issues++
			}
			if isGroupActiveInSession("input") {
				fmt.Printf("%s 'input' group active in session\n", colorGreen("[✓]"))
			} else {
				fmt.Printf("%s 'input' group not active in current session - re-login required\n", colorYellow("[!]"))
			}
		}
	}

	// Check bluetoothctl
	btPath, err := exec.LookPath("bluetoothctl")
	if err != nil {
		fmt.Printf("%s bluetoothctl not found on PATH\n", colorRed("[✗]"))
		fmt.Printf("    Install: sudo pacman -S bluez-utils\n")
		issues++
	} else {
		fmt.Printf("%s bluetoothctl found: %s\n", colorGreen("[✓]"), btPath)
	}

	// Check config
	if configPath != "" {
		fmt.Printf("%s Config: %s\n", colorGreen("[✓]"), configPath)
		fmt.Printf("    maxidle=%d  deadzone=%d  devices=%d\n", cfg.MaxIdle, cfg.Deadzone, len(cfg.Names))
	} else {
		fmt.Printf("%s No config found, using defaults\n", colorDim("[~]"))
	}

	// Check systemd service
	home, _ := os.UserHomeDir()
	servicePaths := []string{
		"/usr/lib/systemd/user/jstimeout.service",
		filepath.Join(home, ".config", "systemd", "user", "jstimeout.service"),
	}
	serviceFound := false
	for _, p := range servicePaths {
		if _, err := os.Stat(p); err == nil {
			fmt.Printf("%s Systemd service: %s\n", colorGreen("[✓]"), p)
			serviceFound = true
			break
		}
	}
	if !serviceFound {
		fmt.Printf("%s No systemd user service found\n", colorDim("[~]"))
	}

	// Check for systemd override
	if home != "" {
		overrideDir := filepath.Join(home, ".config", "systemd", "user", "jstimeout.service.d")
		if _, err := os.Stat(overrideDir); err == nil {
			fmt.Printf("%s Systemd override detected: %s\n", colorYellow("[!]"), overrideDir)
			fmt.Printf("    Check for renamed flags (v2.0.0: --maxidletime → --maxidle)\n")
		}
	}

	fmt.Println()
	if issues > 0 {
		os.Exit(1)
	}
}

// --- CLI ---

func printHelp() {
	fmt.Printf(`jstimeout v%s - auto-disconnect idle Bluetooth gamepads

Usage: jstimeout [options]

Options:
  -m, --maxidle SEC    Idle timeout in seconds (1-10800, default: from config or 3600)
  -z, --deadzone VAL   Axis deadzone threshold (0-32767, default: from config or 6000)
  -c, --config PATH    Path to config file
  -v, --verbose        Enable debug logging
  -V, --version        Print version and exit
      --setup          Run diagnostics (check permissions, config, systemd)
  -h, --help           Print this help

Config file (INI format):
  Lookup order:
    1. --config flag
    2. ~/.config/jstimeout/config
    3. /etc/jstimeout/config
    4. Auto-copy from /usr/share/jstimeout/config.example

  Example:
    [settings]
    maxidle = 1800
    deadzone = 6000

    [devices]
    Sony PLAYSTATION(R)3 Controller
    Xbox Wireless Controller

  CLI flags override config file values.

Legacy device files (.jstimeout.devices, ~/.config/jstimeout/devices) are
automatically migrated to the new config format on first run.
`, version)
}

func main() {
	// Parse CLI args
	var (
		maxIdleFlag  = -1
		deadzoneFlag = -1
		configFlag   string
		setupMode    bool
	)

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--maxidle", "-m":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "Error: --maxidle requires a value")
				os.Exit(1)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: invalid --maxidle value: %s\n", args[i])
				os.Exit(1)
			}
			maxIdleFlag = n
		case "--deadzone", "-z":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "Error: --deadzone requires a value")
				os.Exit(1)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: invalid --deadzone value: %s\n", args[i])
				os.Exit(1)
			}
			deadzoneFlag = n
		case "--config", "-c":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "Error: --config requires a path")
				os.Exit(1)
			}
			i++
			configFlag = args[i]
		case "--verbose", "-v":
			verbose = true
		case "--version", "-V":
			fmt.Printf("jstimeout v%s\n", version)
			os.Exit(0)
		case "--setup":
			setupMode = true
		case "--help", "-h":
			printHelp()
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown flag: %s\nRun 'jstimeout --help' for usage.\n", args[i])
			os.Exit(1)
		}
	}

	// Load config
	var cfg Config
	configPath, found := resolveConfig(configFlag)

	if found {
		var err error
		cfg, err = loadConfig(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config %s: %v\n", configPath, err)
			os.Exit(1)
		}
		debugf("Loaded config: %s", configPath)
	} else {
		cfg = defaultConfig()
		debugf("No config found, using defaults")
	}

	// CLI flags override config
	if maxIdleFlag >= 0 {
		cfg.MaxIdle = maxIdleFlag
	}
	if deadzoneFlag >= 0 {
		cfg.Deadzone = deadzoneFlag
	}

	// Setup mode
	if setupMode {
		runSetup(cfg, configPath)
		return
	}

	// Validate
	if err := validateConfig(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	debugf("Using config: %s", configPath)
	debugf("Loaded device names:")
	for _, name := range cfg.Names {
		debugf("  - %s", name)
	}
	debugf("Max idle time: %ds | Deadzone: %d", cfg.MaxIdle, cfg.Deadzone)

	deadzone := int16(cfg.Deadzone) //nolint:gosec // G115: validated 0-32767 above
	idleDuration := time.Duration(cfg.MaxIdle) * time.Second

	// Always print summary (single line, journalctl-friendly)
	fmt.Printf("Started: %d devices, maxidle=%ds, deadzone=%d\n", len(cfg.Names), cfg.MaxIdle, cfg.Deadzone)

	deviceQuitChannels := make(map[string]chan bool)
	var mu sync.Mutex

	for {
		devices, err := parseInputDevices(cfg.Names)
		if err != nil {
			fmt.Printf("Error parsing devices: %v\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		currentDevices := make(map[string]bool)
		mu.Lock()

		// Handle new devices
		for _, device := range devices {
			if _, exists := deviceQuitChannels[device.Uniq]; !exists {
				for _, handler := range device.Handlers {
					if strings.HasPrefix(handler, "js") {
						quit := make(chan bool)
						deviceQuitChannels[device.Uniq] = quit
						var wg sync.WaitGroup
						wg.Add(1)
						go monitorDevice("/dev/input/"+handler, device.Uniq, idleDuration, &wg, quit, deadzone)
					}
				}
			}
			currentDevices[device.Uniq] = true
		}

		// Handle removed devices
		for uniq, quit := range deviceQuitChannels {
			if _, stillPresent := currentDevices[uniq]; !stillPresent {
				fmt.Printf("Device %s removed, stopping monitoring...\n", uniq)
				close(quit)
				delete(deviceQuitChannels, uniq)
			}
		}

		mu.Unlock()

		time.Sleep(5 * time.Second)
	}
}
