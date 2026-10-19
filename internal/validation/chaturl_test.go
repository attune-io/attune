/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validation

import "testing"

func TestChatWebhookURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw    string
		reason string
	}{
		{raw: "https://hooks.slack.com/services/T/B/x"},
		{raw: "https://hooks.slack.com:443/services/T/B/x"},
		{raw: "https://abc.environment.api.powerplatform.com/powerautomate/workflows/1"},
		{raw: "http://hooks.slack.com/services/T/B/x", reason: "chat webhook URL rejected"},
		{raw: "https://user:pass@hooks.slack.com/services/T/B/x", reason: "chat webhook URL rejected"},
		{raw: "https://hooks.slack.com/services/T/B/x#frag", reason: "chat webhook URL rejected"},
		{raw: "https://hooks.slack.com:8443/services/T/B/x", reason: "chat webhook URL rejected"},
		{raw: "https://hooks.slack.com./services/T/B/x", reason: "chat webhook URL rejected"},
		{raw: "https://127.0.0.1/services/T/B/x", reason: "chat webhook URL rejected"},
		{raw: "https://hooks.slack.com/services/../x", reason: "chat webhook URL rejected"},
		{raw: "https://hooks.slack.com/other", reason: "chat webhook URL rejected"},
		{raw: "https://example.com/services/T/B/x", reason: "chat webhook URL rejected"},
		{raw: "https://environment.api.powerplatform.com/powerautomate/workflows/1", reason: "chat webhook URL rejected"},
		{raw: "https://abc.environment.api.powerplatform.com/workflows/1", reason: "chat webhook URL rejected"},
		{raw: "https://webhook.office.com/webhookb2/x", reason: "chat webhook office connector rejected"},
		{raw: "https://outlook.office.com/webhook/x", reason: "chat webhook outlook connector rejected"},
		{raw: "https://logic.azure.com/workflows/x", reason: "chat webhook logic app rejected"},
		{raw: "https://prod.logic.azure.com/workflows/x", reason: "chat webhook logic app rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			err := ChatWebhookURL(tc.raw)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("want allow, got %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.reason {
				t.Fatalf("got %v want %s", err, tc.reason)
			}
		})
	}
}
