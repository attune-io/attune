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

import (
	"net"
	"net/url"
	"strings"
)

// ChatWebhookURL checks a weekly-report webhook URL.
// Error text is a fixed reason. It does not include the URL, host, or path.
func ChatWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return errChatWebhookInvalid
	}
	if u.Opaque != "" || u.User != nil || u.Fragment != "" {
		return errChatWebhookInvalid
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errChatWebhookInvalid
	}
	host := u.Hostname()
	if host == "" || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil {
		return errChatWebhookInvalid
	}
	port := u.Port()
	if port != "" && port != "443" {
		return errChatWebhookInvalid
	}
	if pathHasTraversal(u) {
		return errChatWebhookInvalid
	}
	host = strings.ToLower(host)
	switch {
	case hostDenied(host, "webhook.office.com"):
		return errChatWebhookOffice
	case hostDenied(host, "outlook.office.com"):
		return errChatWebhookOutlook
	case hostDenied(host, "logic.azure.com"):
		return errChatWebhookLogic
	case host == "hooks.slack.com" && strings.HasPrefix(u.EscapedPath(), "/services/"):
		return nil
	case powerPlatformHost(host) && strings.Contains(u.EscapedPath(), "/powerautomate/"):
		return nil
	default:
		return errChatWebhookInvalid
	}
}

func hostDenied(host, denied string) bool {
	return host == denied || strings.HasSuffix(host, "."+denied)
}

func powerPlatformHost(host string) bool {
	const suffix = ".environment.api.powerplatform.com"
	return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}

func pathHasTraversal(u *url.URL) bool {
	raw := strings.ToLower(u.EscapedPath())
	if strings.Contains(raw, "..") || strings.Contains(raw, "%2e%2e") || strings.Contains(raw, "%2e.") || strings.Contains(raw, ".%2e") {
		return true
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

var (
	errChatWebhookInvalid = chatWebhookError("chat webhook URL rejected")
	errChatWebhookOffice  = chatWebhookError("chat webhook office connector rejected")
	errChatWebhookOutlook = chatWebhookError("chat webhook outlook connector rejected")
	errChatWebhookLogic   = chatWebhookError("chat webhook logic app rejected")
)

type chatWebhookError string

func (e chatWebhookError) Error() string { return string(e) }
