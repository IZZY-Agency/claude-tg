package telegram

import (
	"fmt"
	"testing"
)

func TestConflictKinds(t *testing.T) {
	poller := &APIError{Code: 409, Description: "Conflict: terminated by other getUpdates request; make sure that only one bot instance is running"}
	webhook := &APIError{Code: 409, Description: "Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first"}
	for _, tc := range []struct {
		name             string
		err              error
		poll, webhookSet bool
	}{
		{"another poller", poller, true, false},
		{"wrapped poller", fmt.Errorf("getUpdates: %w", poller), true, false},
		{"webhook", webhook, false, true},
		{"other 409", &APIError{Code: 409, Description: "Conflict"}, false, false},
		{"429", &APIError{Code: 429, Description: "Too Many Requests", RetryAfter: 3}, false, false},
		{"network", &NetError{Err: fmt.Errorf("post: no route to host")}, false, false},
		{"nil", nil, false, false},
	} {
		if got := PollConflict(tc.err); got != tc.poll {
			t.Errorf("%s: PollConflict=%v, want %v", tc.name, got, tc.poll)
		}
		if got := WebhookConflict(tc.err); got != tc.webhookSet {
			t.Errorf("%s: WebhookConflict=%v, want %v", tc.name, got, tc.webhookSet)
		}
	}
}
