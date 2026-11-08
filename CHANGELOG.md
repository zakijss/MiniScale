# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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

[Unreleased]: https://github.com/zakijss/MiniScale/compare/v1.0.5...HEAD
[1.0.5]: https://github.com/zakijss/MiniScale/compare/v1.0.4...v1.0.5
[1.0.4]: https://github.com/zakijss/MiniScale/compare/v1.0.3...v1.0.4
[1.0.3]: https://github.com/zakijss/MiniScale/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/zakijss/MiniScale/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/zakijss/MiniScale/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/zakijss/MiniScale/releases/tag/v1.0.0
