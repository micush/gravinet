# Vendored: @xterm/xterm

`xterm.js` and `xterm.css` in this directory are the unmodified `lib/xterm.js`
and `css/xterm.css` files from the `@xterm/xterm` npm package, version 6.0.0
(https://github.com/xtermjs/xterm.js), MIT licensed — see `LICENSE` in this
directory.

## Why this is here

gravinet's remote-shell feature (see `../shell.go`) needs a real terminal
emulator in the browser to render a PTY's output correctly — SGR colors,
cursor movement, the alternate screen buffer, scroll regions, and the dozens
of other VT100/VT220/xterm control sequences a real shell session and the
full-screen apps run inside it (`vim`, `htop`, `less`, `tmux`, ...) rely on.
An earlier version of this feature hand-rolled a small subset of that
parser directly in `ui.go`. It worked for plain shell output but broke on
real interactive use — see docs/changelog.md's v301 entry for the specific
bug (a 3-byte charset-select escape sequence, `ESC ( B`, misparsed as 2
bytes, leaking a stray character into the output on every occurrence) found
by capturing and replaying a real `htop` session. Fixing that class of bug
one incorrect-or-missing escape sequence at a time, discovered one
application at a time, doesn't converge — VT100/xterm compatibility is a
large, well-specified surface that a small hand-rolled subset will always
be behind on. `xterm.js` is the actual industry-standard implementation of
that surface (used by VS Code, GitHub Codespaces, Google Cloud Shell, and
effectively every other web-based terminal), so gravinet vendors it rather
than continuing to reimplement pieces of it.

This is a static, unauthenticated browser asset (served by
`handleXtermJS`/`handleXtermCSS` in `webadmin.go`, same trust level as the
main index page's own HTML/JS/CSS) — not a Go module dependency. gravinet's
`go.mod` still has zero third-party Go dependencies; this doesn't change
that.

## How to update

```
npm pack @xterm/xterm@<version>
tar -xzf xterm-xterm-<version>.tgz
cp package/lib/xterm.js   internal/webadmin/vendor/xterm/xterm.js
cp package/css/xterm.css  internal/webadmin/vendor/xterm/xterm.css
cp package/LICENSE        internal/webadmin/vendor/xterm/LICENSE
```

Then bump the version in this file and spot-check the shell feature still
works (`go test ./internal/webadmin/... -run Shell`, plus opening a real
shell and running something like `htop` or `vim` by hand — the Go tests
exercise the PTY/proxy/auth plumbing but can't drive a real browser).

Current version: **6.0.0**, packed 2026-07-10.
