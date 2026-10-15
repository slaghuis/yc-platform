package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

// ReplayHeader names the DLQ sequence a replayed event came from.
const ReplayHeader = "YC-Replay-Of"

// sequences is a repeatable --seq flag that also takes comma lists.
type sequences []uint64

func (s *sequences) String() string { return fmt.Sprint(*s) }

func (s *sequences) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		n, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
		if err != nil || n == 0 {
			return fmt.Errorf("%q is not a stream sequence", part)
		}
		*s = append(*s, n)
	}
	return nil
}

type opts struct {
	natsURL   string
	tenant    string
	domain    string
	service   string
	consumer  string
	eventType string
	eventID   string
	seqs      sequences
	limit     int
	jsonOut   bool
	all       bool
	dryRun    bool
}

func parseOpts(cmd string, args []string, stderr io.Writer) (opts, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o opts
	fs.StringVar(&o.natsURL, "nats-url", envOr("NATS_URL", "nats://localhost:4222"), "NATS URL")
	fs.StringVar(&o.tenant, "tenant", "", "tenant ID, e.g. demo-shop (default every tenant)")
	fs.StringVar(&o.domain, "domain", "", "event domain, e.g. order (default every domain)")
	fs.StringVar(&o.service, "service", "", "the consuming service that gave up, e.g. yc-message")
	fs.StringVar(&o.consumer, "consumer", "", "its durable consumer name")
	fs.StringVar(&o.eventType, "event-type", "", "original event type, e.g. order.placed.v1")
	fs.StringVar(&o.eventID, "event-id", "", "original event ID")
	fs.Var(&o.seqs, "seq", "YC_DLQ stream sequence; repeatable or comma-separated")
	if cmd == "list" {
		fs.IntVar(&o.limit, "limit", 50, "show at most this many (the newest)")
		fs.BoolVar(&o.jsonOut, "json", false, "one JSON object per line")
	}
	if cmd == "replay" {
		fs.BoolVar(&o.all, "all", false, "replay every dead letter matching the filters")
		fs.BoolVar(&o.dryRun, "dry-run", false, "print what would be replayed")
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	var errs []error
	if o.tenant != "" {
		if err := eventbus.ValidateTenantID(o.tenant); err != nil {
			errs = append(errs, fmt.Errorf("--tenant: %w", err))
		}
	}
	if o.domain != "" && strings.ContainsAny(o.domain, ".*> ") {
		errs = append(errs, errors.New("--domain is one word, e.g. order"))
	}
	if cmd == "replay" && len(o.seqs) == 0 && o.eventID == "" && !o.all {
		errs = append(errs, errors.New("replay needs --seq, --event-id or --all"))
	}
	if cmd == "list" && o.limit < 1 {
		errs = append(errs, errors.New("--limit must be at least 1"))
	}
	return o, errors.Join(errs...)
}

// subject is the DLQ subject filter for the options.
func (o opts) subject() string {
	return "yc." + or(o.tenant, "*") + "." + or(o.domain, "*") + ".dlq.v1"
}

func (o opts) matches(d deadLetter) bool {
	m := d.DLQMetadata
	return (o.service == "" || m.Service == o.service) &&
		(o.consumer == "" || m.Consumer == o.consumer) &&
		(o.eventType == "" || d.OriginalEvent.EventType == o.eventType) &&
		(o.eventID == "" || d.OriginalEvent.EventID == o.eventID)
}

// deadLetter is a YC_DLQ message.
type deadLetter struct {
	eventbus.DLQEnvelope
	Sequence uint64    `json:"sequence"`
	Subject  string    `json:"subject"`
	Stored   time.Time `json:"stored_at"`
	// original is the original event exactly as it was published.
	original json.RawMessage
}

func parse(subject string, seq uint64, stored time.Time, data []byte) (deadLetter, error) {
	d := deadLetter{Sequence: seq, Subject: subject, Stored: stored.UTC()}
	if err := json.Unmarshal(data, &d.DLQEnvelope); err != nil {
		return d, fmt.Errorf("dead letter %d: %w", seq, err)
	}
	var raw struct {
		OriginalEvent json.RawMessage `json:"original_event"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return d, fmt.Errorf("dead letter %d: %w", seq, err)
	}
	d.original = raw.OriginalEvent
	return d, nil
}

// replaySubject is where the original event goes back to. A dead letter
// in the _platform DLQ for another tenant's event is not replayable
// (ADR-003): its tenant could not be trusted.
func (d deadLetter) replaySubject() (string, error) {
	tenant, err := eventbus.TenantOfSubject(d.Subject)
	if err != nil {
		return "", err
	}
	env := d.OriginalEvent
	if tenant != env.TenantID {
		return "", fmt.Errorf("its event belongs to %q but it is in the %s DLQ; it is not replayed", env.TenantID, tenant)
	}
	return eventbus.SubjectForEvent(env.TenantID, env.EventType)
}

func execute(ctx context.Context, cmd string, o opts, stdout io.Writer) error {
	nc, err := nats.Connect(o.natsURL, nats.Name("yc-dlq-cli"))
	if err != nil {
		return fmt.Errorf("connect %s: %w", o.natsURL, err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	stream, err := js.Stream(ctx, eventbus.DLQStreamName)
	if err != nil {
		return fmt.Errorf("stream %s: %w", eventbus.DLQStreamName, err)
	}
	letters, err := fetch(ctx, stream, o)
	if err != nil {
		return err
	}
	switch cmd {
	case "list":
		if len(letters) > o.limit {
			letters = letters[len(letters)-o.limit:]
		}
		return list(stdout, letters, o.jsonOut)
	case "show":
		if len(letters) == 0 {
			return errors.New("no matching dead letters")
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		for _, d := range letters {
			if err := enc.Encode(d); err != nil {
				return err
			}
		}
		return nil
	default:
		return replay(ctx, js, stdout, letters, o.dryRun)
	}
}

// fetch returns the matching dead letters, oldest first.
func fetch(ctx context.Context, stream jetstream.Stream, o opts) ([]deadLetter, error) {
	var out []deadLetter
	keep := func(d deadLetter) {
		if o.matches(d) {
			out = append(out, d)
		}
	}
	if len(o.seqs) > 0 {
		for _, seq := range o.seqs {
			msg, err := stream.GetMsg(ctx, seq)
			if err != nil {
				return nil, fmt.Errorf("dead letter %d: %w", seq, err)
			}
			if !matchSubject(o.subject(), msg.Subject) {
				continue
			}
			d, err := parse(msg.Subject, msg.Sequence, msg.Time, msg.Data)
			if err != nil {
				return nil, err
			}
			keep(d)
		}
		return out, nil
	}
	cons, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{o.subject()}, DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, err
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return nil, err
	}
	for pending := info.NumPending; pending > 0; {
		batch, err := cons.Fetch(int(min(pending, 500)), jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			return nil, err
		}
		n := 0
		for msg := range batch.Messages() {
			n++
			meta, err := msg.Metadata()
			if err != nil {
				return nil, err
			}
			d, err := parse(msg.Subject(), meta.Sequence.Stream, meta.Timestamp, msg.Data())
			if err != nil {
				return nil, err
			}
			keep(d)
			pending = meta.NumPending
		}
		if err := batch.Error(); err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	return out, nil
}

func list(w io.Writer, letters []deadLetter, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		for _, d := range letters {
			if err := enc.Encode(d); err != nil {
				return err
			}
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SEQ\tSTORED\tTENANT\tEVENT TYPE\tEVENT ID\tCONSUMER\tREASON\tATTEMPTS\tLAST ERROR")
	for _, d := range letters {
		m, e := d.DLQMetadata, d.OriginalEvent
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", d.Sequence, d.Stored.Format(time.RFC3339),
			e.TenantID, e.EventType, e.EventID, m.Consumer, m.FailureReason, m.Attempts, truncate(m.LastError, 80))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%d dead letter(s)\n", len(letters))
	return err
}

func replay(ctx context.Context, js jetstream.JetStream, w io.Writer, letters []deadLetter, dryRun bool) error {
	if len(letters) == 0 {
		return errors.New("no matching dead letters")
	}
	var failed int
	for _, d := range letters {
		subject, err := d.replaySubject()
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(w, "skip    seq %d: %v\n", d.Sequence, err)
			continue
		}
		if dryRun {
			_, _ = fmt.Fprintf(w, "would replay seq %d: %s %s -> %s (failed in %s)\n", d.Sequence,
				d.OriginalEvent.EventType, d.OriginalEvent.EventID, subject, d.DLQMetadata.Consumer)
			continue
		}
		msg := nats.NewMsg(subject)
		msg.Data = d.original
		msg.Header.Set(ReplayHeader, strconv.FormatUint(d.Sequence, 10))
		if _, err := js.PublishMsg(ctx, msg); err != nil {
			return fmt.Errorf("replay seq %d: %w", d.Sequence, err)
		}
		_, _ = fmt.Fprintf(w, "replayed seq %d: %s %s -> %s (failed in %s)\n", d.Sequence,
			d.OriginalEvent.EventType, d.OriginalEvent.EventID, subject, d.DLQMetadata.Consumer)
	}
	if failed > 0 {
		return fmt.Errorf("%d dead letter(s) not replayed", failed)
	}
	return nil
}

// matchSubject matches a subject against a filter with * wildcards.
func matchSubject(filter, subject string) bool {
	f, s := strings.Split(filter, "."), strings.Split(subject, ".")
	if len(f) != len(s) {
		return false
	}
	for i := range f {
		if f[i] != "*" && f[i] != s[i] {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func or(s, otherwise string) string {
	if s == "" {
		return otherwise
	}
	return s
}

func envOr(key, otherwise string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return otherwise
}
