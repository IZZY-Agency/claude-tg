# Changelog

## 0.4.0

- Prompts during a turn you started from the phone come to the phone at once,
  even while you are at the Mac, until you answer at the Mac one of its
  prompts that came to the phone with buttons, or for an hour.
- MCP tool prompts get buttons.
- Readable session names. A message starts with the name you gave the
  session's agterm tab, else the project and Claude Code's title for the
  session (`nova · Login token claim failure on…`), instead of Claude Code's
  `nova-ca`. `@name` matches either; two live sessions with the same name are
  told apart in the session list (`Nova · 6a`).
- Calls Claude makes at the same time, which share one dialog ("1 of 2"),
  each get their buttons when their turn comes. Before, a second call got
  only the plain alert, and a tap on the first could find it already closed
  as answered at the Mac.
- Voice notes. Send the bot a voice note and it replies with the text and
  **Send** / **Cancel** buttons; nothing is typed until you tap Send. tg
  ships no speech recognition: set `voice_command` in the config to a
  command that prints the text of `$TG_VOICE_FILE`. The README has a local
  Whisper recipe. Notes up to 5 minutes, one language per note.
- `tg transcribe <file>` runs `voice_command` on a local file and prints the
  transcript, for setting it up; it uses the relay's PATH, so a command that
  works there works in the relay. `tg setup --check` prints `voice:
  configured` or `voice: not configured (see the README)`; `tg setup` keeps
  `voice_command`.
- Spoken answers. When you ask by voice (a voice note, or a prompt answered
  by voice), the answer comes back as a voice message first, then the text
  as before: a short answer whole, a long one's opening paragraph (with the
  next paragraph when the first is a short lead-in such as "Sure, here it
  is." or ends in a colon); code and tables stay text. The voice speaks the
  answer's language, or your question's when the answer has too few letters
  to tell ("2 + 2 = 4."). Quotes, slashes and times are rewritten so the
  voice does not read them out ("відкриті лапки", "slash"). tg ships no
  speech engine: set `speech_command` in the config to a command that
  writes an OGG/Opus file to `$TG_SPEECH_OUT` from the text in
  `$TG_SPEECH_FILE`. The README has a macOS recipe with `say` and `ffmpeg`.
  Unset, nothing changes.
- `tg speak <text>` (or `tg speak -`) runs `speech_command` and sends the
  result as a voice message, for setting it up. `tg setup --check` prints
  `speech: configured` or `speech: not configured (see the README)`; `tg
  setup` keeps `speech_command`.
- Choosing spoken answers. With a TypeSafe key (`tg setup --typesafe-key`,
  or the prompt in `tg setup`), the answer to a turn you started from the
  phone is spoken when TypeSafe's model judges it factual and pleasant to
  hear; code is never sent and never spoken. Answers to questions asked by
  voice are never sent to TypeSafe: they are spoken on the hard rules
  alone. Of a long answer, one that is mostly code or one with a table, only
  an opening paragraph of prose can be: the voice says it, then the full
  answer follows as text. Without a key, typed answers are never spoken.
  `tg setup --check` prints `classifier: configured (TypeSafe)` or
  `classifier: not configured (typed answers are never spoken)`; `tg setup`
  keeps the key.
- `tg classify <question> <answer>` (or `-` to read the answer from standard
  input) prints the hard rules' verdict, TypeSafe's two probabilities and
  the decision, sending nothing to Telegram, for setting it up.
- `tg relay --once` waits for the voice notes it received to be
  transcribed before it exits.
- `/sessions` has a button for each live session. Tap one to pick it:
  plain text and voice notes go to that session until you pick another or
  tap **✖ Clear**, or for an hour after the pick or the last message sent
  to that session. A swipe-reply or `@name` still goes where it points. The
  pick survives a relay restart; a pick whose session has ended is dropped,
  and the message gets the session keyboard with a note.
- A message delivered through the "Which session?" buttons now gets the 👍
  reaction too; before, only direct deliveries did.
- Credential redaction masks AWS keys and GitHub tokens written back to
  back as a whole; before, the second GitHub token's body stayed in clear.
- Other non-text messages (photos, stickers, files, locations, contacts,
  polls) get the reply `tg reads text and voice notes only` instead of
  silence.
- Linux: the relay's systemd unit gets `KillMode=mixed`, so a stop or
  restart signals the relay alone and the relay itself ends a running voice
  command.
- Upgrading: run `tg relay install` to reload the relay with the new binary
  (on Linux it also rewrites the unit); until then a running 0.3.0 relay
  ignores voice notes.

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
