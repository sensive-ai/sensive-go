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
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	taxonomyv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/taxonomy/v1"
)

// Mode is what the client does with a check: SENSIVE_MODE.
type Mode string

const (
	// ModeEnforced sends the check and waits for the decision.
	ModeEnforced Mode = "ENFORCED"
	// ModeLogOnly sends the check in the background and returns ALLOW.
	ModeLogOnly Mode = "LOG_ONLY"
	// ModeDisabled sends nothing and returns ALLOW.
	ModeDisabled Mode = "DISABLED"
)

// The variables every Sensive SDK reads (README.md, "Configuration").
const (
	envDACURL      = "SENSIVE_DAC_URL"
	envEnvironment = "SENSIVE_ENVIRONMENT"
	envTimeout     = "SENSIVE_TIMEOUT_MS"
	envMode        = "SENSIVE_MODE"
)

const defaultTimeout = 5000 * time.Millisecond

// ConfigError is an environment the client cannot read: the variable, and
// what is wrong with it.
type ConfigError struct {
	Variable string
	Problem  string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("sensive: %s %s", e.Variable, e.Problem)
}

// settings is what the client reads from the environment. In DISABLED
// mode it is the mode alone.
type settings struct {
	mode        Mode
	dacURL      string
	target      string // the URL's host and port, which gRPC calls
	secure      bool   // whether the URL asks for TLS
	environment taxonomyv1.Environment_Value
	timeout     time.Duration
}

// settingsFrom reads the settings from an environment, as os.LookupEnv
// does: each value trimmed, an empty one unset, and every value spelled
// exactly as the contract lists it. A value it cannot read is a
// ConfigError naming the variable; a default never stands in for one.
func settingsFrom(lookup func(string) (string, bool)) (settings, error) {
	value := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	refuse := func(name, format string, args ...any) (settings, error) {
		return settings{}, &ConfigError{Variable: name, Problem: fmt.Sprintf(format, args...)}
	}

	s := settings{mode: ModeEnforced}
	switch v := value(envMode); Mode(v) {
	case "":
	case ModeEnforced, ModeLogOnly, ModeDisabled:
		s.mode = Mode(v)
	default:
		return refuse(envMode, "is %q, want ENFORCED, LOG_ONLY or DISABLED", v)
	}
	if s.mode == ModeDisabled {
		return s, nil
	}

	s.dacURL = value(envDACURL)
	if s.dacURL == "" {
		return refuse(envDACURL, "is not set")
	}
	target, secure, err := targetOf(s.dacURL)
	if err != nil {
		return refuse(envDACURL, "is %q: %v", s.dacURL, err)
	}
	s.target, s.secure = target, secure

	s.environment = taxonomyv1.Environment_PRODUCTION_DATA
	switch v := value(envEnvironment); v {
	case "":
	case taxonomyv1.Environment_PRODUCTION_DATA.String(), taxonomyv1.Environment_TEST_DATA.String():
		s.environment = taxonomyv1.Environment_Value(taxonomyv1.Environment_Value_value[v])
	default:
		return refuse(envEnvironment, "is %q, want PRODUCTION_DATA or TEST_DATA", v)
	}

	s.timeout = defaultTimeout
	if v := value(envTimeout); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || !isDigits(v) || ms <= 0 {
			return refuse(envTimeout, "is %q, want a whole number of milliseconds greater than zero", v)
		}
		s.timeout = time.Duration(ms) * time.Millisecond
	}
	return s, nil
}

// isDigits reports whether text is decimal digits and nothing else, so a
// sign or a unit is refused.
func isDigits(text string) bool {
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return text != ""
}

// targetOf is the gRPC target a checker URL names, its host and port, and
// whether the URL asks for TLS. The checker serves the same methods over
// gRPC on the URL's host as under its path.
func targetOf(checkerURL string) (target string, secure bool, err error) {
	u, err := url.Parse(checkerURL)
	if err != nil {
		return "", false, err
	}
	port := u.Port()
	switch u.Scheme {
	case "https":
		secure = true
		if port == "" {
			port = "443"
		}
	case "http":
		if port == "" {
			port = "80"
		}
	default:
		return "", false, fmt.Errorf("the scheme is not https or http")
	}
	if u.Hostname() == "" {
		return "", false, fmt.Errorf("it names no host")
	}
	return net.JoinHostPort(u.Hostname(), port), secure, nil
}

// logAttrs is what the client logs once it is configured: the mode, and
// unless it is DISABLED, the checker, the environment and the timeout.
func (s settings) logAttrs() []any {
	if s.mode == ModeDisabled {
		return []any{slog.String("mode", string(s.mode))}
	}
	return []any{
		slog.String("mode", string(s.mode)),
		slog.String("dac_url", s.dacURL),
		slog.String("environment", s.environment.String()),
		slog.Int64("timeout_ms", s.timeout.Milliseconds()),
	}
}
