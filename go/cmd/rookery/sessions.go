package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/llcoolkm/rookery/go/core"
)

func showSessions(ctx context.Context, client *core.Client, args arguments) error {
	var interval time.Duration
	if raw, set := args.options["watch"]; set {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 3600 {
			return errors.New("--watch must be between 1 and 3600 seconds")
		}
		interval = time.Duration(seconds) * time.Second
	}
	if len(args.positionals) != 0 {
		return errors.New("usage: rookery sessions [--watch SECONDS]")
	}
	stat, _ := os.Stdout.Stat()
	term := os.Getenv("TERM")
	redraw := interval > 0 && stat != nil && stat.Mode()&os.ModeCharDevice != 0 && term != "" && term != "dumb"
	return readSessions(ctx, os.Stdout, interval, redraw, func(ctx context.Context) (map[string]any, error) {
		result, err := client.Invoke(ctx, "peers", nil)
		if err != nil {
			return nil, err
		}
		return result.(map[string]any), nil
	})
}

func readSessions(ctx context.Context, out io.Writer, interval time.Duration, redraw bool, peers func(context.Context) (map[string]any, error)) error {
	for {
		directory, err := peers(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if interval == 0 && err != nil {
			return err
		}
		if redraw {
			if _, err := fmt.Fprint(out, "\033[H\033[J"); err != nil {
				return err
			}
		}
		if err != nil {
			if _, err := fmt.Fprintf(out, "Could not read peer sessions: %v\n", err); err != nil {
				return err
			}
		} else {
			if err := writeSessions(out, directory, float64(time.Now().Unix())); err != nil {
				return err
			}
		}
		if interval == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

func sessionText(row map[string]any, key string) string {
	value, _ := row[key].(string)
	return value
}

func writeSessions(out io.Writer, directory map[string]any, now float64) error {
	rank := func(status string) int {
		switch status {
		case "waiting":
			return 0
		case "busy":
			return 1
		case "idle":
			return 2
		default:
			return 3
		}
	}
	var live []map[string]any
	rows, _ := directory["sessions"].([]any)
	for _, row := range rows {
		if session, ok := row.(map[string]any); ok && session["online"] == true {
			live = append(live, session)
		}
	}
	slices.SortStableFunc(live, func(a, b map[string]any) int {
		if order := cmp.Compare(rank(sessionText(a, "status")), rank(sessionText(b, "status"))); order != 0 {
			return order
		}
		for _, key := range []string{"node", "provider", "name", "id"} {
			if order := cmp.Compare(sessionText(a, key), sessionText(b, key)); order != 0 {
				return order
			}
		}
		return 0
	})
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Node\tProvider\tName\tStatus\tWaiting for\tSince\tTmux\tCwd")
	for _, row := range live {
		node, status := sessionText(row, "node"), sessionText(row, "status")
		waiting, age, tmux := "", "", sessionText(row, "tmux")
		if status == "waiting" {
			waiting = sessionText(row, "waiting_for")
		}
		if stamp, ok := row["status_since"].(float64); ok {
			age = fmt.Sprintf("%.0fs", math.Floor(max(0, now-stamp)))
		}
		if tmux != "" {
			tmux = node + " " + tmux
		}
		fields := []string{node, sessionText(row, "provider"), sessionText(row, "name"), status, waiting, age, tmux, sessionText(row, "cwd")}
		for i, value := range fields {
			if value == "" {
				fields[i] = "—"
			} else {
				fields[i] = strings.Map(func(r rune) rune {
					if unicode.IsPrint(r) {
						return r
					}
					return '?'
				}, value)
			}
		}
		fmt.Fprintln(w, strings.Join(fields, "\t"))
	}
	if len(live) == 0 {
		switch {
		case directory["detail"] == "helper has not connected yet":
			fmt.Fprintln(w, "Helper has not connected yet.")
		case directory["stale"] == true:
			fmt.Fprintln(w, "Peer data is stale.")
		default:
			fmt.Fprintln(w, "No live peer sessions.")
		}
	}
	return w.Flush()
}
