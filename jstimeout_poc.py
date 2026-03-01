#!/usr/bin/env -S python3 -u

'''
Script to automatically disable the bluetooth gamepad when
there is no activity for a specified time. It matches /dev/input
with bluetooth mac addresses for DS3 controllers to force a BT disconnect.
This is necessary, because DS3 timeout cannot be configured without a PS3
due to a proprietary timeout implementation by Sony.

use -m or --maxidletime command to set idle time between 1s and 10800s (3h)
the default is 3600s (1h)
Modify specfic_names list to include any other controllers that need monitoring.

Мake the script executable and add it to autorun in desktop mode
or better yet a systemctl service to recover it if it crashes.

################################################################
######                 Service Setup                      ######
################################################################
------
File |
------
[Unit]
Description=jstimeout daemon
After=network.target auditd.service

[Service]
ExecStartPre=/bin/sleep 10
Type=idle
ExecStart=/home/gandalf/bin/jstimeout
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target

----------
Commands |
----------
systemctl daemon-reload
systemctl enable --user jstimeout.service
systemctl start --user jstimeout.service

'''

import struct
from datetime import datetime as dt
import sys
import os
import select
import time
from threading import Thread, Event
import argparse

# List of known devices to query from /proc/bus/input/devices
specific_names = ["Sony PLAYSTATION(R)3 Controller", "Sony Computer Entertainment Wireless Controller"]

# Dictionary to keep track of running threads by their uniq identifier and dev path
running_threads = {}


# Argument parser to accept maxidletime from command line
def parse_arguments():
    parser = argparse.ArgumentParser(description="Bluetooth gamepad idle disconnect script.")
    parser.add_argument('-m', '--maxidletime', type=int, default=3600,
                        help='Maximum idle time in seconds before disconnecting. Must be between 1 and 10800 seconds.')
    args = parser.parse_args()
    # Validate maxidletime
    if not (1 <= args.maxidletime <= 10800):
        print("Error: maxidletime must be a number between 1 and 10800 seconds.")
        sys.exit(1)
    return args.maxidletime


# Function to parse /proc/bus/input/devices and return matching devices with "js" handler and uniq field
def parse_input_devices(specific_names):
    devices = []
    current_device = {}
    try:
        with open("/proc/bus/input/devices", "r") as f:
            for line in f:
                line = line.strip()
                # Identify the device name
                if line.startswith("N: Name="):
                    device_name = line.split('=')[1].strip('"')
                    current_device['name'] = device_name
                # Identify the uniq field
                elif line.startswith("U: Uniq="):
                    uniq = line.split('=')[1].strip()
                    current_device['uniq'] = uniq
                # Identify the handler
                elif line.startswith("H: Handlers="):
                    handlers = line.split('=')[1].strip().split()
                    current_device['handlers'] = handlers
                # End of a device block, add to list if it matches specific names and has "js" in handlers
                elif line == "":
                    if ('name' in current_device and 
                        current_device['name'] in specific_names and
                        'uniq' in current_device and
                        any("js" in handler for handler in current_device['handlers'])):
                        devices.append(current_device)
                    current_device = {}

        return devices
    except FileNotFoundError:
        print("Error: Unable to open /proc/bus/input/devices. Are you running this on a system with /proc available?")
        return []


# Input checker function that listens for device events
def input_checker(dev, uniq_and_dev, device_event):
    try:
        while os.path.exists(dev):
            while True:
                time.sleep(1)
                EVENT_SIZE = struct.calcsize("llHHI")
                file = open(dev, "rb")
                break
            while True:
                r, w, e = select.select([file], [], [], 0)
                if file in r:
                    try:
                        event = file.read(EVENT_SIZE)
                        struct.unpack("llHHI", event)
                        device_event.set()
                        # commenting out to de-clutter journalctl logs
                        # print(f"movement detected for {uniq_and_dev}")
                    except:
                        break
                else:
                    pass
    finally:
        # Clean up and remove from running_threads when done
        if uniq_and_dev in running_threads:
            del running_threads[uniq_and_dev]


# Timer function to disconnect device if idle for too long
def timer(devid, maxidletime, dev, uniq_and_dev, device_event):
    currtime = dt.now()
    prevtime = currtime
    try:
        while True:
            time.sleep(1)
            currtime = dt.now()
            if os.path.exists(dev):
                if device_event.is_set():
                    # commenting out to de-clutter journalctl logs
                    # print(f"date updated for {uniq_and_dev}")
                    prevtime = currtime
                    device_event.clear()

                if (currtime - prevtime).total_seconds() >= maxidletime:
                    print(f"Device {uniq_and_dev} has been idle for {maxidletime} seconds, disconnecting...")
                    os.system(f"echo disconnect {devid} | bluetoothctl")
                    time.sleep(1)
                    os.system("echo exit | bluetoothctl")
                    sys.exit()
            else:
                sys.exit()
    finally:
        # Clean up and remove from running_threads when done
        if uniq_and_dev in running_threads:
            del running_threads[uniq_and_dev]

# Main function to initiate threads for matching devices
def start_threads_for_devices(devices, maxidletime):
    global running_threads

    for device in devices:
        for handler in device['handlers']:
            if handler.startswith('js'):  # Ensure the handler is a joystick
                dev_path = f"/dev/input/{handler}"
                uniq_and_dev = (device['uniq'], dev_path)

                # Check if a thread is already running for this device's uniq and dev_path
                if uniq_and_dev not in running_threads:
                    print(f"Starting threads for device: {device['name']} (Handler: {dev_path}, Uniq: {device['uniq']})")
                    # Create a per-device event
                    device_event = Event()
                    # Start input checker thread
                    t1 = Thread(target=input_checker, args=(dev_path, uniq_and_dev, device_event))
                    t1.start()
                    # Start timer thread
                    t2 = Thread(target=timer, args=(device['uniq'], maxidletime, dev_path, uniq_and_dev, device_event))
                    t2.start()

                    # Track the running threads by (uniq, dev_path)
                    running_threads[uniq_and_dev] = {'input_checker': t1, 'timer': t2}


# Periodically query for new devices and start threads if needed
def query_devices_periodically(maxidletime):
    global running_threads
    while True:
        devices = parse_input_devices(specific_names)
        if devices:
            start_threads_for_devices(devices, maxidletime)
        else:
            # commented out to minimize journalctl clutter
            # print(f"No devices found matching names: {specific_names}")
            pass  # nop cause not printing

        # Check for new devices every 5 seconds
        time.sleep(5)


if __name__ == "__main__":
    # Get maxidletime from command-line arguments
    maxidletime = parse_arguments()

    print(f"Starting Joystick Idle Monitoring w/ Idle Cutoff of {maxidletime / 60} minutes")

    # Start querying devices in a loop
    query_devices_periodically(maxidletime)
