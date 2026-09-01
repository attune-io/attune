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
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// EKS cluster names: 1-100 chars, start alphanumeric, then [A-Za-z0-9-_].
// Underscore is allowed for defense-in-depth beyond current EKS rules.
var cloudWatchClusterNameRE = regexp.MustCompile(`^[0-9A-Za-z][A-Za-z0-9\-_]{0,99}$`)

// IAM role ARN: partition aws / aws-cn / aws-us-gov, 12-digit account, role path.
var iamRoleARNRE = regexp.MustCompile(`^arn:aws(-us-gov|-cn)?:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]+$`)

// CloudWatchClusterName allowlists EKS-like names so they cannot break out of
// a CloudWatch SEARCH ClusterName="..." term.
func CloudWatchClusterName(name string) error {
	if name == "" {
		return fmt.Errorf("clusterName is required")
	}
	if err := CloudWatchSEARCHToken(name); err != nil {
		return fmt.Errorf("clusterName: %w", err)
	}
	if !cloudWatchClusterNameRE.MatchString(name) {
		return fmt.Errorf("clusterName must be an EKS-style name (1-100 chars, alphanumeric, hyphen, underscore)")
	}
	return nil
}

// CloudWatchRoleARN validates an optional IAM role ARN used for AssumeRole.
// Empty is allowed (IRSA / Pod Identity). Quotes and spaces are rejected so
// the ARN cannot be used as a SEARCH-style injection string if interpolated.
func CloudWatchRoleARN(arn string) error {
	if arn == "" {
		return nil
	}
	if len(arn) > 2048 {
		return fmt.Errorf("roleArn is too long")
	}
	if err := CloudWatchSEARCHToken(arn); err != nil {
		return fmt.Errorf("roleArn: %w", err)
	}
	if !iamRoleARNRE.MatchString(arn) {
		return fmt.Errorf("roleArn must be an IAM role ARN (arn:aws:iam::ACCOUNT:role/NAME)")
	}
	return nil
}

// CloudWatchSEARCHToken rejects characters that can break out of a quoted
// CloudWatch SEARCH term (double/single quote, backslash, whitespace).
func CloudWatchSEARCHToken(s string) error {
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(s, "\"'\\") {
		return fmt.Errorf("must not contain quotes or backslash")
	}
	for _, r := range s {
		if unicode.IsSpace(r) {
			return fmt.Errorf("must not contain whitespace")
		}
	}
	return nil
}
