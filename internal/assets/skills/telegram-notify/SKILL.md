---
name: telegram-notify
description: Send the user a message on their phone through the `tg` CLI (private Telegram bot). Use when the user says notify me, send to Telegram, report to Telegram, monitor X and report, follow this on Telegram, stop following, ping me when done, or when a genuinely critical event happens while the user may be away (deploy or production failure, data-loss risk, a decision only they can take). Never call the Telegram API directly; always go through `tg`.
---

# telegram-notify

`tg` posts one-way messages from this session to the user's private Telegram chat. Every message is prefixed with this session's name, so the user knows which project is talking. With the relay installed (`tg relay install`) the user can answer from the phone; see "Replies from the phone" below.

Run every `tg` command on its own line, never chained with `&&`, `;` or `|`: the user's permission rules pre-approve `tg send`, `tg follow`, `tg sessions` and `tg test` only when they are the whole command. If `tg` is not installed the shell says `command not found`; then tell the user and do nothing else.

## 1. When to send on your own (the user did not ask)

Send, once, when one of these happens and the user is probably not watching the terminal:

- A production or deploy step failed, or a release is half applied.
- A destructive or irreversible step is next and needs a decision only the user can take.
- A long unattended task (more than about 10 minutes) finished or failed.
- You are blocked on something the hooks cannot see, such as waiting for an external party.

Do not send for routine progress, for "task done" after a short task, for anything the user is watching live, or to repeat something you already sent. If in doubt, do not send. The hooks already alert the user when you wait on a permission prompt; do not send a message about that yourself.

## 2. When the user asks

- "Notify me when X" / "ping me when done": run `tg send` at that moment with the outcome, not before.
- "Monitor X and report": poll with the project's own tools on the cadence the user named (or every 10 minutes if none). Send only when something changed, or on the cadence they asked for, whichever they said. Keep each report under about 400 characters and include the numbers and identifiers that let them act. Use `--level info` for routine reports and the default level for problems.
- "Follow this on Telegram": run `tg follow on`, then tell the user it is on. From then on the hooks post your final answer after every turn; you do not need to send it yourself. "Stop following": run `tg follow off` and confirm.

## 2b. Replies from the phone

The user can answer from Telegram. A reply arrives in this session as an ordinary typed prompt, exactly as if the user had typed it here; treat it like any other message. When you ask a question or wait on a permission prompt while the user is away, and the session runs in agterm or tmux, the phone usually shows buttons and one tap answers it (prompts of MCP tools, plan approval and subagents get a plain alert instead, with no buttons). The tap or the typed reply reaches you as the dialog's own answer: a denial from the phone says so in its message, and a reply to a permission message arrives as the reason for the denial. The user can also approve or deny with /approve and /deny, and switch follow mode with /follow and /unfollow. While a question or permission prompt is open, text from the phone to this session is refused rather than typed. Nothing on your side is needed for any of this.

## 3. How to write a message

- Lead with what the user would act on: "deploy to prod failed: migrate step exited 1, site still on old release" beats "task finished with errors".
- One event per message. Include ids, hostnames, URLs. Never include secrets, tokens, passwords, or full logs; summarise instead.
- `--title "..."` adds a short bold subject. Plain text is fine; `**bold**`, `` `code` `` and fenced blocks are rendered.
- Default level makes the phone buzz. `--level info` is silent; use it for routine report lines from a loop.

## 4. Commands

```bash
tg send "deploy to prod failed: migrate step exited 1, site still on old release"
tg send --level info --title "claims" "12 pending, 3 failed since 14:00 (ids 881, 884, 890)"
echo "multi-line report" | tg send -          # body from stdin
tg follow on          # live feed of this session's answers
tg follow off
tg follow status
tg sessions           # what is followed, which sessions are alive
```

Exit codes: 0 sent; 2 not configured (tell the user to run `tg setup`); 3 Telegram refused the message (fix the text, do not retry blindly); 4 network failure (the message is spooled and sent with the next call; no action needed).

`tg send`, `tg follow`, `tg sessions` and `tg test` are pre-approved in the user's permissions, so expect no permission prompt for them.

## 5. Do not

- Call the Telegram API, read `~/.config/claude-tg/`, or print the bot token.
- Send more than one message per minute from a loop unless the user asked for that cadence.
- Send from a subagent on behalf of the main session; let the main session decide.
- Edit the hooks or the skill files; `tg setup` and `tg remove` own them.
- Chain `tg` with other commands (no `cd` first, no `;`, no pipes); run it alone so it never waits on a permission prompt.
