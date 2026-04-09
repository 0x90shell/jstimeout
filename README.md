# jstimeout

Daemon that auto-disconnects idle Bluetooth gamepads after a configurable timeout. Monitors `/dev/input/jsX` events and uses `bluetoothctl` to force a BT disconnect when no input is detected.

Originally written for DS3 controllers, whose timeout can't be configured without a PS3 due to Sony's proprietary implementation, but works with any controller listed in the config.

## Usage

```
jstimeout [options]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-m`, `--maxidle` | `3600` (1h) | Idle time in seconds before disconnect (1-10800) |
| `-z`, `--deadzone` | `6000` (~18%) | Axis deadzone threshold (0-32767); events below this are ignored as stick drift |
| `-c`, `--config` | (auto-resolved) | Path to config file |
| `-v`, `--verbose` | off | Enable debug logging |
| `-V`, `--version` | | Print version and exit |
| `--setup` | | Run diagnostics (permissions, config, systemd) |
| `-h`, `--help` | | Print help |

CLI flags override config file values.

## Configuration

Config file uses INI format with two sections:

```ini
# ~/.config/jstimeout/config

[settings]
maxidle = 3600      # idle timeout in seconds (1-10800)
deadzone = 6000     # axis deadzone threshold (0-32767)

[devices]
# Controller names from /proc/bus/input/devices (N: Name= field)
Sony PLAYSTATION(R)3 Controller
Sony Computer Entertainment Wireless Controller
```

### Config file lookup order

1. `--config` flag
2. `~/.config/jstimeout/config`
3. `/etc/jstimeout/config`
4. Auto-copy from `/usr/share/jstimeout/config.example`

### Migrating from v1

v1 used a plain device-names file (`.jstimeout.devices` or `~/.config/jstimeout/devices`). On first run, v2 automatically converts any legacy file found to the new config format and renames the old file to `.v1.bak`. No manual steps required.

v2 also renamed flags:

| v1 | v2 |
|----|----|
| `-maxidletime`, `-m` | `--maxidle`, `-m` |
| `-devicefile`, `-d` | removed (use `--config`, `-c`) |
| `-deadzone` | `--deadzone`, `-z` |

If you have a systemd override with old flags, update it:

```sh
systemctl --user edit jstimeout
```

## Diagnostics

Run `--setup` to check permissions, config, and systemd:

```
$ jstimeout --setup

jstimeout v2.0.0 - diagnostics

[✓] /dev/input/js0 readable
[✓] bluetoothctl found: /usr/bin/bluetoothctl
[✓] Config: /home/user/.config/jstimeout/config
    maxidle=3600  deadzone=6000  devices=2
[✓] Systemd service: /usr/lib/systemd/user/jstimeout.service
```

## Installation

### Arch Linux (AUR)

```sh
yay -S jstimeout-bin   # pre-built binary from GitHub release
yay -S jstimeout-git   # build from latest source
```

To auto-update `-git` packages when upstream changes:

```sh
yay --devel --save
```

### From source

Requires Go 1.21+.

```sh
git clone https://github.com/0x90shell/jstimeout.git
cd jstimeout
go build -o jstimeout .
sudo install -Dm755 jstimeout /usr/bin/jstimeout
sudo install -Dm644 config /usr/share/jstimeout/config.example
sudo install -Dm644 jstimeout.service /usr/lib/systemd/user/jstimeout.service
```

## Systemd User Service

```sh
systemctl --user enable --now jstimeout
journalctl --user -u jstimeout -b -e -f  # view logs
```

The service waits 10 seconds before starting to give the Bluetooth subsystem time to initialize.

To customize the timeout without editing the config file, create a systemd override:

```sh
systemctl --user edit jstimeout
```

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/jstimeout -m 1800
```

The blank `ExecStart=` clears the default before setting your own.

### UDev launch (alternative)

You can launch jstimeout via udev rules when specific devices connect instead of running it as a persistent service. This minimizes running processes but does not stop when controllers disconnect. The binary uses minimal resources, so a persistent service (above) is usually simpler.

`/etc/udev/rules.d/99-jstimeout.rules`:

```
SUBSYSTEM=="input", ATTRS{name}=="Sony PLAYSTATION(R)3 Controller", TAG+="systemd", ENV{SYSTEMD_USER_WANTS}="jstimeout.service"
SUBSYSTEM=="input", ATTRS{name}=="Sony Computer Entertainment Wireless Controller", TAG+="systemd", ENV{SYSTEMD_USER_WANTS}="jstimeout.service"
```

```sh
udevadm control --reload-rules
udevadm monitor --environment --udev  # verify on device connection
```

## License

MIT
