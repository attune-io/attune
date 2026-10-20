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

package weeklyreport

import (
	"fmt"
	"regexp"
	"time"
)

const (
	stampConfigMap         = "attune-weekly-report"
	stampAnnotation        = "attune.io/last-report-week"
	stampSkippedAnnotation = "attune.io/last-report-skipped"
	reportInterval         = 15 * time.Minute
	historyWindow          = 168 * time.Hour
	textRuneCap            = 3000
	maxList                = 5
)

var weekIDPattern = regexp.MustCompile(`^[0-9]{4}-W(0[1-9]|[1-4][0-9]|5[0-3])$`)

// WeekID is the UTC ISO week, zero-padded, for example 2026-W43.
func WeekID(t time.Time) string {
	year, week := t.UTC().ISOWeek()
	return sprintfWeek(year, week)
}

func sprintfWeek(year, week int) string {
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// stampAlreadySent reports whether a stored stamp should suppress a send.
// A value that is not YYYY-Www is unstamped. A later week waits for the clock.
func stampAlreadySent(stamp, current string) bool {
	if !weekIDPattern.MatchString(stamp) {
		return false
	}
	return stamp >= current
}
