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
	"strings"
	"syscall"
	"time"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/rest"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serveCmd(os.Args[2:], log)
	case "user":
		err = userCmd(os.Args[2:])
	case "invite":
		err = inviteCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `voidbar - Discord-compatible IRCv3 bouncer

Usage:
  voidbar serve  [--config <path>]
  voidbar user   add  --username <name> --email <addr> [--password <pass>] [--admin] [--config <path>]
  voidbar user   list [--config <path>]
  voidbar invite create [--uses N] [--by <user-id>] [--config <path>]
  voidbar invite list  [--config <path>]
`)
}

func serveCmd(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if _, err := util.LoadOrCreateMasterKey(cfg.MasterKeyPath()); err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer store.Close()
	sf := util.NewSnowflake(0, 0)
	authSvc := auth.New(store, sf, cfg.Auth.Registration)
	gw := gateway.New(authSvc, cfg, log)
	handler := rest.New(authSvc, cfg, log, gw)
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	log.Info("voidbar listening", "addr", cfg.Server.Listen, "public_url", cfg.Server.PublicURL, "registration", cfg.Auth.Registration)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func userCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("user requires a subcommand: add | list")
	}
	switch args[0] {
	case "add":
		return userAddCmd(args[1:])
	case "list":
		return userListCmd(args[1:])
	default:
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
}

func userAddCmd(args []string) error {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file")
	username := fs.String("username", "", "username")
	email := fs.String("email", "", "email")
	password := fs.String("password", "", "password (omit to read from stdin)")
	admin := fs.Bool("admin", false, "make user an admin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" || *email == "" {
		return errors.New("--username and --email are required")
	}
	pass := *password
	if pass == "" {
		fmt.Print("password: ")
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		pass = strings.TrimSpace(line)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer store.Close()
	hash, err := util.HashPassword(pass)
	if err != nil {
		return err
	}
	if len(pass) < 8 || len(pass) > 128 {
		return auth.ErrInvalidPassword
	}
	if !strings.EqualFold(*email, strings.ToLower(*email)) {
		return errors.New("email must be lowercase")
	}
	count, err := store.UserCount()
	if err != nil {
		return err
	}
	isAdmin := *admin || count == 0
	u := &storage.User{
		ID:        util.NewSnowflake(0, 0).New(),
		Username:  strings.ToLower(*username),
		Email:     strings.ToLower(*email),
		PassHash:  hash,
		IsAdmin:   isAdmin,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateUser(u); err != nil {
		return err
	}
	fmt.Printf("created user %s (username=%s admin=%v)\n", u.ID, u.Username, u.IsAdmin)
	return nil
}

func userListCmd(args []string) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer store.Close()
	users, err := store.ListUsers()
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Println("no users")
		return nil
	}
	fmt.Printf("%-20s %-32s %-30s %-6s %s\n", "ID", "USERNAME", "EMAIL", "ADMIN", "CREATED")
	for _, u := range users {
		fmt.Printf("%-20s %-32s %-30s %-6v %s\n", u.ID, u.Username, u.Email, u.IsAdmin, u.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func inviteCmd(args []string) error {
	if len(args) < 1 {
		return errors.New("invite requires a subcommand: create | list")
	}
	switch args[0] {
	case "create":
		return inviteCreateCmd(args[1:])
	case "list":
		return inviteListCmd(args[1:])
	default:
		return fmt.Errorf("unknown invite subcommand %q", args[0])
	}
}

func inviteCreateCmd(args []string) error {
	fs := flag.NewFlagSet("invite create", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file")
	uses := fs.Int("uses", 0, "max uses (0 = unlimited)")
	by := fs.String("by", "", "creating user id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer store.Close()
	code, err := util.RandomToken(5)
	if err != nil {
		return err
	}
	inv := &storage.RegInvite{
		Code:      code,
		CreatedBy: *by,
		MaxUses:   *uses,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateRegInvite(inv); err != nil {
		return err
	}
	fmt.Printf("created invite %s (uses=%d)\n", inv.Code, inv.MaxUses)
	return nil
}

func inviteListCmd(args []string) error {
	fs := flag.NewFlagSet("invite list", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer store.Close()
	invites, err := store.ListRegInvites()
	if err != nil {
		return err
	}
	if len(invites) == 0 {
		fmt.Println("no invites")
		return nil
	}
	fmt.Printf("%-12s %-8s %-8s %s\n", "CODE", "USES", "MAX", "CREATED")
	for _, inv := range invites {
		max := "inf"
		if inv.MaxUses > 0 {
			max = fmt.Sprintf("%d", inv.MaxUses)
		}
		fmt.Printf("%-12s %-8d %-8s %s\n", inv.Code, inv.Uses, max, inv.CreatedAt.Format(time.RFC3339))
	}
	return nil
}
