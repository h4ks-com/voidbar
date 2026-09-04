package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
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
	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/rest"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func main() {
	logLevel := slog.LevelInfo
	if v := os.Getenv("VOIDBAR_LOG_LEVEL"); v != "" {
		if err := logLevel.UnmarshalText([]byte(v)); err != nil {
			logLevel = slog.LevelInfo
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
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

Backend only: point a Discord client (e.g. the repackaged Android build)
at the instance's REST/Gateway endpoints.
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
	masterKey, err := util.LoadOrCreateMasterKey(cfg.MasterKeyPath())
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer store.Close()
	sf := util.NewSnowflake(0, 0)
	authSvc := auth.New(store, sf, cfg.Auth.Registration)
	// The master key arms the /api/v9/admin/users endpoints so user
	// provisioning works against the running instance (the CLI's
	// direct-storage path deadlocks on badger's lock while serve holds
	// it).
	authSvc.SetAdminKey(masterKey)
	gw := gateway.New(authSvc, cfg, log, nil, nil)
	manager := ircmanage.New(store, gw, log, sf)
	netSvc := network.NewService(store, gw, sf, manager, log)
	manager.EnsureAll()
	// A single gateway instance: the manager dispatches IRC events into it,
	// and the network service supplies the READY/GUILD_CREATE payloads.
	gw.SetGuildProviders(
		func(u string) ([]any, error) { return netSvc.ReadyGuildPayloads(u), nil },
		netSvc.GuildCreateForUser,
	)
	gw.SetDMChannelsProvider(netSvc.DMChannelPayloads)
	gw.SetSettingsProvider(netSvc.UserSettings)
	gw.SetNotesProvider(netSvc.UserNotes)
	gw.SetReadStateProvider(netSvc.ReadStateEntries)
	gw.SetMemberListProvider(netSvc.MemberListPayload)
	gw.SetMemberChunkProvider(netSvc.MemberChunkPayload)
	manager.SetOccupancyNotifier(netSvc.RefreshOccupancy)
	manager.SetMemberNotifier(netSvc.RefreshMember)
	manager.SetLinkNotifier(netSvc.OnLinkChange)
	manager.SetPeerAvatarNotifier(netSvc.RefreshPeerAvatar)
	// Upstream draft/ICON network icons are mirrored into the avatar
	// store and re-announced as GUILD_UPDATE.
	manager.SetIconNotifier(netSvc.OnNetworkIcon)
	// Self-mentions bump the read-state badge so pings survive restarts.
	manager.SetMentionNotifier(netSvc.OnMentionRelayed)
	// Arm the embed media proxy: unfurled images are mirrored locally
	// and served from the public origin (see SetPublicURL).
	manager.SetPublicURL(cfg.Server.PublicURL)
	// The network service mints avatar URLs published upstream from the
	// same origin.
	netSvc.SetPublicURL(cfg.Server.PublicURL)
	restHandler := rest.New(authSvc, cfg, log, gw, netSvc, manager)

	root := http.NewServeMux()
	root.Handle("/api/", restHandler)
	root.Handle("/health", restHandler)
	root.Handle("/gateway", restHandler)
	// Discord clients append "/?v=9&encoding=json" to GATEWAY_ENDPOINT,
	// producing /gateway/ - must reach the WS handler, not the web catch-all.
	root.Handle("/gateway/", restHandler)
	root.Handle("/remote-auth", restHandler)
	root.Handle("/remote-auth/", restHandler)
	root.Handle("/", restHandler)
	handler := root
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

// adminKeyFor resolves the master key for remote admin calls: explicit
// flag, env, or the instance's master.key file next to the storage.
func adminKeyFor(flagKey, configPath string) (string, error) {
	if flagKey != "" {
		return flagKey, nil
	}
	if env := os.Getenv("VOIDBAR_ADMIN_KEY"); env != "" {
		return env, nil
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", err
	}
	key, err := util.LoadOrCreateMasterKey(cfg.MasterKeyPath())
	if err != nil {
		return "", err
	}
	// The key file holds 32 raw bytes; the API speaks its hex form
	// (raw bytes are not header-safe).
	return hex.EncodeToString(key), nil
}

func userAddCmd(args []string) error {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file")
	username := fs.String("username", "", "username")
	email := fs.String("email", "", "email")
	password := fs.String("password", "", "password (omit to read from stdin)")
	admin := fs.Bool("admin", false, "make user an admin")
	server := fs.String("server", "", "create via a RUNNING voidbar at this base URL (badger's lock blocks direct storage access while serve holds it)")
	keyFlag := fs.String("master-key", "", "master key for --server (defaults to $VOIDBAR_ADMIN_KEY or the config's master.key)")
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
	if *server != "" {
		key, err := adminKeyFor(*keyFlag, *configPath)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"username": *username, "email": *email, "password": pass, "admin": *admin})
		req, err := http.NewRequest("POST", strings.TrimSuffix(*server, "/")+"/api/v9/admin/users", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Master-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var out struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Admin    bool   `json:"admin"`
			Message  string `json:"message"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return err
		}
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("server refused (%d): %s", resp.StatusCode, out.Message)
		}
		fmt.Printf("created user %s (username=%s admin=%v)\n", out.ID, out.Username, out.Admin)
		return nil
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("%w\nstorage is locked by a running voidbar? pass --server <base-url> to provision through the admin API", err)
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
	server := fs.String("server", "", "list via a RUNNING voidbar at this base URL")
	keyFlag := fs.String("master-key", "", "master key for --server (defaults to $VOIDBAR_ADMIN_KEY or the config's master.key)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *server != "" {
		key, err := adminKeyFor(*keyFlag, *configPath)
		if err != nil {
			return err
		}
		req, err := http.NewRequest("GET", strings.TrimSuffix(*server, "/")+"/api/v9/admin/users", nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Master-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Email    string `json:"email"`
			Admin    bool   `json:"admin"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("server refused (%d)", resp.StatusCode)
		}
		if len(users) == 0 {
			fmt.Println("no users")
			return nil
		}
		fmt.Printf("%-20s %-32s %-30s %-6s\n", "ID", "USERNAME", "EMAIL", "ADMIN")
		for _, u := range users {
			fmt.Printf("%-20s %-32s %-30s %-6v\n", u.ID, u.Username, u.Email, u.Admin)
		}
		return nil
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("%w\nstorage is locked by a running voidbar? pass --server <base-url> to read through the admin API", err)
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

