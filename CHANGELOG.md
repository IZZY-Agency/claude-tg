# Changelog

## 0.2.0

First public release, under IZZY Agency.

- `tg` posts messages from your Claude Code sessions to a private Telegram
  bot chat on your phone: an alert when a session waits on a permission
  prompt or stops on an API error, every answer of a session you chose to
  follow, and messages Claude sends because it decided something is worth
  telling you. With the relay installed you answer from the phone: your text
  is typed into the right session, and a permission prompt can be approved or
  denied. `tg setup` installs a skill, six hooks and four allow rules into
  Claude Code at user scope; `tg remove` takes them out again.
- Linux support, x86_64 and arm64: the relay runs as a systemd user service
  (`~/.config/systemd/user/claude-tg-relay.service`), tmux is the terminal
  backend, and the away gate reads logind's idle hint; where no desktop session
  exists every alert is delivered, and `away_after_seconds` set to `0` forces
  that anywhere.
- Builds per platform on GitHub Releases: macOS arm64 and x86_64, signed with
  the IZZY Agency Developer ID and notarized, and Linux arm64 and x86_64,
  each as a tarball holding `tg`, `LICENSE` and `README.md` with a `.sha256`
  sidecar. A Homebrew formula: `brew install izzy-agency/tap/claude-tg`.
- `tg --version` and `tg -v` print the version, like `tg version`.

Versions 0.1.x were internal builds and were not published.
