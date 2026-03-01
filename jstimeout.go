/*

Program to automatically disconnect bluetooth gamepads when
there is no activity for a specified time. It matches /dev/input
with bluetooth mac addresses to force a BT disconnect. Originally
written for DS3 controllers, whose timeout cannot be configured
without a PS3 due to a proprietary timeout implementation by Sony,
but works with any controller listed in the devices file.

Use -m or -maxidletime arg to set idle time between 1s and 10800s (3h)
The default idle time is 3600s (1h)

Use -d or -devicefile to set the location to pull the device name list.
Without -d, the program checks for ".jstimeout.devices" in the current
working directory first, then ~/.config/jstimeout/devices. If neither
exists but the system example (/usr/share/jstimeout/devices.example)
is present, it is copied to ~/.config/jstimeout/devices automatically.
Add names from /proc/bus/input/devices for any additional controllers
that need to be monitored.

Use -deadzone to set the axis deadzone threshold (0-32767). Axis events
with |value| below this are ignored as stick drift. Default is 6000
(~18% of full range).

Make the binary executable and add it to autorun in desktop mode
or better yet a systemctl service to recover it if it crashes.

################################################################
######                 Device List Setup                  ######
################################################################

Devices file lookup order (without -d):
  1. ./.jstimeout.devices  (current working directory)
  2. ~/.config/jstimeout/devices
  3. Auto-copy from /usr/share/jstimeout/devices.example to #2

------
./.jstimeout.devices
------
Sony PLAYSTATION(R)3 Controller
Sony Computer Entertainment Wireless Controller

################################################################
######            [Opt 1] User Service Setup              ######
################################################################

Substitute exec start to the path for the jstimeout binary.

-------
~/.config/systemd/user/jstimeout.service
------
[Unit]
Description=jstimeout daemon
After=network.target auditd.service
[Service]
ExecStartPre=/bin/sleep 10
Type=idle
ExecStart=/home/user/bin/jstimeout
Restart=on-failure
RestartSec=5
[Install]
WantedBy=default.target

------
Commands
------
systemctl daemon-reload
systemctl enable --user jstimeout.service
systemctl start --user jstimeout.service
journalctl -u jstimeout.service --user -b -e -f # to see it working on

################################################################
######            [Opt 2] UDev Service Launch             ######
################################################################

Option 2 entails needing root access to modify udev rules so the process
is initiated only when specific devices are connected. This is a great
way to minimize running processes, but I found it does not stop when controllers
are gone which mitigates the benefit. The binary uses very minimal resources so
it doesn't seem like a major problem to leave it running all the time via Option 1
for my use case.

The solution to have it terminate on disconnect entails creating systemd devices or
modifying the program to terminate when no devices are present. I prefer having the
program monitor in an ongoing fashion, personally. To make the udev solution work,
you will need to modify and maintain udev rules should you add new devices.

The below rules will launch the existing user service we previously configured. You'll
want to disable auto-launch (disable) the user service. I explored "StopWhenNeeded" as
an option for stopping the systemd service, but that did not make the service terminate
when devices disconnected.

---
/etc/udev/rules.d/99-jstimeout.rules
---
# Rule for launching the jstimeout program for specific gamepads
SUBSYSTEM=="input", ATTRS{name}=="Sony PLAYSTATION(R)3 Controller", TAG+="systemd", ENV{SYSTEMD_USER_WANTS}="jstimeout.service"
SUBSYSTEM=="input", ATTRS{name}=="Sony Computer Entertainment Wireless Controller", TAG+="systemd", ENV{SYSTEMD_USER_WANTS}="jstimeout.service"

------
Commands
------
udevadm control --reload-rules
systemctl restart systemd-udevd.service
udevadm monitor --environment --udev # to see it working on device connection

*/

package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// JsEvent represents the Linux js_event struct from /dev/input/jsX.
// Layout: { __u32 time; __s16 value; __u8 type; __u8 number; }
type JsEvent struct {
	Time   uint32
	Value  int16
	Type   uint8
	Number uint8
}

const systemExample = "/usr/share/jstimeout/devices.example"

var specificNames []string

type Device struct {
	Name     string
	Uniq     string
	Handlers []string
}

// resolveDeviceFile finds the device list file. If the user didn't override
// with -d, it checks CWD first, then ~/.config/jstimeout/devices. If neither
// exists but the system example does, it copies it to the XDG path.
func resolveDeviceFile(path string, userOverride bool) string {
	if userOverride {
		return path
	}

	// Check CWD first (original behavior)
	if _, err := os.Stat(path); err == nil {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	xdgPath := filepath.Join(home, ".config", "jstimeout", "devices")

	// Check XDG config path
	if _, err := os.Stat(xdgPath); err == nil {
		return xdgPath
	}

	// Copy system example to XDG path if available
	if _, err := os.Stat(systemExample); err == nil {
		if err := os.MkdirAll(filepath.Dir(xdgPath), 0755); err != nil {
			fmt.Printf("Warning: could not create config dir: %v\n", err)
			return path
		}
		src, err := os.ReadFile(systemExample)
		if err != nil {
			fmt.Printf("Warning: could not read %s: %v\n", systemExample, err)
			return path
		}
		if err := os.WriteFile(xdgPath, src, 0644); err != nil {
			fmt.Printf("Warning: could not write %s: %v\n", xdgPath, err)
			return path
		}
		fmt.Printf("Copied default device list to %s — edit it to add your controllers\n", xdgPath)
		return xdgPath
	}

	return path
}

func loadSpecificNames(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			specificNames = append(specificNames, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading file: %v", err)
	}

	return nil
}

// parseJsEvent parses 8 bytes from /dev/input/jsX into a JsEvent.
func parseJsEvent(buf []byte) (JsEvent, error) {
	if len(buf) != jsEventSize {
		return JsEvent{}, fmt.Errorf("expected %d bytes, got %d", jsEventSize, len(buf))
	}
	return JsEvent{
		Time:   binary.LittleEndian.Uint32(buf[0:4]),
		Value:  int16(binary.LittleEndian.Uint16(buf[4:6])),
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

// parseInputDevicesFromReader parses the /proc/bus/input/devices format from any reader.
func parseInputDevicesFromReader(r io.Reader) ([]Device, error) {
	var devices []Device
	var currentDevice Device
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "N: Name=") {
			currentDevice.Name = strings.Trim(line[len("N: Name="):], `"`)
		} else if strings.HasPrefix(line, "U: Uniq=") {
			currentDevice.Uniq = strings.TrimSpace(line[len("U: Uniq="):])
		} else if strings.HasPrefix(line, "H: Handlers=") {
			currentDevice.Handlers = strings.Fields(line[len("H: Handlers="):])
		} else if line == "" && currentDevice.Name != "" {
			for _, handler := range currentDevice.Handlers {
				if strings.HasPrefix(handler, "js") {
					for _, name := range specificNames {
						if currentDevice.Name == name {
							devices = append(devices, currentDevice)
							break
						}
					}
				}
			}
			currentDevice = Device{}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %v", err)
	}
	return devices, nil
}

func parseInputDevices() ([]Device, error) {
	file, err := os.Open("/proc/bus/input/devices")
	if err != nil {
		return nil, fmt.Errorf("failed to open devices: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only
	return parseInputDevicesFromReader(file)
}

func inputChecker(devPath string, uniq string, deviceEvent chan struct{}, quit chan bool, deadzone int16) {
	fmt.Printf("Checking input on device: %s (%s)\n", uniq, devPath)

	file, err := os.Open(devPath)
	if err != nil {
		fmt.Printf("Failed to open device %s: %v\n", uniq, err)
		return
	}
	defer file.Close() //nolint:errcheck // read-only

	buf := make([]byte, jsEventSize)

	for {
		select {
		case <-quit:
			fmt.Printf("Stopping input check for device %s\n", uniq)
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
			fmt.Printf("Stopping monitoring for device %s\n", uniq)
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
	cmd := exec.Command("bluetoothctl", "disconnect", uniq)
	err := cmd.Run()
	if err != nil {
		fmt.Printf("Failed to disconnect %s: %v\n", uniq, err)
	} else {
		fmt.Printf("Disconnected device %s\n", uniq)
	}
}

func main() {
	maxIdle := flag.Int("maxidletime", 3600, "Maximum idle time in seconds (1-10800)")
	maxIdleShort := flag.Int("m", 3600, "Maximum idle time in seconds (1-10800)")
	filePath := flag.String("devicefile", ".jstimeout.devices", "Path to the file with device names")
	filePathShort := flag.String("d", ".jstimeout.devices", "Path to the file with device names")
	deadzoneFlag := flag.Int("deadzone", 6000, "Axis deadzone threshold (0-32767)")

	flag.Parse()

	// Validate deadzone
	if *deadzoneFlag < 0 || *deadzoneFlag > 32767 {
		fmt.Println("Error: deadzone must be between 0 and 32,767")
		os.Exit(1)
	}
	deadzone := int16(*deadzoneFlag)

	// Validate max idle time
	idleValue := *maxIdle
	if *maxIdleShort != 3600 {
		idleValue = *maxIdleShort
	}
	if idleValue < 1 || idleValue > 10800 {
		fmt.Println("Error: max idle time must be between 1 and 10,800 seconds (3 hours)")
		os.Exit(1)
	}

	// Resolve device file — use flag.Visit to detect if -d or -devicefile was explicitly set
	deviceFilePath := *filePath
	userOverride := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "d":
			deviceFilePath = *filePathShort
			userOverride = true
		case "devicefile":
			userOverride = true
		}
	})
	deviceFilePath = resolveDeviceFile(deviceFilePath, userOverride)

	fmt.Printf("Using device file: %s\n", deviceFilePath)

	// Load device names from file
	if err := loadSpecificNames(deviceFilePath); err != nil {
		fmt.Printf("Error loading device names: %v\n", err)
		return
	}

	// Print the device names on startup
	fmt.Println("Loaded device names:")
	for _, name := range specificNames {
		fmt.Println(" -", name)
	}

	idleDuration := time.Duration(idleValue) * time.Second
	fmt.Printf("Max idle time set to: %v seconds\n", idleDuration.Seconds())
	fmt.Printf("Axis deadzone set to: %d\n", deadzone)

	deviceQuitChannels := make(map[string]chan bool)
	var mu sync.Mutex

	for {
		devices, err := parseInputDevices()
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
