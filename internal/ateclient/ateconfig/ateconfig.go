// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ateconfig reads and writes AteConfig files, which tell an ate-api
// client where ate-api is, how to verify it, and how to get a credential for
// it.
package ateconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

const (
	APIVersion = "ate.dev/v1alpha1"
	Kind       = "AteConfig"

	// PathEnv names the AteConfig file when --ateconfig is not given.
	PathEnv = "ATECONFIG"
)

// Config is an AteConfig file.
type Config struct {
	APIVersion     string    `json:"apiVersion"`
	Kind           string    `json:"kind"`
	CurrentContext string    `json:"currentContext,omitempty"`
	Contexts       []Context `json:"contexts"`

	// dir is the directory relative exec commands resolve against.
	dir string
}

// Context is one ate-api deployment and the credential used for it.
type Context struct {
	Name     string   `json:"name"`
	Endpoint Endpoint `json:"endpoint"`
	// TLSServerName is the name checked on ate-api's serving certificate.
	TLSServerName string `json:"tlsServerName,omitempty"`
	// CertificateAuthorityData is a PEM bundle that verifies ate-api. When
	// empty, the client reads the live ClusterTrustBundle through Kubernetes.
	CertificateAuthorityData []byte `json:"certificateAuthorityData,omitempty"`
	// Audience is passed to an exec plugin as spec.cluster.config.audience.
	// Defaults to TLSServerName.
	Audience   string     `json:"audience,omitempty"`
	Credential Credential `json:"credential,omitempty"`
}

// Endpoint says how to reach ate-api. Exactly one field is set.
type Endpoint struct {
	PortForward *PortForward `json:"portForward,omitempty"`
	// Address is a host:port dialed directly.
	Address string `json:"address,omitempty"`
}

// PortForward reaches ate-api through the Kubernetes API server.
type PortForward struct {
	// KubeContext is the kubeconfig context to use. Empty means the
	// kubeconfig's current context.
	KubeContext string `json:"kubeContext,omitempty"`
	Namespace   string `json:"namespace"`
	Service     string `json:"service"`
}

// Credential says how to authenticate to ate-api. At most one field is set.
// With neither, the client mints a token for the default ServiceAccount
// through Kubernetes, as `kubectl ate credential` does.
type Credential struct {
	// Exec is a kubeconfig exec stanza; any kubectl credential plugin works.
	Exec *clientcmdapi.ExecConfig `json:"exec,omitempty"`
	// TokenFile is read on every RPC, so a rotated token is picked up.
	TokenFile string `json:"tokenFile,omitempty"`
}

// Locate returns the AteConfig path to use: flagPath if set, then
// $ATECONFIG, then ~/.ate/config. explicit reports whether the path came from
// flagPath, which callers treat as an error to be missing.
func Locate(flagPath string) (path string, explicit bool, err error) {
	if flagPath != "" {
		return flagPath, true, nil
	}
	if p := os.Getenv(PathEnv); p != "" {
		return p, false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("locate AteConfig: %w", err)
	}
	return filepath.Join(home, ".ate", "config"), false, nil
}

// Load reads and validates the AteConfig at path. The error wraps
// fs.ErrNotExist when the file is missing.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse AteConfig %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid AteConfig %s: %w", path, err)
	}
	if cfg.dir, err = filepath.Abs(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save writes cfg to path, creating its directory. The file is mode 0600
// because an exec stanza may carry secrets in its env.
func Save(path string, cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadOrNew loads path, or returns an empty Config if it does not exist.
func LoadOrNew(path string) (*Config, error) {
	cfg, err := Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{APIVersion: APIVersion, Kind: Kind}, nil
	}
	return cfg, err
}

// Validate checks cfg's structure. It does not check that the endpoint is
// reachable or the credential works.
func (c *Config) Validate() error {
	if c.APIVersion != APIVersion || c.Kind != Kind {
		return fmt.Errorf("apiVersion/kind is %s/%s, want %s/%s", c.APIVersion, c.Kind, APIVersion, Kind)
	}
	names := map[string]bool{}
	for i, ctx := range c.Contexts {
		if ctx.Name == "" {
			return fmt.Errorf("contexts[%d]: name is required", i)
		}
		if names[ctx.Name] {
			return fmt.Errorf("contexts[%d]: duplicate name %q", i, ctx.Name)
		}
		names[ctx.Name] = true
		if err := ctx.validate(); err != nil {
			return fmt.Errorf("context %q: %w", ctx.Name, err)
		}
	}
	if c.CurrentContext != "" && !names[c.CurrentContext] {
		return fmt.Errorf("currentContext %q is not a context", c.CurrentContext)
	}
	return nil
}

func (c *Context) validate() error {
	switch pf := c.Endpoint.PortForward; {
	case pf != nil && c.Endpoint.Address != "":
		return errors.New("endpoint: set one of portForward or address, not both")
	case pf != nil:
		if pf.Namespace == "" || pf.Service == "" {
			return errors.New("endpoint.portForward: namespace and service are required")
		}
	case c.Endpoint.Address != "":
		if c.TLSServerName == "" {
			return errors.New("tlsServerName is required with endpoint.address")
		}
	default:
		return errors.New("endpoint: one of portForward or address is required")
	}
	if c.Credential.Exec != nil && c.Credential.TokenFile != "" {
		return errors.New("credential: set one of exec or tokenFile, not both")
	}
	if e := c.Credential.Exec; e != nil {
		if e.Command == "" {
			return errors.New("credential.exec.command is required")
		}
		// client.authentication.k8s.io/v1 has no default interactiveMode.
		if e.APIVersion == "client.authentication.k8s.io/v1" && e.InteractiveMode == "" {
			return errors.New("credential.exec.interactiveMode is required for client.authentication.k8s.io/v1")
		}
	}
	return nil
}

// Current returns a copy of the current context, ready to use; see Get.
func (c *Config) Current() (*Context, error) {
	if c.CurrentContext == "" {
		return nil, errors.New("AteConfig has no currentContext; run `kubectl ate config use-context`")
	}
	return c.Get(c.CurrentContext)
}

// Get returns a copy of the named context with a relative exec command
// resolved against the file's directory. A bare command name is left for
// $PATH lookup.
func (c *Config) Get(name string) (*Context, error) {
	for _, ctx := range c.Contexts {
		if ctx.Name != name {
			continue
		}
		if e := ctx.Credential.Exec; e != nil {
			e = e.DeepCopy()
			if c.dir != "" && !filepath.IsAbs(e.Command) && strings.ContainsRune(e.Command, filepath.Separator) {
				e.Command = filepath.Join(c.dir, e.Command)
			}
			ctx.Credential.Exec = e
		}
		return &ctx, nil
	}
	return nil, fmt.Errorf("AteConfig has no context %q", name)
}

// Upsert adds ctx, replacing any context with the same name.
func (c *Config) Upsert(ctx Context) {
	for i := range c.Contexts {
		if c.Contexts[i].Name == ctx.Name {
			c.Contexts[i] = ctx
			return
		}
	}
	c.Contexts = append(c.Contexts, ctx)
}
