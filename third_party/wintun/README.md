# Wintun

[gravinet]'s Windows TUN backend uses the **Wintun** driver by WireGuard LLC.

- The signed `wintun.dll` binaries are **Copyright © 2018–2021 WireGuard LLC**,
  used under the **Wintun Prebuilt Binaries License** (see
  [`prebuilt-binaries-license.txt`](prebuilt-binaries-license.txt) in this
  directory, which is the verbatim license shipped inside the Wintun zip).
- [gravinet] calls Wintun only through its documented `wintun.h` API
  (`LoadLibraryEx` + `GetProcAddress`), which is the redistribution condition the
  license requires.
- Only the official signed DLLs from <https://www.wintun.net/> are distributed
  (Wintun 0.14.1, zip SHA-256
  `07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51`). The build
  pipeline downloads and checksum-verifies them.
- "Wintun", "WireGuard", and WireGuard LLC names are not used to endorse or
  promote [gravinet].

This license covers the bundled Wintun binary only; [gravinet] itself is licensed
under the GPLv3 (see the top-level `LICENSE`).
