// Command thaw builds, restores and forks agent VM snapshots, and runs the
// benchmarks.
//
//	thaw base   -env py-data                 boot an environment, warm it, snapshot it
//	thaw run    -snap ID -task NAME          restore, run one task, print timings
//	thaw bench  -out results/vm.json         restore every strategy on every environment
//	thaw sim    -out results/sim.json        replay recorded traces under other conditions
//	thaw ls                                  list snapshots
//	thaw plan   -snap ID                     show the prefetch plan the traces give
//	thaw repack -snap ID                     rewrite hot blocks in plan order
//	thaw gc                                  delete what no snapshot needs
//	thaw serve  -addr :7070                  HTTP API and live restore events
//	thaw export -out web/public/runs         replayed runs for the site to play back
//
// Every command takes -store (a directory or s3://bucket/prefix?endpoint=...)
// and -profile, which shapes the store's latency and bandwidth to match a
// real object store when the bytes actually come from a local disk.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Rakshit-gen/thaw/objstore"
	"github.com/Rakshit-gen/thaw/repo"
	"github.com/Rakshit-gen/thaw/restore"
)

// Set by the release build.
var (
	version = "dev"
	commit  = ""
)

type common struct {
	store   string
	profile string
	seed    uint64
	local   string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.store, "store", envOr("THAW_STORE", "/var/lib/thaw/store"), "object store: a directory or s3://bucket/prefix?endpoint=host:port")
	fs.StringVar(&c.profile, "profile", envOr("THAW_PROFILE", "none"), "shape the store like: none, s3-standard, s3-express, peer")
	fs.Uint64Var(&c.seed, "seed", 1, "seed for the shaped store's latency draws")
	fs.StringVar(&c.local, "local", "", "directory caching immutable objects such as manifests")
}

// open returns the repo and the request counter under it.
func (c *common) open() (*repo.Repo, *objstore.Counting, error) {
	st, err := objstore.Open(c.store)
	if err != nil {
		return nil, nil, err
	}
	counted := objstore.NewCounting(st)
	var s objstore.Store = counted
	if c.profile != "none" && c.profile != "" {
		p, ok := objstore.ProfileByName(c.profile)
		if !ok {
			return nil, nil, fmt.Errorf("unknown profile %q", c.profile)
		}
		s = objstore.NewShaped(counted, p, c.seed)
	}
	return repo.Open(s, c.local), counted, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type command struct {
	name  string
	about string
	run   func(ctx context.Context, args []string) error
}

var commands []command

func main() {
	commands = append(commands,
		command{"ls", "list snapshots", cmdLs},
		command{"plan", "show the prefetch plan for a snapshot", cmdPlan},
		command{"repack", "rewrite a snapshot's hot blocks in plan order", cmdRepack},
		command{"gc", "delete packs and manifests no snapshot needs", cmdGC},
		command{"sim", "replay recorded traces under other conditions", cmdSim},
		command{"serve", "HTTP API and live restore events", cmdServe},
		command{"export", "write replayed runs for the site to play back", cmdExport},
		command{"report", "write results/README.md and charts from the result files", cmdReport},
	)
	commands = append(commands, vmCommands...)
	sort.Slice(commands, func(i, j int) bool { return commands[i].name < commands[j].name })

	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("thaw", version, commit)
		return
	}
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for _, c := range commands {
		if c.name == os.Args[1] {
			if err := c.run(ctx, os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "thaw:", err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "thaw: unknown command %q\n", os.Args[1])
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: thaw <command> [flags]   (thaw version prints the version)")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-7s %s\n", c.name, c.about)
	}
}

func cmdLs(ctx context.Context, args []string) error {
	var c common
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	c.register(fs)
	fs.Parse(args)
	r, _, err := c.open()
	if err != nil {
		return err
	}
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return err
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Created.Before(snaps[j].Created) })
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tENV\tPARENT\tCREATED\tMEM\tDISK\tNEW\tTRACES")
	for _, s := range snaps {
		traces, _ := r.Traces(ctx, s.ID)
		var newBytes int64
		for _, f := range s.Files {
			newBytes += f.New
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", s.ID, s.Env, orDash(s.Parent),
			s.Created.Format("2006-01-02 15:04"), mib(s.Files["mem"].Stored), mib(s.Files["disk"].Stored), mib(newBytes), len(traces))
	}
	return w.Flush()
}

func cmdPlan(ctx context.Context, args []string) error {
	var c common
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	c.register(fs)
	id := fs.String("snap", "", "snapshot ID")
	asJSON := fs.Bool("json", false, "print the plan as JSON")
	fs.Parse(args)
	r, _, err := c.open()
	if err != nil {
		return err
	}
	s, err := r.Snapshot(ctx, *id)
	if err != nil {
		return err
	}
	p, from, err := restore.PlanFor(ctx, r, s, 3, defaultPlan)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(p)
	}
	fmt.Printf("plan for %s from %d traces of %s\n", s.ID, p.Traces, strings.Join(from, ", "))
	for _, f := range p.Files {
		n := len(p.Order(f))
		fmt.Printf("  %-5s %5d blocks, %s\n", f, n, mib(int64(n)*int64(s.Files[f].Block)))
	}
	return nil
}

func cmdRepack(ctx context.Context, args []string) error {
	var c common
	fs := flag.NewFlagSet("repack", flag.ExitOnError)
	c.register(fs)
	id := fs.String("snap", "", "snapshot ID")
	fs.Parse(args)
	r, counted, err := c.open()
	if err != nil {
		return err
	}
	s, err := r.Snapshot(ctx, *id)
	if err != nil {
		return err
	}
	p, _, err := restore.PlanFor(ctx, r, s, 3, defaultPlan)
	if err != nil {
		return err
	}
	order := map[string][]int{}
	for _, f := range p.Files {
		order[f] = p.Order(f)
	}
	_, st, err := r.Repack(ctx, s, order, 0)
	if err != nil {
		return err
	}
	fmt.Printf("repacked %d hot blocks (%s) into %d packs in %v using %d reads; %d requests in all\n",
		st.Blocks, mib(st.Bytes), st.Packs, st.Took.Round(time.Millisecond), st.Requests, counted.Counts().Gets)
	return nil
}

func cmdGC(ctx context.Context, args []string) error {
	var c common
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	c.register(fs)
	grace := fs.Duration("grace", 10*time.Minute, "never delete what was condemned or written less than this long ago")
	fs.Parse(args)
	r, _, err := c.open()
	if err != nil {
		return err
	}
	r.Grace = *grace
	st, err := r.GC(ctx)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(b))
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", a...)
}
