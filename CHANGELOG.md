# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Your own server: logging in or adding an account asks for Tailscale or your own server (Headscale or another Tailscale-compatible one), and the Account tab shows each account's server.
- WireGuard: connect to a plain WireGuard server from a `.conf` file, through the same SOCKS5 and HTTP proxies. The config is kept encrypted with the device key.
- NetBird, off by default: its client (0.82.1, built from source with its own root certificates) joins NetBird's cloud or your own server, logging in with a QR code or a setup key. Its state is kept encrypted with the device key. An HTTP proxy is put in front of its SOCKS5 one.
- Settings > Network switches between Tailscale, WireGuard and NetBird (Y does too while MiniScale is off). Every login stays saved.
- While WireGuard or NetBird runs, /tmp/miniscale/routes tells the MiniDeck apps (1.1 and later) what to send through the proxy.

## [1.1.0] - 2026-11-12

### Added

- L2 and R2 jump to the top and the bottom of a list.
- The Devices tab shows how many devices are online, on this Miyoo's row.
- Files arriving by Taildrop are announced at the top of the screen until the next press.
- When Tailscale runs but doesn't answer, MiniScale says so instead of "Off", and A restarts it.

### Changed

- Tailscale 1.104.6, built with Go 1.27.4.

## [1.0.5] - 2026-11-08

### Changed

- Error notices say to try again.
- A logged-out account reads "Not logged in", and an offline device "Offline".
- Grey and red small text is easier to read (MiniDeck 1.0.3).

## [1.0.4] - 2026-11-07

### Fixed

- The status icon no longer draws over the screen while the Onion menu is paused, as when a game or tool started over SSH stops the menu and takes the screen.

## [1.0.3] - 2026-11-03

### Fixed

- Saving a Taildrop file no longer overwrites a file in Media/Taildrop whose name is the new file's name plus `.part` (for example `song.mp3.part` when `song.mp3` arrives).

## [1.0.2] - 2026-11-02

### Fixed

- The on-screen keyboard now shows how to leave it: B on an empty field goes back, and the footer shows B Back, or MENU Cancel once there is text (MiniDeck 1.0.1).

## [1.0.1] - 2026-11-02

### Fixed

- The status icon in the Onion menu used about 3% of the CPU all the time the menu was open, which cost battery. It now checks quickly only right after a button press and just before the menu's once-a-second repaint, and uses about 1%, with no blinking.

## [1.0.0] - 2026-11-02

### Added

- The first release: login by QR code, devices with ping, exit nodes, subnet routes, Tailscale DNS and SSH, shields up, accounts, Taildrop, start at boot, SOCKS5 and HTTP proxies, `miniscale-fwd` port forwards and a status icon in the Onion menu, on Tailscale 1.104.4.

[Unreleased]: https://github.com/zakijss/MiniScale/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/zakijss/MiniScale/compare/v1.0.5...v1.1.0
[1.0.5]: https://github.com/zakijss/MiniScale/compare/v1.0.4...v1.0.5
[1.0.4]: https://github.com/zakijss/MiniScale/compare/v1.0.3...v1.0.4
[1.0.3]: https://github.com/zakijss/MiniScale/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/zakijss/MiniScale/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/zakijss/MiniScale/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/zakijss/MiniScale/releases/tag/v1.0.0
