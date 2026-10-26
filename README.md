<p align="center">
  <a href="https://github.com/IZZY-Agency/claude-tg/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/IZZY-Agency/claude-tg/ci.yml?branch=main&amp;label=CI" alt="CI"></a>
  <a href="https://github.com/IZZY-Agency/claude-tg/releases/latest"><img src="https://img.shields.io/github/v/release/IZZY-Agency/claude-tg?label=release&amp;color=f5c518" alt="Latest release"></a>
  <a href="https://github.com/IZZY-Agency/homebrew-tap"><img src="https://img.shields.io/badge/brew-izzy--agency%2Ftap%2Fclaude--tg-f5c518" alt="Homebrew formula"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/IZZY-Agency/claude-tg" alt="MIT license"></a>
</p>

# claude-tg

## What it does

`tg` is a small command-line tool for a Mac or a Linux machine that runs Claude Code. It posts messages from your Claude Code sessions to a private Telegram bot chat on your phone: an alert when a session waits on a permission prompt or stops on an API error, every answer of a session you chose to follow, and messages Claude sends because it decided something is worth telling you. With the relay installed you can answer from the phone: your text is typed into the right session, and you can answer a question or a permission prompt with one tap on a button. `tg setup` installs everything into Claude Code at user scope: a skill, six hooks and four allow rules. `tg remove` takes it out again.

[Releases](https://github.com/IZZY-Agency/claude-tg/releases) ·
[Issues](https://github.com/IZZY-Agency/claude-tg/issues)

## Requirements

- macOS 13 Ventura or later, Apple Silicon or Intel. The release builds are one per architecture (arm64 and x86_64), built with Go 1.27, which requires macOS 13.
- Or Linux, x86_64 or arm64, with these limits: the relay runs as a systemd user service; tmux is the only terminal backend; the away gate relies on logind's idle hint, which desktop environments set. Where no desktop session exists (ssh, tty, containers) or `loginctl` is missing, every alert is delivered; set `away_after_seconds` to `0` in `~/.config/claude-tg/config.json` to force that anywhere.
- Claude Code installed, with the `claude` command on PATH. `tg` runs `claude agents --json` to learn session names.
- A Telegram account on your phone.
- Only for replies and buttons on the phone: the Claude session must run inside agterm (macOS) or tmux (3.1 or newer), and the relay must be installed (`tg relay install`). Nothing else needs either tool.

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

**Upgrading from 0.2.0.** Run `tg setup`: it moves the `PermissionRequest` hook to the new synchronous `permission-wait` hook. Run `tg relay install` to reload the relay with the new binary; a relay from 0.2.0 that is still running does not take button taps, so until you reload it every prompt gets the plain 🔐 alert. Then restart Claude sessions that were already open; they keep the old hook until you do, and send the plain 🔐 alert without buttons. `tg setup --check` reports an old hook as outdated; it does not check the relay, so run `tg relay install` yourself.

## What reaches the phone

| Icon | When | Sound |
| --- | --- | --- |
| 🔔 | `tg send "..."` (default level): a Claude decided something is critical, or you asked to be pinged | yes |
| 📋 | `tg send --level info "..."`: routine lines from a monitoring loop | silent |
| 💬 | follow mode: every final answer of a followed session, split into up to 4 parts | silent |
| 🔐 | a session waits on a permission prompt, naming the tool and command. Where buttons are possible (see Questions and permission prompts) it carries buttons and arrives once you count as away, or at once during a turn you started from the phone. Otherwise (no relay, a session opened before the upgrade, a prompt that gets no buttons) it is the plain alert, about 6 s after the prompt | yes |
| ✋ | a session asks you a question, with one button per option (where buttons are possible); also a background agent that needs input, as a plain alert | yes |
| 💥 | a session stopped on an API error (rate limit, auth, billing) | yes |
| ⚙️ | follow on/off confirmations, test messages | silent |

Every message starts with the session's name, then an optional bold title,
then the body rendered from Claude's markdown: bold, italics, code, fenced
blocks, lists, links, quotes, tables in monospace.

The name is the one you gave the session's agterm tab (`Shop`), else the
project and Claude Code's title for the session (`acme-shop · Fix the
checkout bug`, cut to 30 characters), else Claude Code's own name
(`acme-shop-92`). A tab you did not name shows Claude Code's title with a
status mark in front (`✳ …`); tg does not take that for a name. When two live
sessions share a name, the session list and its buttons add the end of
Claude Code's name (`Shop · 92`). Renaming a tab shows from the session's
next message. `@name` matches the shown name, or Claude Code's name, which
keeps working. When what you write after `@` also goes on like another
session's longer name (`@Shop staging…` beside `Shop` and `Shop staging`),
tg asks which session instead of guessing. A credential in a name shows as
`•••`.

Hook-generated alerts (🔐 ✋ 💥) are sent only when you have been away from
the machine for `away_after_seconds` (default 120; `0` disables the gate),
unless the session is working on something you sent from the phone: then they
come at once, even while you are at the Mac, until you answer at the Mac one of
its prompts that came to the phone with buttons, or for an hour after you sent
it. Either way, an alert goes out only if the same alert was not sent in the
last 10 minutes. Questions and permission prompts with buttons: each one is
its own message and is never deduplicated. A dialog that opens while you are
at the machine, in a turn you started there, reaches the phone once you have
been idle for `away_after_seconds`. macOS reports the idle time itself; on
Linux it comes from logind's idle hint (see Requirements). Your own `tg send`
calls and the follow feed are never held back.

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
tg setup [--token t] [--chat-id id] [--typesafe-key k] [--yes] [--check] [--no-claude-md] [--force]
tg transcribe <file>                       # run voice_command on a file, print the text
tg speak <text | ->                        # run speech_command on the text, send it as a voice message
tg classify <question> <answer | ->        # would TypeSafe have this answer spoken? sends nothing to Telegram
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
debugging; it waits for the voice notes among them to be transcribed before
it exits, and Ctrl-C stops them. The service's own output goes to `~/.local/state/claude-tg/relay.out.log`.

`tg relay install`, `tg relay status` and `tg setup --check` print which backends they found, for example `terminal backends: agterm, tmux` (`none` when there is neither). They look in the relay's own PATH (`~/.local/bin`, `/opt/homebrew/bin`, `/home/linuxbrew/.linuxbrew/bin`, `/usr/local/bin`, `/usr/bin`, `/bin`), not your shell's, so the report matches what the relay will see. When neither tool is there the relay still installs but warns:

```
warning: neither agtermctl nor tmux is on PATH; replies and /approve will be refused, /sessions, /follow and /status still work
```

Addressing a session, in order:

1. Swipe-reply to a message that session sent. A reply to a message no
   session is known to have sent is refused, never guessed.
2. Start with `@name`; a unique prefix is enough (`@acme`). `name:` works with
   the exact session name only.
3. The session you picked. `/sessions` lists the live sessions with a button
   for each; tap one to pick it. Plain text and voice notes then go to that
   session until you pick another or tap **✖ Clear** under `/sessions`, or
   until an hour has passed since the pick or the last message sent to that
   session, by any route. A swipe-reply or `@name` still goes where it points, for that message
   only; the pick stays. The pick survives a relay restart. When the picked
   session has ended, the pick is dropped and the message gets the keyboard
   of live sessions, with a note that the session ended.
4. With no address and no pick, the single followed session gets the text.
5. Otherwise the bot shows a keyboard of live sessions; tap one.

Text is typed into the session's pane in two steps: the text itself (with a
leading space, so a leading digit can never pick a dialog option), a second
look at the screen, then Enter. If a permission dialog appeared in between,
Enter is withheld and you are told to resend. Newlines become spaces, 4000
characters max. When the session is in the middle of a turn the text is queued
and you are told so.

### Questions and permission prompts

When a session asks a question or waits on a permission prompt and you have
been away for `away_after_seconds`, or the session is working on something you
sent from the phone, the phone gets a message with buttons.
One tap answers it. The dialog stays live on the Mac, and whichever side
answers first wins. The phone message then says what happened: `answered at
the Mac` (you answered, or pressed Esc), `session ended` (Claude was quit), or
`expired: answer at the Mac` after 12 hours. If you answered on the phone but
the answer could not reach Claude, the message says `not delivered: answer at
the Mac`.

- **Question.** One message per question, one button per option. Tap an option
  to answer. Tap **✍️ Other**, then reply to the message with your own text.
  For a multi-select question, tap options to tick them, then tap **Done**.
  A swipe-reply with text to a question message answers it as well. A call
  with several questions sends one message each, and Claude gets the answers
  once every question has one.
- **Permission prompt.** **✅ Yes** and **❌ No**. If Claude Code offers to
  allow the command or a folder for good or for the session, the message lists
  each choice in full, numbered, and a short button (`① Yes, always`, `② Yes,
  this session`) carries the same number. The "switch to auto mode" choice is
  not offered. **❌ No** stops Claude, like Esc at the Mac. Reply to the message
  with text to deny the prompt and send your text to Claude, so it can adjust.

Buttons need all of this: the relay from this version is running, the session
runs in agterm or tmux, and the hook can read that pane and finds the dialog of
its own call on it within 3 seconds. Calls Claude makes at the same time (MCP
lookups often are) share one dialog that shows them one at a time, with "1 of
2" at its top right; each gets its own message with buttons when its turn
comes. The hook reads the pane again while the message is out to see the
dialog close at the Mac. Buttons come for questions
and for Bash, Read, Edit, Write, WebFetch and MCP tool prompts.
These keep the plain alert, with no buttons: an MCP prompt tg cannot tie to
its call (no plain one-line text among its arguments), plan approval, prompts
raised inside a subagent, a session in
another terminal, a dialog whose text tg cannot match to the call (a very long
command, a call queued behind a dialog taller than the pane), a question call whose questions
share one text, the sandbox network-request prompt and background agents. Non-interactive runs (`claude -p`, the Agent SDK) have no
dialog, so nothing is sent for them. tg does not cover MCP elicitation
dialogs: they get no alert at all.

While a session has a question or permission prompt open, text sent to it is
never typed. Once its buttons are on the phone, you get a pointer to them.
Before that, you are told they will arrive shortly, or to answer at the Mac. A
dialog on the pane that has no buttons (a plan approval, a question picker, any
prompt without them) is refused with "answer it on the Mac"; only beside the
plain "Do you want to proceed?" dialog of a Bash, Read or MCP prompt does the
refusal offer `/approve` and `/deny`.

`/approve` and `/deny` decide a prompt that has buttons on the phone like its
✅ Yes and ❌ No, and type nothing: as a reply to the prompt's message, exactly
that prompt (one no longer open is refused); with `@name`, or for the single
followed session, the newest when several wait (the reply names which). On a
question they point you to its buttons. When the session has no prompt on the
phone, they type into the plain "Do you want to proceed?" dialog of a Bash,
Read or MCP prompt, and only there: `1` for `/approve`, Esc for `/deny`, after
two reads of the screen that show the same dialog. Anything else (an Edit,
Write or WebFetch dialog without buttons, a dialog too tall for its top to be
read, a plan approval, a question picker) is answered at the Mac.

After a reply or an approval from the phone, that session's next answer comes
back to the phone even when it is not followed (two answers when it was busy).

Commands: `/sessions`, `/follow name`, `/unfollow name`, `/approve` and
`/deny`, `/status`, `/help`. Feedback: 👀 voice note received, 👍 delivered,
👌 approved, 👎 denied (Telegram allows a fixed set of reaction emojis),
otherwise a reply with the reason. Only your Telegram id in your chat is
honoured; everyone else is dropped silently. Each update is acknowledged
before it is acted on, so a crash never replays a keystroke. Messages older
than 10 minutes (the startup backlog, or what piled up while the Mac slept)
are skipped with one notice; button taps are never skipped.

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

- The relay runs as a systemd user service, `~/.config/systemd/user/claude-tg-relay.service`; `tg relay install` writes it, reloads the user manager, enables it and (re)starts it. Its output goes to `~/.local/state/claude-tg/relay.out.log`, not the journal. The unit has `KillMode=mixed`: a stop or restart signals the relay alone, and the relay itself ends any voice command it is running.
- On a headless machine run `loginctl enable-linger $USER` once, otherwise the user manager and the relay stop when your last session ends. Over a bare ssh login `systemctl --user` may report `Failed to connect to bus`; `tg relay status` then shows `unknown (...)`.
- tmux is the only terminal backend on Linux (agterm is a macOS terminal). The relay looks for `tmux` in `~/.local/bin`, `/opt/homebrew/bin`, `/home/linuxbrew/.linuxbrew/bin`, `/usr/local/bin`, `/usr/bin` and `/bin`.
- The away gate uses logind's idle hint, which desktop environments set after their own idle timeout, so hook alerts in a turn started at the Mac arrive after that timeout plus `away_after_seconds`. Where no desktop session exists (ssh, tty, containers) or `loginctl` is missing, every alert is delivered.

Two messages mean the relay could not type:

- `<name> runs in a terminal I cannot type into (replies need agterm or tmux)`: no pane is known for that session. Either it runs in neither agterm nor tmux, or no hook has run in it yet. Start Claude inside tmux or agterm, or let the session finish one turn and send again.
- `<name>'s <backend> pane <target> is not reachable (is <tool> installed and the pane still open?)`: a pane was recorded but the relay cannot reach it. Check that `tmux` (or `agtermctl`) is installed in a directory on the relay's PATH, which is `~/.local/bin`, `/opt/homebrew/bin`, `/home/linuxbrew/.linuxbrew/bin`, `/usr/local/bin`, `/usr/bin` and `/bin`, and that the pane was not closed.

## Voice notes

Send a voice note to the bot and tg turns it into text, shows you the text, and types it only after you tap **✅ Send**. tg ships no speech recognition: you give it a command that prints the text of an audio file, and tg runs that command on each note.

Set it up on a Mac with Whisper running locally.

```sh
brew install whisper-cpp ffmpeg
mkdir -p ~/.local/share/whisper
curl -L -o ~/.local/share/whisper/ggml-large-v3-turbo-q5_0.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo-q5_0.bin
```

The model is 547 MiB. On Linux install `ffmpeg` from your distribution, and get whisper.cpp with Homebrew on Linux (the formula is `whisper-cpp`) or build it from source. The command needs `whisper-cli` and `ffmpeg` in a directory on the relay's PATH, which is `~/.local/bin`, `/opt/homebrew/bin`, `/home/linuxbrew/.linuxbrew/bin`, `/usr/local/bin`, `/usr/bin` and `/bin`. Then add this line inside the top-level object of `~/.config/claude-tg/config.json`, with a comma between it and its neighbours:

```json
"voice_command": "ffmpeg -loglevel error -i \"$TG_VOICE_FILE\" -ar 16000 -ac 1 \"$TG_VOICE_DIR/v.wav\" && whisper-cli -m ~/.local/share/whisper/ggml-large-v3-turbo-q5_0.bin -l auto -nt -np -f \"$TG_VOICE_DIR/v.wav\""
```

`whisper-cli` cannot read Telegram's voice format (Opus in OGG), so `ffmpeg` converts the note first. Once the config is saved, test the command on any audio file, then reload the relay:

```sh
tg transcribe some-note.ogg
tg relay install
```

`tg transcribe <file>` runs `voice_command` on a local file and prints the transcript. It runs the command with the relay's PATH, not your shell's, so a command that works here works in the relay. Ctrl-C stops the command and removes the copy of the file. `tg setup --check` prints `voice: configured` or `voice: not configured (see the README)`, and `tg setup` keeps `voice_command` when you rerun it.

On an Apple M5 Pro, a 7 second note took about 0.7 seconds and a 53 second note about 1.9 seconds. The first note after a boot is slower, because the model has to load.

Any command that prints the text works. tg runs it with `/bin/sh -c`, so a pipeline is fine. Standard input is not used. The note is in `$TG_VOICE_FILE`, and `$TG_VOICE_DIR` is a fresh private directory that holds it and is yours for scratch files. Exit status 0 and some output count as success; tg collapses whitespace in the output and uses it as the transcript.

What you see on the phone:

1. 👀 appears on your note at once.
2. tg replies with `🎙 to <session>:` and the text, with **✅ Send** and **✖️ Cancel**, and a second row, **↪ Other session**, to pick a different one. For a swipe-reply to a question or permission message it says `🎙 answer to <session>:` and Send answers that prompt like a typed reply would. With no target it says `🎙 Which session?` and you tap a session; that tap sends.
3. After Send the message becomes `→ <session>` with the text (`→ <session> · queued (busy)` when the session is busy), and the note gets 👍. If the delivery is refused (a dialog is open, the pane is unreachable), the message says why and keeps its buttons, so you can tap Send again; when the reason and the text would not fit one message, the reason comes as a separate reply. An answer to a question or permission prompt is not retried: if the prompt was closed or answered meanwhile, the message says why, the buttons go and the reaction is cleared. If the text was typed but the final Enter failed, it may have been sent: the message says so and to check the session, the buttons go and the reaction is cleared, so it can never be sent twice.
4. **Cancel** edits the message to `✖️ not sent` and clears the reaction. A confirmation expires 10 minutes after the text arrived, and tapping Send again after a refusal does not extend that. A tap after that answers `expired, send it again`: the buttons go and the reaction is cleared, and the text stays on the message so you can copy it. A confirmation you never tap just stays as it is.

Nothing is typed before you tap. You cannot edit the text on the phone: cancel and type it, or record the note again.

Addressing works as for typed text, applied to the transcript. Whisper does not write the `@`, so by voice you address a session by swipe-replying to one of its messages, by picking it in `/sessions`, by having a single followed session, or by tapping a button.

Limits:

- A note longer than 5 minutes is refused before it is downloaded, and a file over 20 MB is refused too (a bot cannot download more).
- The command is stopped after 3 minutes. Output over 64 KiB counts as a failure.
- A transcript over 4000 characters is refused before any button, with its first 300 characters shown, so what you confirm is always what gets typed. Characters are counted the way Telegram counts them, so most emoji count as two. A session name longer than 64 characters is shortened in the confirmation. Together these keep every confirmation within one Telegram message.
- One note is transcribed at a time; up to 16 wait their turn.
- Whisper picks one language per note. A note that switches language half way garbles the later part.

When something goes wrong you get a reply instead of silence: `could not transcribe:` followed by the reason (`timed out after 3 min`, `output too long`, `nothing recognized`, the last line the command wrote to stderr, or `exit status N`), `could not download the voice note:`, or `voice notes need voice_command in tg's config; see the README` when it is not set. Other non-text messages (photos, stickers, files, locations, contacts, polls) get `tg reads text and voice notes only`.

Privacy: with the recipe above the audio never leaves your machine. tg downloads the note to a private temporary directory and deletes it as soon as the command finishes, whatever the outcome; if the relay is killed before it can (a crash, a power loss), it removes such leftovers older than 10 minutes the next time it starts. The bot token and the message text are never put in the command line or its environment. The command line comes only from your own `config.json`. If you point `voice_command` at a cloud service, the audio goes where that command sends it.

### Spoken answers

tg can also answer a voice note with a voice message. When you ask by voice (a voice note, or a prompt you answered by voice), you first get the answer spoken, then the same text as always: a short answer whole, a long one's opening paragraph. When that paragraph is only a short lead-in ("Sure, here it is in Ukrainian.") or ends in a colon, the paragraph after it is spoken too. Everything you typed comes back as text only, unless you set up [Choosing spoken answers](#choosing-spoken-answers). Only prose is spoken: code blocks, tables, long paths and links stay in the text. If the speech command or the voice upload fails or is slow (tg waits 30 seconds at most for both together), you just get the text.

tg ships no speech engine either: you give it a command that turns text into an OGG/Opus file. Set it up on a Mac with the built-in `say` and `ffmpeg` (`brew install ffmpeg`). Add this line inside the top-level object of `~/.config/claude-tg/config.json`, with a comma between it and its neighbours:

```json
"speech_command": "case \"$TG_SPEECH_LANG\" in ru) v=Milena;; uk) v=Lesya;; fr) v=Thomas;; *) v=Samantha;; esac; say -v \"$v\" -f \"$TG_SPEECH_FILE\" -o \"$TG_SPEECH_DIR/s.aiff\" && ffmpeg -loglevel error -i \"$TG_SPEECH_DIR/s.aiff\" -c:a libopus -b:a 32k -ac 1 \"$TG_SPEECH_OUT\""
```

Then try it and reload the relay:

```sh
tg speak "Привет"
tg relay install
```

`tg speak <text>` (or `tg speak -` to read the text from standard input) runs `speech_command` with the relay's PATH and sends the result to your chat as a voice message, so you can hear what the relay would send. It prints `sent a 0.7 s voice message`, with the length of the audio. Ctrl-C, or closing the terminal, stops the command. `tg setup --check` prints `speech: configured` or `speech: not configured (see the README)`, and `tg setup` keeps `speech_command` when you rerun it.

The recipe uses the voices Milena (Russian), Lesya (Ukrainian), Thomas (French) and Samantha (English). Better-sounding voices can be downloaded in System Settings → Accessibility → Spoken Content; name the one you like in the `case`. On Linux any command that meets the contract below works; tg gives no exact recipe.

Better voices: [Piper](https://github.com/OHF-Voice/piper1-gpl) is a free neural speech engine that runs on your Mac, so nothing leaves it, and it sounds much less robotic than `say`. It needs Python 3.11 or newer (the `python3` that comes with macOS is 3.9: use Homebrew's, which may be named `python3.14`, or [uv](https://docs.astral.sh/uv/)) and about 450 MB for four voices. Install it under a short path such as `~/.local/share/piper`: in a deeply nested folder its macOS build cannot find its pronunciation data and fails with `Error processing file …/phontab`.

```sh
python3.14 -m venv ~/.local/share/piper/venv    # or: uv venv --python 3.12 ~/.local/share/piper/venv
~/.local/share/piper/venv/bin/pip install piper-tts
~/.local/share/piper/venv/bin/python -m piper.download_voices --data-dir ~/.local/share/piper/voices ru_RU-dmitri-medium uk_UA-mykyta-high en_GB-alan-medium fr_FR-tom-medium
```

Then use this `speech_command` instead:

```json
"speech_command": "case \"$TG_SPEECH_LANG\" in ru) v=ru_RU-dmitri-medium;; uk) v=uk_UA-mykyta-high;; fr) v=fr_FR-tom-medium;; *) v=en_GB-alan-medium;; esac; p=\"$HOME/.local/share/piper\"; \"$p/venv/bin/python\" -m piper -m \"$v\" --data-dir \"$p/voices\" -f \"$TG_SPEECH_DIR/s.wav\" < \"$TG_SPEECH_FILE\" && ffmpeg -loglevel error -i \"$TG_SPEECH_DIR/s.wav\" -c:a libopus -b:a 32k -ac 1 \"$TG_SPEECH_OUT\""
```

Each voice takes about a second per answer. Other voices are in the [Piper voice list](https://huggingface.co/rhasspy/piper-voices), for example `en_GB-cori-high` (British), `en_GB-alba-medium` (Scottish) or `en_GB-jenny_dioco-medium` (Irish); download them the same way and name them in the `case` (a voice with several speakers also takes `-s <number>`). Piper's engine is GPL-3.0 and each voice has its own licence, given in its `MODEL_CARD`.

The contract: tg runs the command with `/bin/sh -c`, in its own process group, which is killed after 30 seconds. The text to speak is in `$TG_SPEECH_FILE` (UTF-8, also on standard input). The command must write an OGG file with an Opus stream, at most 10 MB, to `$TG_SPEECH_OUT`; anything else counts as a failure. `$TG_SPEECH_DIR` is a fresh private directory for scratch files, removed afterwards. `$TG_SPEECH_LANG` is `ru`, `uk`, `en`, `fr`, or empty when tg is unsure. It is the language of the text; for a spoken answer with fewer than three letters ("2 + 2 = 4."), the language of your question, typed or spoken. Before the command runs, tg rewrites what voices tend to read out as symbols: `«»` become `“”`, a slash between two short words (`km/h`, `and/or`) becomes a space except in French (a path such as `src/config` keeps it), and in Ukrainian the colon of a time (`19:02`) becomes a space. Standard output is ignored. The bot token is not in the command's environment.

Limits: the spoken text is at most 500 characters, which is about 30 seconds of speech. Only the first answer after the note's text was submitted to the session is spoken, and a note that gets no answer within an hour is forgotten, so a later answer is text only. In a followed session only the answer to the note is spoken, never other answers; `/unfollow` or `tg follow off` forgets a note not answered yet. While older messages are still waiting to be delivered (Telegram could not be reached), no answer is spoken: its text queues behind them as usual.

`tg.log` records `spoke N characters (D s) for <session>` or `speech: <reason>`. tg itself never logs the answer's text, but when the speech command fails the reason can be the last line it wrote to stderr (with the bot token and obvious credentials filtered out), so a command that prints its input there can leave some of it in the log.

Privacy: with the recipe above the text stays on your Mac until it goes to Telegram as a voice message. If you point `speech_command` at a cloud text-to-speech service, the text of your answers goes where that command sends it.

### Choosing spoken answers

Without more setup, only answers to questions asked by voice are spoken. With a [TypeSafe](https://typesafe.ai) key, tg also decides for answers to questions you typed from the phone: a typed "what is the weather in Paris?" comes back as a voice message, then the text, while a list of files comes back as text only. The check needs `speech_command` to be set up first.

What is considered: the answer to a line you typed from the phone, the first answer after it. An answer to a question asked by voice is spoken on the hard rules alone and never sent to TypeSafe, key or no key. A turn you started at the Mac is never spoken unless you answered one of its prompts by voice.

Hard rules come first and stay on your Mac: an answer with a code block is text only and is never sent to TypeSafe. A long answer (over 500 characters once made speakable), one that is mostly code or one with a table is never spoken whole; when it opens with a short paragraph of prose, such as a weather report's lead sentence before its figures, that paragraph alone is judged, and spoken if TypeSafe agrees; the full answer follows as text as always. An opening paragraph of eight words or fewer followed by another paragraph of prose is taken with that paragraph, so a lead-in ("Sure, here it is.") is not all you hear. A lead that ends in a colon ("Here's the current weather in Paris:") is spoken only with a paragraph of prose right after it, never alone or before a list. Otherwise it is text only and is never sent to TypeSafe.

What is sent to TypeSafe, and when: only for an answer that passed the hard rules, the phone prompt and the answer (or only its opening paragraph), both cleaned the way Telegram messages are (the bot token scrubbed, obvious credentials redacted), to `api.typesafe.ai` with the model `jev-1.14.0`. It asks two questions: is the answer factual, and does it read well aloud. Only if both come back 0.7 or above is the answer spoken; unsure, a failure or a wait over 5 seconds means text only. TypeSafe says it does not train on customer data; how long it keeps requests is in its [data processing agreement](https://typesafe.ai/legal/data-processing) and its [privacy policy](https://typesafe.ai/legal/privacy-policy).

Cost: TypeSafe bills input tokens only, and a check is a few hundred tokens, a small fraction of a cent.

Get a key at [console.typesafe.ai/keys](https://console.typesafe.ai/keys), then run `tg setup` and paste it at the prompt, or pass it: `tg setup --typesafe-key <key>` (the environment variable `TYPESAFE_API_KEY` works too). Setup checks the key without spending tokens, and prints `TypeSafe: key accepted`, `TypeSafe: key rejected; not saved`, or says it could not check it and saved it anyway. `tg setup --check` prints `classifier: configured (TypeSafe)` or `classifier: not configured (typed answers are never spoken)`. The key is stored in the config (mode 0600) and never appears in the log, in Telegram or in the environment of your commands.

Try it without sending anything to Telegram: `tg classify "<question>" "<answer>"`, or `tg classify "<question>" -` to read the answer from standard input. It prints the hard rules' verdict (`hard rules: pass (opening paragraph)` when only the opening paragraph would be judged and spoken), then `factual`, `listenable`, `model` and `decision: voice` or `decision: text`.

To turn it off, delete `typesafe_api_key` from the config: that is the reliable way. Rerunning `tg setup` does not turn it off by itself: a `TYPESAFE_API_KEY` in the environment wins over the prompt (unset it first), and `--yes` keeps an existing key. Declining to reuse the key at the prompt, then pressing Enter at the next one, also removes it. Typed answers are text again; nothing else changes.

## Privacy and security

- The bot token, and the TypeSafe key if you set one, are stored in `~/.config/claude-tg/config.json` with mode 0600, in a directory with mode 0700. `tg` never prints either, and replaces them with `[redacted]` if they ever appear in a message label, title, body or log line. Both are stripped from the environment of `voice_command` and `speech_command`.
- The relay acts only on messages from your own Telegram user id in your own chat, both recorded at setup. Everything else is dropped silently.
- What leaves the machine goes to Telegram's Bot API, and, only if you set a TypeSafe key, the phone's prompt and a short answer also go to `api.typesafe.ai` (the last bullet below):
  - the session name at the top of every message: your agterm tab name, or the project folder and Claude Code's title for the session, or the name Claude Code shows, or `<folder>-<first 6 characters of the session id>` when none of these can be read;
  - the final answer text of a followed session, and the next answer of a session after you replied to it or approved a prompt from the phone;
  - the title and body Claude passes to `tg send`;
  - for a question: its header, the question text, its options with their descriptions and previews, and the answer you gave; for a permission alert: the tool name and a one-line summary, which is the command for `Bash`, the file path for `Edit`, `Write`, `Read`, `MultiEdit` and `NotebookEdit`, and the raw tool input for other tools, cut to 120 characters; with buttons, also the allow rules and folders Claude Code offers to remember, and the text of a denial you typed;
  - for a stopped session: the error and its details as Claude Code reported them;
  - for a background agent that needs input: the notification text;
  - for a voice note: the transcript, shown in the confirmation and its outcome, and when the command fails, the last line it wrote to stderr (or its exit status);
  - for a spoken answer: its audio, as a voice message captioned `🔊 <session name>`. The text it is made from goes to your `speech_command`, which with the recipe above keeps it on the machine; a cloud speech engine would send it where that command sends it;
  - on `/sessions` from the phone: session names, busy or idle, follow state, and the last folder name of each session's working directory;
  - once, at setup: `tg setup complete on <hostname>`;
  - with a TypeSafe key, to `api.typesafe.ai` and not to Telegram: for an answer to a turn from the phone that passed the hard rules, the phone prompt (cut to 1000 characters) and the answer or only its opening paragraph, both cleaned as above, and once at setup a request that only checks the key. See "Choosing spoken answers".
- Nothing is sent unless a hook event fires, Claude runs `tg send` or `tg follow`, or you run `tg` yourself. Hook alerts also wait until you have been away from the machine for `away_after_seconds`, except during a turn you started from the phone.
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
