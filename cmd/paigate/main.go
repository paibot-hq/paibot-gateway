// Command paigate runs the gateway and manages virtual keys.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/paibot-hq/paigate/internal/admin"
	"github.com/paibot-hq/paigate/internal/gateway"
	"github.com/paibot-hq/paigate/internal/store"
)

const usage = `usage:
  paigate serve  [-config paigate.json]   run the gateway (SIGHUP reloads the config)
  paigate check  [-config paigate.json]   validate the config and exit
  paigate genkey                          print a new virtual key and its hash
  paigate hash                            read a key from stdin and print its hash

With "database_url_env" in the config:
  paigate db migrate                      create or update the database tables
  paigate key create -name N -models a,b [-owner O] [-rpm N] [-budget TOKENS] [-expires YYYY-MM-DD]
  paigate key list
  paigate key update -name N [-models a,b] [-owner O] [-rpm N] [-budget TOKENS] [-expires YYYY-MM-DD|never]
  paigate key revoke -name N
  paigate usage [-month YYYY-MM]

Every command takes -config. Put flags before other arguments.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "serve":
		err = serve(args)
	case "check":
		err = check(args)
	case "genkey":
		k := gateway.GenerateKey()
		fmt.Printf("key:  %s\nhash: %s\n", k, gateway.HashKey(k))
		fmt.Fprintln(os.Stderr, "Give the key to its user once; put only the hash in the config.")
	case "hash":
		err = hash()
	case "db":
		if len(args) == 0 || args[0] != "migrate" {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		err = dbMigrate(args[1:])
	case "key":
		err = keyCommand(args)
	case "usage":
		err = usageReport(args)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "paigate:", err)
		os.Exit(1)
	}
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	path := fs.String("config", "paigate.json", "path to the JSON config")
	return fs, path
}

func check(args []string) error {
	fs, path := newFlags("check")
	fs.Parse(args)
	cfg, err := gateway.LoadConfig(*path)
	if err != nil {
		return err
	}
	if _, err := gateway.New(cfg, slog.New(slog.DiscardHandler), os.Getenv); err != nil {
		return err
	}
	if err := needEnv(cfg.DatabaseURLEnv, "database_url_env"); err != nil {
		return err
	}
	if err := needEnv(cfg.AdminTokenEnv, "admin_token_env"); err != nil {
		return err
	}
	fmt.Println("config ok")
	return nil
}

func needEnv(name, field string) error {
	if name != "" && os.Getenv(name) == "" {
		return fmt.Errorf("%s: environment variable %s is not set", field, name)
	}
	return nil
}

func openStore(cfg *gateway.Config) (*store.Store, error) {
	if cfg.DatabaseURLEnv == "" {
		return nil, errors.New(`this needs a database: set "database_url_env" in the config`)
	}
	if err := needEnv(cfg.DatabaseURLEnv, "database_url_env"); err != nil {
		return nil, err
	}
	return store.Open(os.Getenv(cfg.DatabaseURLEnv))
}

func serve(args []string) error {
	fs, path := newFlags("serve")
	fs.Parse(args)
	cfg, err := gateway.LoadConfig(*path)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	g, err := gateway.New(cfg, log, os.Getenv)
	if err != nil {
		return err
	}
	var current atomic.Pointer[gateway.Config]
	current.Store(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var st *store.Store
	if cfg.DatabaseURLEnv != "" {
		if st, err = openStore(cfg); err != nil {
			return err
		}
		defer st.Close()
		g.UseDatabase(st, cfg.Location())
		// Load keys before serving, but do not refuse to start if the
		// database is down: config-file keys keep working, and the sync
		// below keeps retrying.
		first, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := g.SyncOnce(first); err != nil {
			log.Error("database not ready; keys from it will work once it is", "error", err)
		}
		cancel()
		go g.Sync(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// No WriteTimeout: streamed completions can legitimately run long.
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
	servers := []*http.Server{srv}
	if cfg.AdminTokenEnv != "" {
		token := os.Getenv(cfg.AdminTokenEnv)
		if token == "" {
			return fmt.Errorf("admin_token_env: environment variable %s is not set", cfg.AdminTokenEnv)
		}
		ui, err := admin.New(admin.Options{
			Store:    st,
			Token:    token,
			Models:   func() []string { return current.Load().ModelNames() },
			Location: cfg.Location(),
			Log:      log,
			NewKey: func() (string, string) {
				k := gateway.GenerateKey()
				return k, gateway.HashKey(k)
			},
		})
		if err != nil {
			return err
		}
		servers = append(servers, &http.Server{
			Addr:              cfg.AdminListen,
			Handler:           ui,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
		})
	}

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errc <- s.ListenAndServe() }()
	}
	log.Info("listening", "addr", cfg.Listen)
	if len(servers) > 1 {
		log.Info("admin UI listening", "addr", cfg.AdminListen)
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case err := <-errc:
			return err
		case <-hup:
			reload(*path, g, &current, log)
			continue
		case <-ctx.Done():
		}
		break
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			return err
		}
	}
	if err := g.Flush(shutdownCtx); err != nil {
		log.Error("could not write the last usage counts", "error", err)
	}
	for range servers {
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

// reload applies a changed config file. A broken file is reported and the
// running config kept.
func reload(path string, g *gateway.Gateway, current *atomic.Pointer[gateway.Config], log *slog.Logger) {
	cfg, err := gateway.LoadConfig(path)
	if err == nil {
		err = g.Reload(cfg)
	}
	if err != nil {
		log.Error("config reload failed; keeping the running config", "error", err)
		return
	}
	old := current.Swap(cfg)
	if cfg.Listen != old.Listen || cfg.DatabaseURLEnv != old.DatabaseURLEnv || cfg.AdminTokenEnv != old.AdminTokenEnv ||
		cfg.AdminListen != old.AdminListen || cfg.Timezone != old.Timezone {
		log.Warn("config reloaded; changes to listen, database_url_env, admin_token_env, admin_listen and timezone take effect after a restart")
		return
	}
	log.Info("config reloaded")
}

// hash reads the key from stdin so it never appears in argv or shell history.
func hash() error {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no key on stdin")
	}
	k := strings.TrimSpace(line)
	if k == "" {
		return errors.New("no key on stdin")
	}
	fmt.Println(gateway.HashKey(k))
	return nil
}

// ---- database commands

func dbCommand(name string, args []string, define func(*flag.FlagSet)) (*gateway.Config, *store.Store, *flag.FlagSet, error) {
	fs, path := newFlags(name)
	if define != nil {
		define(fs)
	}
	fs.Parse(args)
	cfg, err := gateway.LoadConfig(*path)
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := openStore(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, st, fs, nil
}

func dbMigrate(args []string) error {
	_, st, _, err := dbCommand("db migrate", args, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	fmt.Println("database is up to date")
	return nil
}

func keyCommand(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "create", "update":
		return keyWrite(args[0], args[1:])
	case "list":
		return keyList(args[1:])
	case "revoke":
		return keyRevoke(args[1:])
	}
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
	return nil
}

type keyFlags struct {
	name, models, owner, rpm, budget, expires *string
}

func (f *keyFlags) define(fs *flag.FlagSet) {
	f.name = fs.String("name", "", "key name")
	f.models = fs.String("models", "", "comma-separated model names")
	f.owner = fs.String("owner", "", "who the key belongs to")
	f.rpm = fs.String("rpm", "", "requests per minute; 0 for no limit")
	f.budget = fs.String("budget", "", "tokens per calendar month; 0 for no budget")
	f.expires = fs.String("expires", "", `last valid day, YYYY-MM-DD, or "never"`)
}

func keyWrite(cmd string, args []string) error {
	var kf keyFlags
	cfg, st, fs, err := dbCommand("key "+cmd, args, kf.define)
	if err != nil {
		return err
	}
	defer st.Close()
	name := *kf.name
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" {
		return errors.New("-name is required")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	ctx := context.Background()
	loc := cfg.Location()

	var s store.KeySettings
	if cmd == "update" {
		k, err := st.Key(ctx, name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		s = store.KeySettings{Owner: k.Owner, Models: k.Models, RPM: k.RPM, MonthlyTokenBudget: k.MonthlyTokenBudget, ExpiresAt: k.ExpiresAt}
	}
	if set["owner"] {
		s.Owner = *kf.owner
	}
	if set["models"] || cmd == "create" {
		s.Models = nil
		known := cfg.ModelNames()
		for _, m := range strings.Split(*kf.models, ",") {
			if m = strings.TrimSpace(m); m == "" {
				continue
			}
			if !slices.Contains(known, m) {
				return fmt.Errorf("unknown model %q; the config defines %s", m, strings.Join(known, ", "))
			}
			s.Models = append(s.Models, m)
		}
	}
	if set["rpm"] {
		if _, err := fmt.Sscan(*kf.rpm, &s.RPM); err != nil {
			return errors.New("-rpm must be a whole number")
		}
	}
	if set["budget"] {
		if s.MonthlyTokenBudget, err = store.ParseTokens(*kf.budget); err != nil {
			return err
		}
	}
	if set["expires"] {
		if s.ExpiresAt, err = store.ParseExpiry(*kf.expires, loc); err != nil {
			return err
		}
	}

	if cmd == "update" {
		if err := st.UpdateKey(ctx, name, s); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Printf("updated %s; the gateway picks it up within %s\n", name, gateway.SyncInterval)
		return nil
	}
	key := gateway.GenerateKey()
	if _, err := st.CreateKey(ctx, name, gateway.HashKey(key), s); err != nil {
		return err
	}
	fmt.Println(key)
	fmt.Fprintf(os.Stderr, "Created %s. This is the only time the key is shown; only its hash is stored.\nThe gateway accepts it within %s.\n", name, gateway.SyncInterval)
	return nil
}

func keyList(args []string) error {
	cfg, st, _, err := dbCommand("key list", args, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	keys, err := st.Keys(ctx)
	if err != nil {
		return err
	}
	loc := cfg.Location()
	used, err := st.TokensSince(ctx, store.MonthStart(time.Now(), loc))
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tOWNER\tMODELS\tRPM\tTOKENS THIS MONTH\tBUDGET\tVALID THROUGH\tSTATUS")
	now := time.Now()
	for _, k := range keys {
		status := "active"
		if k.RevokedAt != nil {
			status = "revoked"
		} else if !k.Active(now) {
			status = "expired"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", k.Name, dash(k.Owner), strings.Join(k.Models, ","),
			dash(nonzero(int64(k.RPM))), used[k.ID], dash(nonzero(k.MonthlyTokenBudget)), dash(store.FormatExpiry(k.ExpiresAt, loc)), status)
	}
	return w.Flush()
}

func keyRevoke(args []string) error {
	var name *string
	_, st, fs, err := dbCommand("key revoke", args, func(fs *flag.FlagSet) { name = fs.String("name", "", "key name") })
	if err != nil {
		return err
	}
	defer st.Close()
	if *name == "" && fs.NArg() > 0 {
		*name = fs.Arg(0)
	}
	if *name == "" {
		return errors.New("-name is required")
	}
	if err := st.RevokeKey(context.Background(), *name); err != nil {
		return fmt.Errorf("%s: %w", *name, err)
	}
	fmt.Printf("revoked %s; it stops working within %s\n", *name, gateway.SyncInterval)
	return nil
}

func usageReport(args []string) error {
	var month *string
	cfg, st, _, err := dbCommand("usage", args, func(fs *flag.FlagSet) { month = fs.String("month", "", "YYYY-MM; the current month if empty") })
	if err != nil {
		return err
	}
	defer st.Close()
	loc := cfg.Location()
	from := store.MonthStart(time.Now(), loc)
	if *month != "" {
		if from, err = time.ParseInLocation("2006-01", *month, loc); err != nil {
			return errors.New("-month must look like 2026-11")
		}
	}
	rows, err := st.Report(context.Background(), from, from.AddDate(0, 1, 0))
	if err != nil {
		return err
	}
	fmt.Printf("%s (%s)\n", from.Format("January 2006"), loc)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "KEY\tMODEL\tREQUESTS\tPROMPT\tCOMPLETION\tTOTAL\t")
	var r, p, c int64
	for _, x := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t\n", x.Key, x.Model, x.Requests, x.PromptTokens, x.CompletionTokens, x.PromptTokens+x.CompletionTokens)
		r, p, c = r+x.Requests, p+x.PromptTokens, c+x.CompletionTokens
	}
	fmt.Fprintf(w, "all\t\t%d\t%d\t%d\t%d\t\n", r, p, c, p+c)
	return w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func nonzero(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}
