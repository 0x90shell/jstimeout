# jstimeout

Program to automatically disconnect bluetooth gamepads when there is no activity for a specified time. It matches `/dev/input` with bluetooth MAC addresses to force a BT disconnect.

Originally written for DS3 controllers, whose timeout cannot be configured without a PS3 due to a proprietary timeout implementation by Sony, but works with any controller listed in the devices file.

## Usage

```
jstimeout [-m|-maxidletime <seconds>] [-d|-devicefile <path>] [-deadzone <threshold>]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-m`, `-maxidletime` | `3600` (1h) | Idle time in seconds before disconnect (1-10800) |
| `-d`, `-devicefile` | `.jstimeout.devices` | Path to the file with device names |
| `-deadzone` | `6000` (~18%) | Axis deadzone threshold (0-32767); events below this are ignored as stick drift |

## Device List Setup

Create a `.jstimeout.devices` file in the current working directory (or specify an absolute path via `-d`). Add device names as they appear in `/proc/bus/input/devices`, one per line.

```
Sony PLAYSTATION(R)3 Controller
Sony Computer Entertainment Wireless Controller
```

## Installation

Build the binary:

```sh
go build -o jstimeout jstimeout.go
```

Make the binary executable and add it to autorun in desktop mode, or better yet a systemctl service to recover it if it crashes.

### Option 1: User Service (Recommended)

Substitute `ExecStart` to the path for your jstimeout binary.

`~/.config/systemd/user/jstimeout.service`
```ini
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
```

```sh
systemctl daemon-reload
systemctl enable --user jstimeout.service
systemctl start --user jstimeout.service
journalctl -u jstimeout.service --user -b -e -f  # view logs
```

### Option 2: UDev Service Launch

Option 2 entails needing root access to modify udev rules so the process is initiated only when specific devices are connected. This is a great way to minimize running processes, but it does not stop when controllers are gone which mitigates the benefit. The binary uses very minimal resources so it doesn't seem like a major problem to leave it running all the time via Option 1.

The solution to have it terminate on disconnect entails creating systemd devices or modifying the program to terminate when no devices are present. To make the udev solution work, you will need to modify and maintain udev rules should you add new devices.

The below rules will launch the existing user service configured above. You'll want to disable auto-launch (`disable`) the user service. `StopWhenNeeded` was explored as an option for stopping the systemd service, but it did not make the service terminate when devices disconnected.

`/etc/udev/rules.d/99-jstimeout.rules`
```
# Rule for launching the jstimeout program for specific gamepads
SUBSYSTEM=="input", ATTRS{name}=="Sony PLAYSTATION(R)3 Controller", TAG+="systemd", ENV{SYSTEMD_USER_WANTS}="jstimeout.service"
SUBSYSTEM=="input", ATTRS{name}=="Sony Computer Entertainment Wireless Controller", TAG+="systemd", ENV{SYSTEMD_USER_WANTS}="jstimeout.service"
```

```sh
udevadm control --reload-rules
systemctl restart systemd-udevd.service
udevadm monitor --environment --udev  # verify on device connection
```
