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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.ate.dev/substrate/internal/ateclient/ateconfig"
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: kind-a
clusters:
- name: a
  cluster: {server: "https://a.invalid"}
- name: b
  cluster: {server: "https://b.invalid"}
users:
- name: u
  user: {token: kube-token}
contexts:
- name: kind-a
  context: {cluster: a, user: u}
- name: kind-b
  context: {cluster: b, user: u}
`

// setupConfigEnv points KUBECONFIG, ATECONFIG and HOME at fresh files and
// returns the AteConfig path, which does not exist yet.
func setupConfigEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfigPath, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	path := filepath.Join(dir, "ateconfig")
	t.Setenv(ateconfig.PathEnv, path)
	return path
}

// runRoot executes rootCmd with args and resets every flag afterwards.
func runRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	defer func() {
		resetFlags(rootCmd)
		resetRootFlags(t)
	}()
	err = rootCmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func resetFlags(cmd *cobra.Command) {
	reset := func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	}
	cmd.Flags().VisitAll(reset)
	cmd.PersistentFlags().VisitAll(reset)
	for _, c := range cmd.Commands() {
		resetFlags(c)
	}
}

func TestConfigInitAndUseContext(t *testing.T) {
	path := setupConfigEnv(t)

	out, _, err := runRoot(t, "config", "init")
	if err != nil {
		t.Fatalf("config init: %v", err)
	}
	if !strings.Contains(out, `"kind-a"`) {
		t.Errorf("config init output = %q, want it to name kind-a", out)
	}
	if _, _, err := runRoot(t, "config", "init", "--context=kind-b", "--name=b-alice", "--service-account=ate-users/alice"); err != nil {
		t.Fatalf("config init --context=kind-b: %v", err)
	}

	cfg, err := ateconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CurrentContext != "b-alice" {
		t.Errorf("currentContext = %q, want b-alice", cfg.CurrentContext)
	}
	a, err := cfg.Get("kind-a")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.Endpoint.PortForward, (&ateconfig.PortForward{KubeContext: "kind-a", Namespace: "ate-system", Service: "api"}); !cmp.Equal(got, want) {
		t.Errorf("kind-a portForward = %+v, want %+v", got, want)
	}
	if got, want := a.Credential.Exec.Args, []string{"credential", "--context=kind-a"}; !cmp.Equal(got, want) {
		t.Errorf("kind-a exec args = %q, want %q", got, want)
	}
	if a.Audience != "api.ate-system.svc" || a.TLSServerName != "api.ate-system.svc" {
		t.Errorf("kind-a audience/tlsServerName = %q/%q", a.Audience, a.TLSServerName)
	}
	b, err := cfg.Get("b-alice")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.Credential.Exec.Args, []string{"credential", "--context=kind-b", "--service-account=ate-users/alice"}; !cmp.Equal(got, want) {
		t.Errorf("b-alice exec args = %q, want %q", got, want)
	}

	if _, _, err := runRoot(t, "config", "use-context", "kind-a"); err != nil {
		t.Fatalf("config use-context: %v", err)
	}
	if cfg, err = ateconfig.Load(path); err != nil || cfg.CurrentContext != "kind-a" {
		t.Errorf("after use-context: currentContext = %q, err = %v; want kind-a", cfg.CurrentContext, err)
	}
	if _, _, err := runRoot(t, "config", "use-context", "nope"); err == nil {
		t.Error("config use-context nope succeeded, want error")
	}
}

func TestConfigUseContextWithoutFile(t *testing.T) {
	setupConfigEnv(t)
	if _, _, err := runRoot(t, "config", "use-context", "kind-a"); err == nil || !strings.Contains(err.Error(), "config init") {
		t.Errorf("config use-context without a file = %v, want an error pointing at config init", err)
	}
}

func TestConfigView(t *testing.T) {
	path := setupConfigEnv(t)
	t.Setenv(ateconfig.PathEnv, "")
	t.Setenv("HOME", filepath.Dir(path))

	out, errOut, err := runRoot(t, "config", "view")
	if err != nil {
		t.Fatalf("config view: %v", err)
	}
	if !strings.Contains(errOut, "No AteConfig") {
		t.Errorf("config view stderr = %q, want a note that the default is shown", errOut)
	}
	for _, want := range []string{"kind: AteConfig", "currentContext: default", "service: api"} {
		if !strings.Contains(out, want) {
			t.Errorf("config view output does not contain %q:\n%s", want, out)
		}
	}

	if _, _, err := runRoot(t, "config", "init"); err != nil {
		t.Fatal(err)
	}
	out, _, err = runRoot(t, "config", "view", "-o", "json")
	if err != nil {
		t.Fatalf("config view -o json: %v", err)
	}
	if !strings.Contains(out, `"currentContext": "kind-a"`) {
		t.Errorf("config view -o json output:\n%s", out)
	}
}

func TestResolveAPIContext(t *testing.T) {
	path := setupConfigEnv(t)

	var got *ateconfig.Context
	var gotSource string
	probe := &cobra.Command{
		Use: "probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			got, gotSource, err = resolveAPIContext()
			return err
		},
	}
	rootCmd.AddCommand(probe)
	t.Cleanup(func() { rootCmd.RemoveCommand(probe) })

	if _, _, err := runRoot(t, "probe"); err != nil {
		t.Fatalf("without a file: %v", err)
	}
	if got.Name != "default" || got.Credential.Exec != nil || gotSource != "" {
		t.Errorf("without a file: context %q from %q, want the default", got.Name, gotSource)
	}

	if _, _, err := runRoot(t, "config", "init", "--context=kind-b"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runRoot(t, "probe"); err != nil {
		t.Fatalf("with a file: %v", err)
	}
	if got.Name != "kind-b" || gotSource != path {
		t.Errorf("with a file: context %q from %q, want kind-b from %s", got.Name, gotSource, path)
	}

	if _, _, err := runRoot(t, "probe", "--context=kind-a"); err != nil {
		t.Fatalf("with --context: %v", err)
	}
	if got.Name != "default" || got.Endpoint.PortForward.KubeContext != "kind-a" {
		t.Errorf("with --context: context %q through %q, want the default through kind-a", got.Name, got.Endpoint.PortForward.KubeContext)
	}

	if _, _, err := runRoot(t, "probe", "--context=kind-a", "--ateconfig="+path); err == nil {
		t.Error("--context with --ateconfig succeeded, want error")
	}
	if _, _, err := runRoot(t, "probe", "--ateconfig="+path+".missing"); err == nil {
		t.Error("missing --ateconfig succeeded, want error")
	}
}
