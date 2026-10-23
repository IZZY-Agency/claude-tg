package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/send"
)

func init() {
	register("test", "send a test message to the configured chat", cmdTest)
}

func cmdTest(c *cli, _ []string) int {
	a, code := newApp(c)
	if code != exitOK {
		return code
	}
	host, _ := os.Hostname()
	body := fmt.Sprintf("tg test from %s, %s", host, time.Now().Format("2006-01-02 15:04:05"))
	ids, err := a.sender.Send(context.Background(), send.Message{Label: "tg", Body: body, Silent: true, Kind: send.KindSystem})
	if err != nil {
		fmt.Fprintf(c.stderr, "tg test: %v\n", err)
		return send.Classify(err)
	}
	fmt.Fprintf(c.stdout, "sent %s\n", joinIDs(ids))
	return exitOK
}
