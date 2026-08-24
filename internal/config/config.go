package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server  Server  `toml:"server"`
	Storage Storage `toml:"storage"`
	Auth    Auth    `toml:"auth"`
}

type Server struct {
	Listen    string `toml:"listen"`
	PublicURL string `toml:"public_url"`
}

type Storage struct {
	Path string `toml:"path"`
}

type Auth struct {
	Registration  string `toml:"registration"`
	MasterKeyPath string `toml:"master_key_path"`
}

func Default() *Config {
	return &Config{
		Server:  Server{Listen: "127.0.0.1:8080", PublicURL: "http://127.0.0.1:8080"},
		Storage: Storage{Path: "./data"},
		Auth:    Auth{Registration: "closed"},
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("config file: %w", err)
		}
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}
	applyEnv(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	set := func(dst *string, env string) {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			*dst = v
		}
	}
	set(&cfg.Server.Listen, "VOIDBAR_SERVER_LISTEN")
	set(&cfg.Server.PublicURL, "VOIDBAR_SERVER_PUBLIC_URL")
	set(&cfg.Storage.Path, "VOIDBAR_STORAGE_PATH")
	set(&cfg.Auth.Registration, "VOIDBAR_AUTH_REGISTRATION")
	set(&cfg.Auth.MasterKeyPath, "VOIDBAR_AUTH_MASTER_KEY_PATH")
}

func (c *Config) Validate() error {
	switch c.Auth.Registration {
	case "open", "closed", "invite":
	default:
		return fmt.Errorf("auth.registration must be one of open, closed, invite; got %q", c.Auth.Registration)
	}
	if c.Server.Listen == "" {
		return errors.New("server.listen must not be empty")
	}
	if !strings.HasPrefix(c.Server.PublicURL, "http://") && !strings.HasPrefix(c.Server.PublicURL, "https://") {
		return errors.New("server.public_url must start with http:// or https://")
	}
	if c.Storage.Path == "" {
		return errors.New("storage.path must not be empty")
	}
	return nil
}

func (c *Config) MasterKeyPath() string {
	if c.Auth.MasterKeyPath != "" {
		return c.Auth.MasterKeyPath
	}
	return filepath.Join(c.Storage.Path, "master.key")
}

func (c *Config) GatewayWSURL() string {
	switch {
	case strings.HasPrefix(c.Server.PublicURL, "https://"):
		return "wss://" + strings.TrimPrefix(c.Server.PublicURL, "https://") + "/gateway"
	case strings.HasPrefix(c.Server.PublicURL, "http://"):
		return "ws://" + strings.TrimPrefix(c.Server.PublicURL, "http://") + "/gateway"
	}
	return c.Server.PublicURL + "/gateway"
}
