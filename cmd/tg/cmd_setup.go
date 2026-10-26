package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/assets"
	"github.com/IZZY-Agency/claude-tg/internal/classify"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/settings"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

func init() {
	register("setup", "configure the bot and install the skill, hooks and allow rules: tg setup [--token t] [--chat-id id] [--typesafe-key k] [--yes] [--check] [--no-claude-md] [--force]", cmdSetup)
}

// claudeHomeDir is ~/.claude unless TG_CLAUDE_HOME (tests) or CLAUDE_CONFIG_DIR says otherwise.
func claudeHomeDir(c *cli) string {
	if p := c.env("TG_CLAUDE_HOME"); p != "" {
		return p
	}
	if p := c.env("CLAUDE_CONFIG_DIR"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

type paths struct {
	config     string
	claudeHome string
	settings   string
	skills     string
	claudeMD   string
	channelEnv string
	accessJSON string
}

func resolvePaths(c *cli) paths {
	home := claudeHomeDir(c)
	return paths{
		config:     configPath(c),
		claudeHome: home,
		settings:   filepath.Join(home, "settings.json"),
		skills:     filepath.Join(home, "skills"),
		claudeMD:   filepath.Join(home, "CLAUDE.md"),
		channelEnv: filepath.Join(home, "channels", "telegram", ".env"),
		accessJSON: filepath.Join(home, "channels", "telegram", "access.json"),
	}
}

// resolveToken picks the bot token: flag, then env, then existing config,
// then the official channel plugin's .env file. The source names where it came from.
func resolveToken(flagTok, envTok, cfgTok, channelEnvPath string) (string, string) {
	switch {
	case flagTok != "":
		return flagTok, "flag"
	case envTok != "":
		return envTok, "TELEGRAM_BOT_TOKEN"
	case cfgTok != "":
		return cfgTok, "existing config"
	}
	b, err := os.ReadFile(channelEnvPath)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "TELEGRAM_BOT_TOKEN="); ok {
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if v != "" {
				return v, channelEnvPath
			}
		}
	}
	return "", ""
}

func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func ask(c *cli, question string, defaultYes bool) bool {
	suffix := " [Y/n] "
	if !defaultYes {
		suffix = " [y/N] "
	}
	fmt.Fprint(c.stdout, question+suffix)
	line, _ := bufio.NewReader(c.stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return defaultYes
	}
	return line == "y" || line == "yes"
}

func binaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "tg"
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return stableBinaryPath(exe)
}

var cellarRe = regexp.MustCompile(`^(.*)/Cellar/([^/]+)/[^/]+/bin/([^/]+)$`)

// stableBinaryPath maps a Homebrew Cellar path (versioned, removed on
// upgrade) to the prefix's opt path, which Homebrew keeps pointing at the
// current version, so hooks and the relay survive `brew upgrade`.
func stableBinaryPath(exe string) string {
	m := cellarRe.FindStringSubmatch(exe)
	if m == nil {
		return exe
	}
	opt := filepath.Join(m[1], "opt", m[2], "bin", m[3])
	// Only when the opt entry really is this executable: a dangling link, a
	// directory or another program there must not become the hook path.
	optInfo, err := os.Stat(opt)
	if err != nil || optInfo.IsDir() {
		return exe
	}
	exeInfo, err := os.Stat(exe)
	if err != nil || !os.SameFile(optInfo, exeInfo) {
		return exe
	}
	return opt
}

// findClaude returns the launcher path as found on PATH. It deliberately
// does not resolve symlinks: ~/.local/bin/claude points into a versioned
// directory that moves on every update.
func findClaude() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "claude")
}

func allowlistedID(accessJSON string) int64 {
	b, err := os.ReadFile(accessJSON)
	if err != nil {
		return 0
	}
	var acc struct {
		AllowFrom []string `json:"allowFrom"`
	}
	if json.Unmarshal(b, &acc) != nil || len(acc.AllowFrom) == 0 {
		return 0
	}
	id, _ := strconv.ParseInt(acc.AllowFrom[0], 10, 64)
	return id
}

func cmdSetup(c *cli, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	tokenFlag := fs.String("token", "", "bot token from @BotFather")
	chatFlag := fs.Int64("chat-id", 0, "Telegram chat id (skips discovery)")
	keyFlag := fs.String("typesafe-key", "", "TypeSafe API key for choosing spoken answers")
	yes := fs.Bool("yes", false, "assume yes to every question")
	check := fs.Bool("check", false, "report what is installed; change nothing")
	noClaudeMD := fs.Bool("no-claude-md", false, "do not touch ~/.claude/CLAUDE.md")
	force := fs.Bool("force", false, "overwrite an unmanaged skill directory")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	p := resolvePaths(c)
	existing, err := config.Load(p.config)
	if err != nil && !errors.Is(err, config.ErrNotConfigured) {
		fmt.Fprintf(c.stderr, "tg setup: %v\n", err)
		return exitUsage
	}
	bin := binaryPath()
	if *check {
		return setupCheck(c, p, existing, bin)
	}

	// 1. Token.
	var cfgTok string
	if existing != nil {
		cfgTok = existing.BotToken
	}
	token, source := resolveToken(*tokenFlag, c.env("TELEGRAM_BOT_TOKEN"), cfgTok, p.channelEnv)
	tty := isTTY(c.stdin)
	if token == "" {
		if !tty {
			fmt.Fprintln(c.stderr, "tg setup: no bot token; pass --token <token> or set TELEGRAM_BOT_TOKEN")
			return exitUsage
		}
		fmt.Fprint(c.stdout, "Bot token (from @BotFather): ")
		token = readSecret(c)
		if token == "" {
			fmt.Fprintln(c.stderr, "tg setup: empty token")
			return exitUsage
		}
		source = "prompt"
	} else if source != "flag" {
		fmt.Fprintf(c.stdout, "Bot token: found in %s.\n", source)
		if tty && !*yes && !ask(c, "Reuse it?", true) {
			fmt.Fprintln(c.stderr, "tg setup: pass --token <token> to use a different one")
			return exitUsage
		}
	}

	// 2. Chat id requirement (before any network call).
	chatID := *chatFlag
	if chatID == 0 && existing != nil {
		chatID = existing.ChatID
	}
	if chatID == 0 {
		if st, err := state.Open(stateDir(c)); err == nil && st.RelayLocked() {
			fmt.Fprintln(c.stderr, "tg setup: the relay is running and owns the bot's updates; pass --chat-id <id> or stop it first (tg relay uninstall)")
			return exitUsage
		}
	}
	if chatID == 0 && !tty {
		fmt.Fprintln(c.stderr, "tg setup: no terminal to confirm the chat; pass --chat-id <id> (your Telegram user id for a private chat)")
		return exitUsage
	}

	// 3. Validate the token.
	ctx := context.Background()
	client := telegram.New(token)
	if setupTelegramBaseURL != "" {
		client.BaseURL = setupTelegramBaseURL
	}
	me, err := client.GetMe(ctx)
	if err != nil {
		fmt.Fprintf(c.stderr, "tg setup: token rejected by Telegram: %v\n", err)
		return send.Classify(err)
	}
	fmt.Fprintf(c.stdout, "Bot: @%s\n", me.Username)

	// 4. Chat discovery.
	var userID int64
	if existing != nil {
		userID = existing.UserID
	}
	if chatID == 0 {
		id, uid, code := discoverChat(c, ctx, client, me.Username, allowlistedID(p.accessJSON), *yes)
		if code != exitOK {
			return code
		}
		chatID, userID = id, uid
	}

	// 4b. TypeSafe key (optional).
	var cfgKey string
	if existing != nil {
		cfgKey = existing.TypesafeAPIKey
	}
	hasSpeech := existing != nil && existing.SpeechCommand != ""
	typesafeKey := setupTypesafeKey(c, ctx, *keyFlag, cfgKey, tty, *yes, hasSpeech)

	// 5. Config.
	cfg := &config.Config{
		BotToken: token, ChatID: chatID, UserID: userID, BotUsername: me.Username,
		ClaudePath: findClaude(), BinaryPath: bin, AwayAfterSeconds: config.DefaultAwayAfterSeconds, InstalledAt: time.Now(),
	}
	carryOver(cfg, existing)
	cfg.TypesafeAPIKey = typesafeKey
	if err := cfg.Save(p.config); err != nil {
		fmt.Fprintf(c.stderr, "tg setup: write config: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(c.stdout, "Config: %s (chat %d)\n", p.config, chatID)

	// 6. Skill.
	changed, err := assets.InstallSkill(p.skills, *force)
	if err != nil {
		fmt.Fprintf(c.stderr, "tg setup: skill: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(c.stdout, "Skill: %s (%s)\n", filepath.Join(p.skills, assets.SkillName), doneWord(changed))

	// 7. Hooks and allow rules.
	src, err := os.ReadFile(p.settings)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(c.stderr, "tg setup: read settings: %v\n", err)
		return exitUsage
	}
	var oldBinaries []string
	if existing != nil && existing.BinaryPath != "" {
		oldBinaries = append(oldBinaries, existing.BinaryPath)
	}
	out, changed, err := settings.Install(src, bin, oldBinaries...)
	if err != nil {
		fmt.Fprintf(c.stderr, "tg setup: settings.json is not valid JSON, nothing written: %v\n", err)
		return exitUsage
	}
	if changed {
		backup := ""
		if len(src) > 0 {
			backup, err = backupFile(p.settings, src)
			if err != nil {
				fmt.Fprintf(c.stderr, "tg setup: backup: %v\n", err)
				return exitUsage
			}
		}
		if err := os.MkdirAll(filepath.Dir(p.settings), 0o755); err != nil {
			fmt.Fprintf(c.stderr, "tg setup: %v\n", err)
			return exitUsage
		}
		if err := replaceFile(p.settings, out); err != nil {
			fmt.Fprintf(c.stderr, "tg setup: write settings: %v\n", err)
			return exitUsage
		}
		if backup != "" {
			fmt.Fprintf(c.stdout, "Hooks and allow rules: installed into %s (backup: %s)\n", p.settings, backup)
		} else {
			fmt.Fprintf(c.stdout, "Hooks and allow rules: installed into %s\n", p.settings)
		}
	} else {
		fmt.Fprintln(c.stdout, "Hooks and allow rules: already installed")
	}

	// 8. CLAUDE.md.
	if !*noClaudeMD {
		if *yes || !tty || ask(c, "Add a one-line rule to "+p.claudeMD+" pointing Claude at the skill?", true) {
			changed, err := assets.InstallClaudeMD(p.claudeMD)
			if err != nil {
				fmt.Fprintf(c.stderr, "tg setup: CLAUDE.md: %v\n", err)
				return exitUsage
			}
			fmt.Fprintf(c.stdout, "CLAUDE.md: %s\n", doneWord(changed))
		}
	}

	// 9. Test message.
	store, err := openStore(c)
	if err == nil {
		host, _ := os.Hostname()
		sender := &send.Sender{API: client, Store: store, ChatID: chatID, MinGap: minGap, Log: store.Logf}
		if _, err := sender.Send(ctx, send.Message{Label: "tg", Body: "tg setup complete on " + host, Silent: true, Kind: send.KindSystem}); err != nil {
			fmt.Fprintf(c.stderr, "tg setup: test message failed: %v\n", err)
			return send.Classify(err)
		}
	}
	fmt.Fprintln(c.stdout, "Sent a test message. Done.")
	return exitOK
}

// classifyBaseURL and setupTelegramBaseURL are test seams: "" means the real
// TypeSafe and Telegram endpoints.
var (
	classifyBaseURL      string
	setupTelegramBaseURL string
)

// setupTypesafeKey picks the optional TypeSafe key (flag, then
// TYPESAFE_API_KEY, then the existing config, then a prompt on a terminal),
// checks it and returns what to save: "" when there is none or it was
// rejected. The key is never printed.
func setupTypesafeKey(c *cli, ctx context.Context, flagKey, cfgKey string, tty, yes, hasSpeech bool) string {
	key := chooseTypesafeKey(strings.TrimSpace(flagKey), strings.TrimSpace(c.env("TYPESAFE_API_KEY")), cfgKey, tty, yes,
		func() bool { return ask(c, "A TypeSafe API key is already configured. Reuse it?", true) },
		func() string {
			fmt.Fprint(c.stdout, typesafePrompt)
			return readSecret(c)
		})
	if key == "" {
		return ""
	}
	err := (&classify.Client{Key: key, BaseURL: classifyBaseURL}).Check(ctx)
	switch {
	case err == nil:
		fmt.Fprintln(c.stdout, "TypeSafe: key accepted")
		unusedNote(c, hasSpeech)
		return key
	case errors.Is(err, classify.ErrRejected):
		fmt.Fprintln(c.stdout, "TypeSafe: key rejected; not saved")
		return ""
	default:
		fmt.Fprintf(c.stdout, "TypeSafe: could not check the key (%s); saved anyway\n", strings.TrimPrefix(err.Error(), "typesafe: "))
		unusedNote(c, hasSpeech)
		return key
	}
}

// unusedNote tells that a saved key does nothing until speech is configured.
func unusedNote(c *cli, hasSpeech bool) {
	if !hasSpeech {
		fmt.Fprintln(c.stdout, "TypeSafe: used once speech_command is set (see the README)")
	}
}

// chooseTypesafeKey picks the key: flag, then env, then the existing config
// key, then a prompt on a terminal. With yes nothing is asked: an existing
// key is kept and without one there is none. On a terminal an existing key
// is kept only if askReuse says so; otherwise readKey is asked for a new
// one, and an empty answer leaves none.
func chooseTypesafeKey(flagKey, envKey, cfgKey string, tty, yes bool, askReuse func() bool, readKey func() string) string {
	switch {
	case flagKey != "":
		return flagKey
	case envKey != "":
		return envKey
	case yes || !tty:
		return cfgKey
	case cfgKey != "" && askReuse():
		return cfgKey
	}
	return readKey()
}

const typesafePrompt = "TypeSafe API key for choosing spoken answers (optional, Enter to skip): "

func doneWord(changed bool) string {
	if changed {
		return "installed"
	}
	return "already installed"
}

func readSecret(c *cli) string {
	// Best effort: hide the echo while the token is typed.
	if f, ok := c.stdin.(*os.File); ok {
		off := exec.Command("/bin/stty", "-echo")
		off.Stdin = f
		_ = off.Run()
		defer func() {
			on := exec.Command("/bin/stty", "echo")
			on.Stdin = f
			_ = on.Run()
			fmt.Fprintln(c.stdout)
		}()
	}
	line, _ := bufio.NewReader(c.stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

// discoverChat waits for the user to message the bot and returns the chat id and sender id.
func discoverChat(c *cli, ctx context.Context, client *telegram.Client, botName string, preselected int64, yes bool) (int64, int64, int) {
	fmt.Fprintf(c.stdout, "Send any message to @%s in Telegram now... waiting up to 60 s\n", botName)
	deadline := time.Now().Add(60 * time.Second)
	var offset int64
	for time.Now().Before(deadline) {
		ups, err := client.GetUpdates(ctx, offset, 20)
		if err != nil {
			var api *telegram.APIError
			if errors.As(err, &api) && api.Code == 409 {
				fmt.Fprintln(c.stderr, "tg setup: Telegram says a webhook is active for this bot; remove it with deleteWebhook before running setup")
				return 0, 0, 3
			}
			fmt.Fprintf(c.stderr, "tg setup: getUpdates: %v\n", err)
			return 0, 0, send.Classify(err)
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			if u.Message == nil || u.Message.Chat.Type != "private" || u.Message.From == nil {
				continue
			}
			from := u.Message.From
			fmt.Fprintf(c.stdout, "Got a message from %s (id %d).\n", from.FirstName, from.ID)
			if preselected != 0 && preselected != from.ID {
				fmt.Fprintf(c.stdout, "Warning: the channel plugin allowlist names id %d, not %d.\n", preselected, from.ID)
			}
			if !yes && !ask(c, "Use this chat?", true) {
				continue
			}
			_, _ = client.GetUpdates(ctx, offset, 0) // acknowledge so the message is not replayed
			return u.Message.Chat.ID, from.ID, exitOK
		}
	}
	fmt.Fprintln(c.stderr, "tg setup: no message arrived; send one to the bot and run setup again, or pass --chat-id")
	return 0, 0, exitUsage
}

func setupCheck(c *cli, p paths, existing *config.Config, bin string) int {
	if existing == nil {
		fmt.Fprintf(c.stdout, "config: missing (%s)\n", p.config)
	} else {
		fmt.Fprintf(c.stdout, "config: %s (chat %d, away after %ds)\n", p.config, existing.ChatID, existing.AwayAfterSeconds)
		if existing.BinaryPath != "" && existing.BinaryPath != bin {
			fmt.Fprintf(c.stdout, "binary: config says %s but this is %s; run `tg setup` to repoint the hooks\n", existing.BinaryPath, bin)
		}
		bin = existing.BinaryPath
	}
	src, err := os.ReadFile(p.settings)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(c.stdout, "settings: unreadable: %v\n", err)
	}
	installed, missing, outdated, missingRules, err := settings.Status(src, bin)
	if err != nil {
		fmt.Fprintf(c.stdout, "settings: not valid JSON: %v\n", err)
	} else {
		fmt.Fprintf(c.stdout, "hooks: %d/%d installed in %s\n", len(installed), len(installed)+len(missing)+len(outdated), p.settings)
		for _, m := range missing {
			fmt.Fprintf(c.stdout, "  missing: %s\n", hookLabel(m))
		}
		for _, m := range outdated {
			fmt.Fprintf(c.stdout, "  outdated: %s (run `tg setup` to update it)\n", hookLabel(m))
		}
		fmt.Fprintf(c.stdout, "allow rules: %d/%d\n", len(settings.AllowRules)-len(missingRules), len(settings.AllowRules))
	}
	skillDir := filepath.Join(p.skills, assets.SkillName)
	switch {
	case fileExists(filepath.Join(skillDir, assets.MarkerFile)):
		fmt.Fprintf(c.stdout, "skill: installed (%s)\n", skillDir)
	case fileExists(skillDir):
		fmt.Fprintf(c.stdout, "skill: present but not managed by tg (%s)\n", skillDir)
	default:
		fmt.Fprintf(c.stdout, "skill: missing (%s)\n", skillDir)
	}
	if b, err := os.ReadFile(p.claudeMD); err == nil && strings.Contains(string(b), assets.StartMarker) {
		fmt.Fprintf(c.stdout, "CLAUDE.md: block present (%s)\n", p.claudeMD)
	} else {
		fmt.Fprintf(c.stdout, "CLAUDE.md: missing block (%s)\n", p.claudeMD)
	}
	if path := serviceFilePath(c); fileExists(path) {
		fmt.Fprintf(c.stdout, "relay: %s installed (%s)\n", newServiceManager(c).Kind(), path)
	} else {
		fmt.Fprintln(c.stdout, "relay: not installed (tg relay install)")
	}
	if existing != nil && existing.VoiceCommand != "" {
		fmt.Fprintln(c.stdout, "voice: configured")
	} else {
		fmt.Fprintln(c.stdout, "voice: not configured (see the README)")
	}
	if existing != nil && existing.SpeechCommand != "" {
		fmt.Fprintln(c.stdout, "speech: configured")
	} else {
		fmt.Fprintln(c.stdout, "speech: not configured (see the README)")
	}
	if existing != nil && existing.TypesafeAPIKey != "" {
		if existing.SpeechCommand == "" {
			fmt.Fprintln(c.stdout, "classifier: configured (TypeSafe), unused until speech_command is set")
		} else {
			fmt.Fprintln(c.stdout, "classifier: configured (TypeSafe)")
		}
	} else {
		fmt.Fprintln(c.stdout, "classifier: not configured (typed answers are never spoken)")
	}
	fmt.Fprintln(c.stdout, backendsLine("none (replies unavailable)"))
	if entries, err := os.ReadDir(filepath.Join(stateDir(c), "spool")); err == nil {
		n := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				n++
			}
		}
		if n > 0 {
			fmt.Fprintf(c.stdout, "spool: %d undelivered message(s)\n", n)
		}
	}
	return exitOK
}

// hookLabel names a hook as "Event" or "Event matcher".
func hookLabel(h settings.HookSpec) string {
	if h.Matcher == "" {
		return h.Event
	}
	return h.Event + " " + h.Matcher
}

// fileMode returns the permission bits of path, or 0644 when it does not exist.
func fileMode(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}

// backupFile writes src next to path as a timestamped backup, created
// exclusively and with the original file's permissions, never broader.
func backupFile(path string, src []byte) (string, error) {
	mode := fileMode(path)
	for i := 0; i < 100; i++ {
		name := path + ".bak-" + time.Now().Format("20060102-150405")
		if i > 0 {
			name += fmt.Sprintf("-%d", i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(src); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("could not pick a free backup name")
}

// replaceFile writes data to a unique temp file in the same directory with
// the original's permissions and renames it over path, so a crash never
// leaves a truncated file behind.
func replaceFile(path string, data []byte) error {
	mode := fileMode(path)
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { f.Close(); os.Remove(tmp) }
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// carryOver copies the settings a re-run of setup must not reset from the
// previous config into the freshly built one.
func carryOver(cfg, existing *config.Config) {
	if existing == nil {
		return
	}
	cfg.AwayAfterSeconds = existing.AwayAfterSeconds
	if !existing.InstalledAt.IsZero() {
		cfg.InstalledAt = existing.InstalledAt
	}
	cfg.VoiceCommand = existing.VoiceCommand
	cfg.SpeechCommand = existing.SpeechCommand
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
