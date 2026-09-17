package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// envInt reads an integer environment variable, falling back to def when the
// variable is unset or malformed (malformed values are logged, not fatal).
func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("ignoring malformed integer env var", "name", name, "value", v, "default", def)
		return def
	}
	return n
}

// envCSV reads a comma-separated environment variable as a trimmed list,
// returning def when the variable is unset or empty.
func envCSV(name string, def []string) []string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
