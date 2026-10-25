# Changelog

## 0.3.0

- Questions (`AskUserQuestion`) and permission prompts reach the phone with
  buttons: one per option for a question, with **Other** for your own text
  and **Done** for multi-select; **Yes**, **No** and Claude Code's own
  "always allow" choices for a permission prompt. The choices are listed in
  full, numbered, in the message; the buttons carry the number. Replying to a
  permission message with text denies it and sends your text to Claude.
- Buttons appear when the relay runs, the session is in agterm or tmux, and
  the hook finds the dialog of its call on the pane. Prompts of other tools
  (MCP among them), plan approval, subagent prompts, a question call with two
  identical question texts and any dialog tg cannot match keep the plain
  alert.
- A tap answers the prompt while the dialog stays live on the Mac. Whichever
  side answers first wins, and the phone message says what happened:
  `answered at the Mac` (including Esc), `session ended` when Claude was
  quit, or `expired: answer at the Mac` after 12 hours.
- A dialog left open reaches the phone once you have been away for
  `away_after_seconds`.
- Text sent to a session with an open question or permission prompt is not
  typed; you get a pointer to the buttons, or to the Mac. With buttons on the
  phone, `/approve` and `/deny` decide that prompt like the buttons (as a
  reply, exactly the replied prompt). Without buttons they type only into a
  plain Bash or Read "Do you want to proceed?" dialog; any other dialog is
  answered at the Mac.
- Upgrading: run `tg setup` to move the `PermissionRequest` hook to the new
  synchronous `permission-wait` hook, reload the relay with `tg relay
  install` (until then a running 0.2.0 relay means plain alerts), and restart
  Claude sessions that were already open.

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
