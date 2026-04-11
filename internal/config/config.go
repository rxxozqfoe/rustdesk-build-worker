package config

import (
	"fmt"
	"os"

	"github.com/spf13/viper"
)

type Config struct {
	API    API            `mapstructure:"api"`
	S3     S3             `mapstructure:"s3"`
	Build  Build          `mapstructure:"build"`
	Worker WorkerIdentity `mapstructure:"worker"`
}

type API struct {
	BaseURL string `mapstructure:"base-url"` // rustdesk-api URL, e.g. "http://rustdesk-api:21114"
	Token   string `mapstructure:"token"`    // shared secret matching API server's worker.token
}

type S3 struct {
	Endpoint  string `mapstructure:"endpoint"`
	AccessKey string `mapstructure:"access-key"`
	SecretKey string `mapstructure:"secret-key"`
	Bucket    string `mapstructure:"bucket"`
	Region    string `mapstructure:"region"`
	UseSSL    bool   `mapstructure:"use-ssl"`
}

type Build struct {
	RustdeskSrcDir   string `mapstructure:"rustdesk-src-dir"`   // path to rustdesk/ source tree
	WorktreeDir      string `mapstructure:"worktree-dir"`       // git worktree for isolated builds
	LogDir           string `mapstructure:"log-dir"`            // build log output
	SigningPublicKey string `mapstructure:"signing-public-key"` // Ed25519 public key to patch into client
}

type WorkerIdentity struct {
	Name      string           `mapstructure:"name"`
	Platforms []PlatformConfig `mapstructure:"platforms"`
}

type PlatformConfig struct {
	Platform string `mapstructure:"platform"`
	Arch     string `mapstructure:"arch"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	v.SetDefault("build.worktree-dir", "/tmp/build-worktree")
	v.SetDefault("build.log-dir", "/tmp/build-logs")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	if cfg.Worker.Name == "" {
		hostname, _ := os.Hostname()
		cfg.Worker.Name = hostname
	}

	return cfg, nil
}
