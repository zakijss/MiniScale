# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [1.0.1] - 2026-11-02

### Fixed

- The status icon in the Onion menu used about 3% of the CPU all the time the menu was open, which cost battery. It now checks quickly only right after a button press and just before the menu's once-a-second repaint, and uses about 1%, with no blinking.

## [1.0.0] - 2026-11-02

### Added

- The first release: login by QR code, devices with ping, exit nodes, subnet routes, Tailscale DNS and SSH, shields up, accounts, Taildrop, start at boot, SOCKS5 and HTTP proxies, `miniscale-fwd` port forwards and a status icon in the Onion menu, on Tailscale 1.104.4.

[Unreleased]: https://github.com/zakijss/MiniScale/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/zakijss/MiniScale/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/zakijss/MiniScale/releases/tag/v1.0.0
