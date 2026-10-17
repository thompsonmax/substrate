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

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.ate.dev/substrate/cmd/kubectl-ate/internal/credplugin"
	"go.ate.dev/substrate/internal/ateclient"
	"go.ate.dev/substrate/internal/ateclient/ateconfig"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

const credentialInstallHint = "kubectl-ate is the Agent Substrate kubectl plugin; install it with: go install go.ate.dev/substrate/cmd/kubectl-ate@latest"

var (
	configInitName           string
	configInitServiceAccount string
	configInitEmbedCA        bool
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage the AteConfig file that says how kubectl ate reaches ate-api",
	Long: `kubectl ate reads its AteConfig from --ateconfig, else $ATECONFIG, else
~/.ate/config. Without one, or when --kubeconfig or --context is given, it
port-forwards through the kubeconfig context to the default ate-api Service and
authenticates as the shared ate-client ServiceAccount.`,
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Add a context for a Kubernetes cluster to the AteConfig and make it current",
	Long: `init writes a context that port-forwards through a kubeconfig context (--context,
or the current one) and authenticates with ` + "`kubectl ate credential`" + `. It replaces any
context with the same name.`,
	Example: `  kubectl ate config init --context=gke_proj_us-central1_prod
  kubectl ate config init --service-account=ate-users/alice`,
	Args: noArgs(""),
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, _, err := ateconfig.Locate(ateconfigPath)
		if err != nil {
			return err
		}
		cfg, err := ateconfig.LoadOrNew(path)
		if err != nil {
			return err
		}

		kubeContext := k8sContext
		if kubeContext == "" {
			if kubeContext, err = currentKubeContext(); err != nil {
				return err
			}
		}
		c := ateclient.DefaultContext(kubeContext)
		c.Name = kubeContext
		if configInitName != "" {
			c.Name = configInitName
		}
		args := []string{"credential", "--context=" + kubeContext}
		if kubeconfig != "" {
			abs, err := filepath.Abs(kubeconfig)
			if err != nil {
				return err
			}
			args = append(args, "--kubeconfig="+abs)
		}
		if configInitServiceAccount != "" {
			args = append(args, "--service-account="+configInitServiceAccount)
		}
		c.Credential.Exec = &clientcmdapi.ExecConfig{
			APIVersion:         credplugin.APIVersionV1,
			Command:            "kubectl-ate",
			Args:               args,
			InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
			ProvideClusterInfo: true,
			InstallHint:        credentialInstallHint,
		}

		if configInitEmbedCA {
			config, err := loadKubeConfig(cmd.ErrOrStderr(), kubeconfig, kubeContext)
			if err != nil {
				return fmt.Errorf("while reading kubeconfig: %w", err)
			}
			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				return err
			}
			if c.CertificateAuthorityData, err = ateclient.LiveTrustBundle(cmd.Context(), clientset); err != nil {
				return err
			}
		}

		cfg.Upsert(*c)
		cfg.CurrentContext = c.Name
		if err := ateconfig.Save(path, cfg); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Wrote context %q to %s and made it the current context.\n", c.Name, path)
		return nil
	},
}

var configViewCmd = &cobra.Command{
	Use:   "view",
	Short: "Print the AteConfig, or the default used in its place",
	Args:  noArgs(""),
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, explicit, err := ateconfig.Locate(ateconfigPath)
		if err != nil {
			return err
		}
		cfg, err := ateconfig.Load(path)
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			fmt.Fprintf(cmd.ErrOrStderr(), "No AteConfig at %s; showing the default used in its place.\n", path)
			c := ateclient.DefaultContext(k8sContext)
			cfg = &ateconfig.Config{APIVersion: ateconfig.APIVersion, Kind: ateconfig.Kind, CurrentContext: c.Name, Contexts: []ateconfig.Context{*c}}
		} else if err != nil {
			return err
		}

		var b []byte
		if outputFmt == "json" {
			b, err = json.MarshalIndent(cfg, "", "  ")
			b = append(b, '\n')
		} else {
			b, err = yaml.Marshal(cfg)
		}
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(b)
		return err
	},
}

var configUseContextCmd = &cobra.Command{
	Use:   "use-context NAME",
	Short: "Set the AteConfig's current context",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, _, err := ateconfig.Locate(ateconfigPath)
		if err != nil {
			return err
		}
		cfg, err := ateconfig.Load(path)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no AteConfig at %s; create one with `kubectl ate config init`", path)
		} else if err != nil {
			return err
		}
		if _, err := cfg.Get(args[0]); err != nil {
			return err
		}
		cfg.CurrentContext = args[0]
		if err := ateconfig.Save(path, cfg); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Switched to context %q.\n", args[0])
		return nil
	},
}

// currentKubeContext returns the kubeconfig's current context.
func currentKubeContext() (string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return "", fmt.Errorf("while reading kubeconfig: %w", err)
	}
	if raw.CurrentContext == "" {
		return "", usageErrorf("the kubeconfig has no current context; pass --context")
	}
	return raw.CurrentContext, nil
}

// resolveAPIContext returns the context newAPIClient dials, and the AteConfig
// path it came from, or "" for the default context.
func resolveAPIContext() (*ateconfig.Context, string, error) {
	if kubeconfig != "" || k8sContext != "" {
		if ateconfigPath != "" {
			return nil, "", usageErrorf("--ateconfig cannot be combined with --kubeconfig or --context")
		}
		return ateclient.DefaultContext(k8sContext), "", nil
	}
	path, explicit, err := ateconfig.Locate(ateconfigPath)
	if err != nil {
		return nil, "", err
	}
	cfg, err := ateconfig.Load(path)
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		return ateclient.DefaultContext(""), "", nil
	} else if err != nil {
		return nil, "", err
	}
	c, err := cfg.Current()
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return c, path, nil
}

func init() {
	requireSubcommand(configCmd)
	rootCmd.AddCommand(configCmd)

	configInitCmd.Flags().StringVar(&configInitName, "name", "", "name of the context to write (defaults to the kubeconfig context name)")
	configInitCmd.Flags().StringVar(&configInitServiceAccount, "service-account", "", "NAMESPACE/NAME of a ServiceAccount to authenticate as instead of the shared ate-client")
	configInitCmd.Flags().BoolVar(&configInitEmbedCA, "embed-certificate-authority", false, "copy ate-api's current trust bundle into the context, so it needs no Kubernetes access to verify ate-api; it must be rewritten when the bundle rotates")
	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configViewCmd)
	configCmd.AddCommand(configUseContextCmd)
}
