// yc-dlq-cli lists, shows and replays dead letters from the YC_DLQ stream
// (ADR-003, docs/runbooks/dlq-replay.md).
//
// A replay republishes the dead letter's original event, unchanged, to its
// original subject. Consumers that already processed the event skip it
// (per-consumer dedup), so in effect only the consumer that gave up runs
// again. Dead letters routed to the _platform DLQ for a tenant mismatch
// are never replayed.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage: yc-dlq-cli <command> [flags]

commands:
  list     list dead letters (newest last)
  show     print dead letters in full
  replay   republish dead letters' original events

Selection flags (all commands): --tenant, --domain, --service, --consumer,
--event-type, --event-id, --seq (repeatable). replay needs --seq, --event-id
or --all with the other filters.

Run 'yc-dlq-cli <command> -h' for every flag.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "yc-dlq-cli:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return fmt.Errorf("missing command")
	}
	switch args[0] {
	case "list", "show", "replay":
		o, err := parseOpts(args[0], args[1:], stderr)
		if err != nil {
			return err
		}
		return execute(ctx, args[0], o, stdout)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return nil
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}
