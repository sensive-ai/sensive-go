// Copyright 2026 Sensive, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sensiveclient

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// refusalLogInterval is how often the client logs a refused check: the
// first at once, then at most one in each interval.
const refusalLogInterval = time.Minute

// A refusal is a check the checker turned away before deciding it: a
// request it could not read, or a caller it would not take one from. Each
// is a client that is misconfigured, and nothing else says so: a refused
// check is never decided, so the access log does not record it.
type refusal struct {
	reason  string // as the checker counts it
	message string
}

// refusalReasons are the codes a refusal answers with, the checker's
// reasons for each.
var refusalReasons = map[codes.Code]refusal{
	codes.InvalidArgument:  {"malformed", "sensive: access check refused, not decided: the request is malformed; the detail names the field and the rule it broke"},
	codes.Unauthenticated:  {"unauthenticated", "sensive: access check refused, not decided: the checker could not identify the caller"},
	codes.PermissionDenied: {"forbidden", "sensive: access check refused, not decided: the caller may not ask for one"},
}

// refusalLog reports the checks the checker refused, in ENFORCED and
// LOG_ONLY mode, so a misconfigured client says so in its own service's
// logs. A refused check has no result: the ALLOW the client returns in
// LOG_ONLY, so the application is never blocked, and the DENY it returns
// in ENFORCED, so it fails closed, are the client's, not a decision, so
// the line says the check was not decided and names neither. Every other
// outcome is left alone: a decision of either kind, and a check that
// failed for another reason, such as a timeout. It logs one refusal in
// each interval, with how many it did not log since the last line, so a
// client whose every check is refused logs a line a minute.
type refusalLog struct {
	// logger is where the lines go; nil is slog's default logger, as it is
	// when the line is written.
	logger *slog.Logger
	// now is the clock the interval is measured on.
	now func() time.Time

	mu      sync.Mutex
	logged  time.Time // when the last line was written; zero before the first
	skipped int       // the refusals since then that were not logged
}

func newRefusalLog() *refusalLog {
	return &refusalLog{now: time.Now}
}

// report logs a check sent in mode that failed with err, when err is the
// checker's refusal and the interval allows.
func (l *refusalLog) report(mode Mode, err error) {
	// The status itself, not status.FromError's, which on a wrapped error
	// replaces the checker's message with the whole chain's.
	var answered interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &answered) {
		return
	}
	s := answered.GRPCStatus()
	r, ok := refusalReasons[s.Code()]
	if !ok {
		return
	}
	l.mu.Lock()
	now := l.now()
	if !l.logged.IsZero() && now.Sub(l.logged) < refusalLogInterval {
		l.skipped++
		l.mu.Unlock()
		return
	}
	skipped := l.skipped
	l.logged, l.skipped = now, 0
	l.mu.Unlock()

	logger := l.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn(r.message,
		slog.String("reason", r.reason),
		slog.String("mode", string(mode)),
		slog.String("detail", s.Message()),
		slog.Int("skipped", skipped),
	)
}
