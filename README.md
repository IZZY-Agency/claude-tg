<p align="center">
  <a href="https://github.com/IZZY-Agency/claude-tg/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/IZZY-Agency/claude-tg/ci.yml?branch=main&amp;label=CI" alt="CI"></a>
  <a href="https://github.com/IZZY-Agency/claude-tg/releases/latest"><img src="https://img.shields.io/github/v/release/IZZY-Agency/claude-tg?label=release&amp;color=f5c518" alt="Latest release"></a>
  <a href="https://github.com/IZZY-Agency/homebrew-tap"><img src="https://img.shields.io/badge/brew-izzy--agency%2Ftap%2Fclaude--tg-f5c518" alt="Homebrew formula"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/IZZY-Agency/claude-tg" alt="MIT license"></a>
</p>

# claude-tg

## What it does

`tg` is a small command-line tool for a Mac or a Linux machine that runs Claude Code. It posts messages from your Claude Code sessions to a private Telegram bot chat on your phone: an alert when a session waits on a permission prompt or stops on an API error, every answer of a session you chose to follow, and messages Claude sends because it decided something is worth telling you. With the relay installed you can answer from the phone: your text is typed into the right session, and you can approve or deny a permission prompt. `tg setup` installs everything into Claude Code at user scope: a skill, six hooks and four allow rules. `tg remove` takes it out again.

[Releases](https://github.com/IZZY-Agency/claude-tg/releases) ·
[Issues](https://github.com/IZZY-Agency/claude-tg/issues)

## Requirements

- macOS 13 Ventura or later, Apple Silicon or Intel. The release builds are one per architecture (arm64 and x86_64), built with Go 1.27, which requires macOS 13.
- Or Linux, x86_64 or arm64, with these limits: the relay runs as a systemd user service; tmux is the only terminal backend; the away gate relies on logind's idle hint, which desktop environments set. Where no desktop session exists (ssh, tty, containers) or `loginctl` is missing, every alert is delivered; set `away_after_seconds` to `0` in `~/.config/claude-tg/config.json` to force that anywhere.
- Claude Code installed, with the `claude` command on PATH. `tg` runs `claude agents --json` to learn session names.
- A Telegram account on your phone.
- Only for replies from the phone: the Claude session must run inside agterm (macOS) or tmux (3.1 or newer). Nothing else needs either tool.

## 1. Create your bot

1. In Telegram, open a chat with @BotFather.
2. Send `/newbot`.
3. Choose a display name when asked.
4. Choose a username. It must end in `bot`.
5. BotFather replies with the HTTP API token. Copy it. `tg setup` asks for it in step 3.

Keep the token secret. Anyone who has it can read and write as the bot. Do not add the bot to any group. It is for a private chat between you and the bot only; setup ignores messages that do not come from a private chat. `tg setup` validates the token with Telegram before it writes anything to disk.

## 2. Install the binary

With Homebrew, on macOS and Linux:

```sh
brew install izzy-agency/tap/claude-tg
```

The formula lives in [IZZY-Agency/homebrew-tap](https://github.com/IZZY-Agency/homebrew-tap) and installs the same signed, notarized build as the archives below; `brew upgrade` keeps it current. The command is `tg`.

Without Homebrew, download the archive for your platform and its `.sha256` from the [latest release](https://github.com/IZZY-Agency/claude-tg/releases/latest):

| Platform | Archive |
| --- | --- |
| macOS, Apple Silicon | `tg-<version>-macos-arm64.tar.gz` |
| macOS, Intel | `tg-<version>-macos-x86_64.tar.gz` |
| Linux, arm64 | `tg-<version>-linux-arm64.tar.gz` |
| Linux, x86_64 | `tg-<version>-linux-x86_64.tar.gz` |

In the folder you downloaded them to (the example is the Apple Silicon archive; on Linux without `shasum`, `sha256sum -c` reads the same file):

```sh
shasum -a 256 -c tg-<version>-macos-arm64.tar.gz.sha256   # must report OK
tar xzf tg-<version>-macos-arm64.tar.gz                    # extracts tg, LICENSE and README.md
mkdir -p ~/.local/bin
mv tg ~/.local/bin/tg
```

The macOS binaries are signed with the IZZY Agency Developer ID and notarized by Apple, so Gatekeeper lets a downloaded copy run. The first launch may need a network connection: a bare command-line binary cannot carry its notarization ticket, so Gatekeeper fetches it online. If macOS still refuses to start it, remove the download mark:

```sh
xattr -d com.apple.quarantine ~/.local/bin/tg
```

Make sure `~/.local/bin` is on your PATH (`echo $PATH`). If it is not, add it in your shell's startup file and open a new terminal. Then check:

```sh
tg version        # prints: tg <version>   (tg --version and tg -v print the same)
```

## 3. Run tg setup

Run `tg setup` in a terminal. It asks a few questions, in this order.

1. **Token.** `Bot token (from @BotFather): ` Paste the token and press Enter. What you type is not echoed. If a token is already known, setup prints `Bot token: found in <source>.` and asks `Reuse it? [Y/n]`. It looks, in order, at the `--token` flag, the `TELEGRAM_BOT_TOKEN` environment variable, an existing `~/.config/claude-tg/config.json`, and `~/.claude/channels/telegram/.env` left behind by the official Telegram channel plugin. Answer `n` to stop, then rerun with `--token <token>`.
2. **Token check.** Setup calls Telegram's `getMe`. A wrong token stops it with `tg setup: token rejected by Telegram: ...` and nothing is written. Otherwise it prints `Bot: @<username>`.
3. **Chat.** `Send any message to @<bot> in Telegram now... waiting up to 60 s`. Open the bot's chat on your phone and send anything. Setup prints `Got a message from <name> (id <id>).` and asks `Use this chat? [Y/n]`.
4. **Files.** Setup writes the config, installs the skill, the hooks and the allow rules, and prints one line per item: `Config: ...`, `Skill: ...`, `Hooks and allow rules: ...`. When it changes `~/.claude/settings.json` it keeps a backup next to it and names it.
5. **CLAUDE.md.** `Add a one-line rule to ~/.claude/CLAUDE.md pointing Claude at the skill? [Y/n]`. The line tells Claude that the skill exists.
6. **Test.** Setup sends a silent message, `tg setup complete on <hostname>`, to the chat and prints `Sent a test message. Done.`

What setup writes:

| What | Where |
| --- | --- |
| config (0600) | `~/.config/claude-tg/config.json` |
| state, spool, log | `~/.local/state/claude-tg/` |
| skill | `~/.claude/skills/telegram-notify/` (marker file `.tg-managed`) |
| hooks + allow rules | `~/.claude/settings.json` (backup `settings.json.bak-<ts>` when changed) |
| one-line rule | `~/.claude/CLAUDE.md` between `<!-- claude-tg:start/end -->` markers (asked first) |

Non-interactive: `tg setup --yes --token <t> --chat-id <id>`. `--chat-id` is your own Telegram user id for a private chat; with it, setup skips the "send any message" step. Other flags: `--no-claude-md` leaves `CLAUDE.md` alone; `--force` overwrites a `telegram-notify` skill directory that `tg` did not install. Re-running setup is idempotent and repoints the hooks if the binary moved.

`tg setup --check` reports what is installed and changes nothing: the config path with the chat id and `away_after_seconds`, how many of the six hooks and four allow rules are in `settings.json`, whether the skill and the `CLAUDE.md` block are present, whether the relay service is installed, which terminal backends are on PATH (`terminal backends: agterm, tmux`, or `none`), and whether undelivered messages wait in the spool. If the binary moved it prints `binary: config says <old> but this is <new>; run tg setup to repoint the hooks`.

Restart Claude sessions that were already open when setup ran. They keep their old hook snapshot until then; only sessions started after setup have the hooks.

## What reaches the phone

| Icon | When | Sound |
| --- | --- | --- |
| 🔔 | `tg send "..."` (default level): a Claude decided something is critical, or you asked to be pinged | yes |
| 📋 | `tg send --level info "..."`: routine lines from a monitoring loop | silent |
| 💬 | follow mode: every final answer of a followed session, split into up to 4 parts | silent |
| 🔐 | a session has waited ~6 s on a permission prompt, naming the tool and command | yes |
| ✋ | a background agent needs input | yes |
| 💥 | a session stopped on an API error (rate limit, auth, billing) | yes |
| ⚙️ | follow on/off confirmations, test messages | silent |

Every message starts with the session name (`acme-shop-92`, the same name
Claude Code shows), then an optional bold title, then the body rendered from
Claude's markdown: bold, italics, code, fenced blocks, lists, links, quotes,
tables in monospace.

Hook-generated alerts (🔐 ✋ 💥) are sent only when you have been away from
the machine for `away_after_seconds` (default 120; `0` disables the gate) and
the same alert was not sent in the last 10 minutes. macOS reports the idle
time itself; on Linux it comes from logind's idle hint (see Requirements).
Your own `tg send` calls and the follow feed are never held back.

Claude sends on its own only in the cases the `telegram-notify` skill names: a production or deploy step failed or a release is half applied, a destructive step is next and needs a decision only you can take, a long unattended task (more than about 10 minutes) finished or failed, or Claude is blocked on something the hooks cannot see. Not for routine progress, not for "task done" after a short task, not for anything you are watching live. When you ask ("notify me when done", "monitor X and report", "follow this on Telegram") Claude runs `tg send` or `tg follow on` for that. The `CLAUDE.md` line points Claude at the skill.

## Commands

```sh
tg send [--level critical|info] [--title t] [--silent] [--from label] [--session id] <text | ->
tg follow on|off|status [--session id]     # live feed of a session's answers
tg sessions [--json]                       # known sessions, follow state, alive, terminal
tg test                                    # silent test message
tg log [-n N]                              # tail of ~/.local/state/claude-tg/tg.log
tg hook <kind>                             # called by the hooks, never by hand
tg relay [--once] [--verbose]              # foreground relay; install|uninstall|status for the relay service
tg setup [--token t] [--chat-id id] [--yes] [--check] [--no-claude-md] [--force]
tg remove [--purge]
tg version                                 # also: tg --version, tg -v
tg help
```

`tg sessions` prints one row per known session with the columns NAME, FOLLOW, ALIVE, TERMINAL, CWD and LAST_SEEN. TERMINAL is `agterm`, `tmux %5` (tmux with the pane id), or `-` when no pane is known for the session.

Exit codes: 0 ok, 1 usage, 2 not configured, 3 Telegram refused the message,
4 network failure (message spooled and retried on the next call), 5 unreadable
hook input (hooks always exit 0 regardless).

Inside a Claude session the session id comes from `CLAUDE_CODE_SESSION_ID`;
from a plain terminal use `--from <label>` or `--session <id>`.

## Replies from the phone

`tg relay install` installs a service that long-polls the bot and delivers
your messages into sessions through agterm or tmux: on macOS a launchd user
agent (`agency.izzy.claude-tg.relay`), on Linux a systemd user service
(`~/.config/systemd/user/claude-tg-relay.service`). It prints `Loaded. The
relay starts now and on every login.` `tg relay status` shows whether the
service is loaded and the relay runs; `tg relay uninstall` removes it;
`tg relay --once --verbose` handles pending updates in the foreground for
debugging. The service's own output goes to `~/.local/state/claude-tg/relay.out.log`.

`tg relay install`, `tg relay status` and `tg setup --check` print which backends they found, for example `terminal backends: agterm, tmux` (`none` when there is neither). They look in the relay's own PATH (`~/.local/bin`, `/opt/homebrew/bin`, `/home/linuxbrew/.linuxbrew/bin`, `/usr/local/bin`, `/usr/bin`, `/bin`), not your shell's, so the report matches what the relay will see. When neither tool is there the relay still installs but warns:

```
warning: neither agtermctl nor tmux is on PATH; replies and /approve will be refused, /sessions, /follow and /status still work
```

Addressing a session, in order:

1. Swipe-reply to a message that session sent. A reply to a message no
   session is known to have sent is refused, never guessed.
2. Start with `@name`; a unique prefix is enough (`@acme`). `name:` works with
   the exact session name only.
3. With no address, the single followed session gets the text.
4. Otherwise the bot shows a keyboard of live sessions; tap one.

Text is typed into the session's pane in two steps: the text itself (with a
leading space, so a leading digit can never pick a dialog option), a second
look at the screen, then Enter. If a permission dialog appeared in between,
Enter is withheld and you are told to resend. Newlines become spaces, 4000
characters max. When the session is in the middle of a turn the text is queued
and you are told so. A session waiting on a permission prompt refuses plain
text: use `/approve` (types `1`) or `/deny` (sends Esc), by reply or with
`@name`; the dialog is read twice and must be the same both times.

After a reply or an approval from the phone, that session's next answer comes
back to the phone even when it is not followed (two answers when it was busy).

Commands: `/sessions`, `/follow name`, `/unfollow name`, `/approve`, `/deny`,
`/status`, `/help`. Feedback: 👍 delivered, 👌 approved, 👎 denied (Telegram
allows a fixed set of reaction emojis), otherwise a reply with the reason. Only
your Telegram id in your chat is honoured; everyone else is dropped silently.
Each update is acknowledged before it is acted on, so a crash never replays a
keystroke. Messages older than 10 minutes while the startup backlog drains are
skipped with one notice.

### Running Claude in tmux

If you do not use agterm, and always on Linux, run Claude inside tmux so the relay has a pane to type into:

```sh
brew install tmux     # on Linux: your distribution's package manager
tmux new -s work      # opens a tmux session named "work"
claude                # start Claude inside it
```

From then on replies reach that session. The hooks run inside the session's environment and record its tmux pane each time a turn ends, so a session becomes addressable after its first completed turn, and a session you resume in another terminal is re-bound after its next one. Sessions need no restart when you replace the `tg` binary, but the relay does: run `tg relay install` again, it reloads the service. tmux 3.1 or newer is required. Then install the relay if you have not yet, and check it:

```sh
tg relay install
tg relay status
```

### Linux notes

- The relay runs as a systemd user service, `~/.config/systemd/user/claude-tg-relay.service`; `tg relay install` writes it, reloads the user manager, enables it and (re)starts it. Its output goes to `~/.local/state/claude-tg/relay.out.log`, not the journal.
- On a headless machine run `loginctl enable-linger $USER` once, otherwise the user manager and the relay stop when your last session ends. Over a bare ssh login `systemctl --user` may report `Failed to connect to bus`; `tg relay status` then shows `unknown (...)`.
- tmux is the only terminal backend on Linux (agterm is a macOS terminal). The relay looks for `tmux` in `~/.local/bin`, `/opt/homebrew/bin`, `/home/linuxbrew/.linuxbrew/bin`, `/usr/local/bin`, `/usr/bin` and `/bin`.
- The away gate uses logind's idle hint, which desktop environments set after their own idle timeout, so hook alerts arrive after that timeout plus `away_after_seconds`. Where no desktop session exists (ssh, tty, containers) or `loginctl` is missing, every alert is delivered.

Two messages mean the relay could not type:

- `<name> runs in a terminal I cannot type into (replies need agterm or tmux)`: no pane is known for that session. Either it runs in neither agterm nor tmux, or no hook has run in it yet. Start Claude inside tmux or agterm, or let the session finish one turn and send again.
- `<name>'s <backend> pane <target> is not reachable (is <tool> installed and the pane still open?)`: a pane was recorded but the relay cannot reach it. Check that `tmux` (or `agtermctl`) is installed in a directory on the relay's PATH, which is `~/.local/bin`, `/opt/homebrew/bin`, `/home/linuxbrew/.linuxbrew/bin`, `/usr/local/bin`, `/usr/bin` and `/bin`, and that the pane was not closed.

## Privacy and security

- The bot token is stored in `~/.config/claude-tg/config.json` with mode 0600, in a directory with mode 0700. `tg` never prints it, and replaces it with `[redacted]` if it ever appears in a message label, title or body.
- The relay acts only on messages from your own Telegram user id in your own chat, both recorded at setup. Everything else is dropped silently.
- What leaves the machine, and only to Telegram's Bot API:
  - the session name at the top of every message (the name Claude Code shows, or `<folder>-<first 6 characters of the session id>` when it cannot be resolved);
  - the final answer text of a followed session, and the next answer of a session after you replied to it or approved a prompt from the phone;
  - the title and body Claude passes to `tg send`;
  - for a permission alert: the tool name and a one-line summary, which is the command for `Bash`, the file path for `Edit`, `Write`, `Read`, `MultiEdit` and `NotebookEdit`, and the raw tool input for other tools, cut to 120 characters;
  - for a stopped session: the error and its details as Claude Code reported them;
  - for a background agent that needs input: the notification text;
  - on `/sessions` from the phone: session names, busy or idle, follow state, and the last folder name of each session's working directory;
  - once, at setup: `tg setup complete on <hostname>`.
- Nothing is sent unless a hook event fires, Claude runs `tg send` or `tg follow`, or you run `tg` yourself. Hook alerts also wait until you have been away from the machine for `away_after_seconds`.
- Message bodies, hook alerts and tool summaries pass through a redaction step that replaces obvious credentials with `[redacted]`: GitHub tokens (`ghp_...`), GitLab tokens (`glpat-...`), keys starting with `sk-`, Slack tokens (`xoxb-...`, `xoxp-...`), AWS access key ids (`AKIA...`), and `token=`, `key=`, `secret=` or `password=` followed by eight or more characters. This is a safety net, not a guarantee. The skill tells Claude never to put secrets, tokens, passwords or full logs into a message.
- The local log is `~/.local/state/claude-tg/tg.log` (`tg log`). It records sends, hook events and relay activity. The relay service's own output goes to `~/.local/state/claude-tg/relay.out.log`. Both stay on the machine.
- `tg setup` reads `~/.claude/channels/telegram/.env` only to offer an existing token for reuse, and asks before using it.

## Troubleshooting

**No message arrived during setup.** Setup ended with `tg setup: no message arrived; send one to the bot and run setup again, or pass --chat-id`. Send the message after the `Send any message to @<bot> ...` line appears, from the private chat with the bot, not from a group. If setup instead says `Telegram says a webhook is active for this bot; remove it with deleteWebhook before running setup`, another integration registered a webhook on this bot, which blocks the `getUpdates` polling `tg` uses. Remove the webhook with the Bot API's `deleteWebhook`, or create a new bot.

**macOS refuses to start `tg` on the first run.** The release build is Developer ID signed and notarized; Gatekeeper fetches the ticket online when a downloaded binary first runs, so make sure the Mac is online and try again. If it still refuses, remove the download mark with `xattr -d com.apple.quarantine ~/.local/bin/tg` (step 2); that skips the download check rather than repairing anything, so do it only for a file you got from a trusted source. A downloaded copy of a build made without `SIGN_IDENTITY` (a plain `make release` or `make dist`) is always refused until that mark is removed; a build you made on the same Mac carries no mark and runs. `codesign -dvv ~/.local/bin/tg` shows who signed yours: `Authority=Developer ID Application: ...` for the release build, `Signature=adhoc` for a local one (the authority names the signer, it does not prove notarization).

**`<name> runs in a terminal I cannot type into`.** The session is not inside agterm or tmux, or no hook has run in it yet: a session started before setup has no hooks until it is restarted, and a fresh session records its pane on its first turn, so wait one turn and send again. See "Running Claude in tmux".

**Nothing reaches the phone from a session that was open during setup.** Hooks are only active in sessions started after setup. Restart that session.

**Claude asks for permission to run `tg`.** The allow rules cover `tg send`, `tg follow`, `tg sessions` and `tg test` when they are the whole command. A chained command such as `cd x && tg send ...` is not covered. `tg setup` and `tg remove` are deliberately not covered. `tg setup --check` should report `allow rules: 4/4`.

**The relay is not running.** `tg relay status` prints a `launchd agent:` line (on Linux a `systemd user service:` line) and a `relay:` line. If the service is `installed but not loaded` or `failed`, run `tg relay install` again. The reason for a crash is in `~/.local/state/claude-tg/relay.out.log`; `tg relay --once --verbose` runs one pass in the foreground and prints what it does.

**`tg setup` says the relay owns the bot's updates.** Only one process can poll the bot. Pass `--chat-id <id>`, or run `tg relay uninstall`, then `tg setup`, then `tg relay install`.

**The binary moved.** `tg setup --check` prints `binary: config says ... but this is ...`. Run `tg setup` again; it repoints the hooks.

## Uninstall

```sh
tg relay uninstall    # stops and removes the launchd agent or the systemd user service
tg remove             # removes the hooks, allow rules, skill and CLAUDE.md block (and the relay service if it is still installed)
tg remove --purge     # the same, plus deletes the config file and ~/.local/state/claude-tg/
```

The `~/.claude/settings.json.bak-<ts>` backups are left in place; delete them yourself when you no longer need them. Finally delete `~/.local/bin/tg`, or run `brew uninstall claude-tg` if Homebrew installed it.

## Security

Please report vulnerabilities privately. See [SECURITY.md](SECURITY.md).

## Development

```sh
make build      # builds bin/tg with Go (stdlib only, no cgo)
make test       # unit tests (payload fixtures, no network)
make live-test  # TG_LIVE=1: sends two labelled test messages through the real bot
make vet
make install    # builds and copies the binary to ~/.local/bin/tg
make dist       # the release archives: macOS arm64 and x86_64, Linux arm64 and x86_64, each with a .sha256, in dist/<version>/
make dist SIGN_IDENTITY="Developer ID Application: <team>" NOTARY_PROFILE=<profile>   # the published build: macOS binaries Developer ID signed, hardened runtime, notarized
make release    # macOS only: one universal macOS binary, ad-hoc signed, as dist/tg-<version>-macos-universal.tar.gz plus a .sha256 file, for hand-outs
```

Go 1.27, standard library only, no cgo. The version comes from `git describe --tags --always --dirty`, so tag before `make dist`. `scripts/package.sh <version> <outdir>` does the work of `make dist`: it cross-compiles the four binaries with `CGO_ENABLED=0 -trimpath` and, on macOS with `SIGN_IDENTITY` set, signs the macOS ones with `codesign --options runtime --timestamp` and, with `NOTARY_PROFILE`, notarizes each through `scripts/notarize.sh`, which submits the binary, waits for Apple's answer, prints the log on rejection and checks that Gatekeeper reports `Notarized Developer ID`. The keychain profile is created once with `xcrun notarytool store-credentials <profile> --apple-id <id> --team-id <team>`. Unsigned archives are reproducible (fixed entry order and mtime, no owner names, no gzip timestamp); a Developer ID signature carries a fresh timestamp, so signed builds differ byte for byte. `make release` additionally needs `lipo`. `bin/` and `dist/` are git-ignored.

Packages: `config`, `state`, `render`, `telegram`, `session`, `idle`, `settings`, `assets`, `send`, `hooks`, `relay`, `term` (backend-neutral terminal access), `agterm`, `tmux`; commands in `cmd/tg`.

claude-tg is developed in a private repository. Each release is published here
as a single commit tagged with its version, so this tree always matches the
shipped build. Bug reports and feature requests go in
[Issues](https://github.com/IZZY-Agency/claude-tg/issues). Pull requests are
welcome: accepted changes are applied to the private tree and credited here in
the changelog.

## License

[MIT](LICENSE).
