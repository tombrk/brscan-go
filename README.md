# brscan-go

One Go binary: discover a Brother AirScan scanner, expose HomeKit switches, save scanner-generated PDFs. No Brother driver, SANE, CUPS, MQTT, or UI.

```sh
go build .
./brscan-go
```

Requires Go 1.26.2+. Run on the same LAN as the scanner and Home Assistant. Allow incoming connections through the host firewall. Discovery uses mDNS; HAP uses an automatically selected TCP port.

## Pair with Home Assistant

1. Start the app. It prints a generated `XXX-XX-XXX` pairing code to stdout when unpaired.
2. In **Settings → Devices & services**, configure the discovered **Brother Scan** using **HomeKit Device** (not HomeKit Bridge).
3. Enter the code. No Apple device/account is needed.
4. Use **Scan Color** or **Scan Text** in your Zigbee-button automation via `switch.turn_on`. HA assigns the actual entity IDs.

Color requests `RGB24`; Text requests `BlackAndWhite1` (not grayscale). Both use the ADF when loaded, otherwise the flatbed. The selected switch stays on during scanning and resets off on completion or failure. Only one scan runs at a time. Turning off while scanning is rejected; cancellation from HA is not implemented. Failures are logged, not exposed as a separate HA entity.

Pairing identity, keys, and PIN persist in `filepath.Join(xdg.StateHome, "brscan-go")`, resolved by `github.com/adrg/xdg` using `$XDG_STATE_HOME` or its platform default. Override with `-state`. Keep this directory across restarts and deployments; treat it as secret. If upgrading from the earlier default, move your existing `.brscan/` directory to the new location to preserve pairing. To reset pairing, stop the app, remove its HA integration, and move aside the state directory before restarting.

The Brother panel buttons are not used. Scanning starts directly through eSCL, avoiding the unresolved native panel-session handoff.

## Options

```sh
./brscan-go -scanner 192.168.178.41  # bypass scanner discovery
./brscan-go -dir /data/scans -state /data/homekit
./brscan-go -now                   # scan color immediately and exit; no HAP
./brscan-go -dpi 300               # checked against scanner capabilities
```

PDF responses are stored unchanged in `scans/<timestamp>-<id>/001.pdf`, `002.pdf`, etc. Some scanners return one PDF per page, others a multi-page PDF; responses are not merged. Failed jobs retain a `.partial` directory for recovery. There is no OCR, rendering, or image conversion.

## Pi 5 build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o brscan-go .
```

Only the executable and writable scan/pairing directories are needed at runtime.
