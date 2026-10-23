# Security policy

`tg` sends text from your Claude Code sessions to Telegram's Bot API and, with
the relay installed, types what you send from the phone into those sessions,
so a flaw in it matters. The README's "Privacy and security" section lists
exactly what leaves the machine: the session name, the answers of a followed
session, what Claude passes to `tg send`, the tool name and a one-line summary
of a permission prompt, the error of a stopped session, and the session list
on `/sessions`. Please report vulnerabilities privately.

## Reporting

Use GitHub's private vulnerability reporting:
[Report a vulnerability](https://github.com/IZZY-Agency/claude-tg/security/advisories/new).
Do not open a public issue for a security problem.

Include the version (`tg --version`), the platform (macOS or Linux), what you
found, and how to reproduce it. We acknowledge reports within a few working
days.

## Scope

In scope: the code in this repository, which is the `tg` command, the hooks
and the skill it installs into Claude Code, the relay, what they send to
Telegram, and what the relay types into a terminal.

Telegram and Claude Code themselves are not in this repository.

## Supported versions

Only the latest release receives fixes.
