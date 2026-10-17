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

package ateconfig

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const exampleConfig = `apiVersion: ate.dev/v1alpha1
kind: AteConfig
currentContext: prod
contexts:
- name: prod
  endpoint:
    portForward: {kubeContext: gke_proj_us-central1_prod, namespace: ate-system, service: api}
  tlsServerName: api.ate-system.svc
  certificateAuthorityData: LS0tLS1CRUdJTg==
  audience: api.ate-system.svc
  credential:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kubectl-ate
      args: [credential, --context=gke_proj_us-central1_prod]
      interactiveMode: Never
      provideClusterInfo: true
- name: relative
  endpoint:
    address: ate.example.com:443
  tlsServerName: ate.example.com
  credential:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: ./bin/plugin
- name: in-cluster
  endpoint:
    address: api.ate-system.svc:443
  tlsServerName: api.ate-system.svc
  credential:
    tokenFile: /var/run/secrets/ate/token
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeFile(t, exampleConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cur, err := cfg.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	want := &Context{
		Name: "prod",
		Endpoint: Endpoint{PortForward: &PortForward{
			KubeContext: "gke_proj_us-central1_prod", Namespace: "ate-system", Service: "api",
		}},
		TLSServerName:            "api.ate-system.svc",
		CertificateAuthorityData: []byte("-----BEGIN"),
		Audience:                 "api.ate-system.svc",
		Credential: Credential{Exec: &clientcmdapi.ExecConfig{
			APIVersion:         "client.authentication.k8s.io/v1",
			Command:            "kubectl-ate",
			Args:               []string{"credential", "--context=gke_proj_us-central1_prod"},
			InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
			ProvideClusterInfo: true,
		}},
	}
	if diff := cmp.Diff(want, cur, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Current() (-want +got):\n%s", diff)
	}

	rel, err := cfg.Get("relative")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, want := rel.Credential.Exec.Command, filepath.Join(filepath.Dir(path), "bin", "plugin"); got != want {
		t.Errorf("relative command = %q, want %q", got, want)
	}
	if got := cfg.Contexts[1].Credential.Exec.Command; got != "./bin/plugin" {
		t.Errorf("Get modified the stored command to %q", got)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load(missing) = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadInvalid(t *testing.T) {
	header := "apiVersion: ate.dev/v1alpha1\nkind: AteConfig\n"
	for _, tc := range []struct {
		name, content, wantErr string
	}{
		{"wrong kind", "apiVersion: v1\nkind: Config\n", "apiVersion/kind"},
		{"unknown field", header + "bogus: true\n", "bogus"},
		{"no endpoint", header + "contexts:\n- name: a\n", "one of portForward or address"},
		{"both endpoints", header + "contexts:\n- name: a\n  tlsServerName: x\n  endpoint: {address: x:1, portForward: {namespace: n, service: s}}\n", "not both"},
		{"address without server name", header + "contexts:\n- name: a\n  endpoint: {address: x:1}\n", "tlsServerName"},
		{"duplicate", header + "contexts:\n- name: a\n  endpoint: {portForward: {namespace: n, service: s}}\n- name: a\n  endpoint: {portForward: {namespace: n, service: s}}\n", "duplicate"},
		{"dangling current", header + "currentContext: b\n", "currentContext"},
		{"v1 without interactiveMode", header + "contexts:\n- name: a\n  endpoint: {portForward: {namespace: n, service: s}}\n  credential:\n    exec: {apiVersion: client.authentication.k8s.io/v1, command: x}\n", "interactiveMode"},
		{"exec and tokenFile", header + "contexts:\n- name: a\n  endpoint: {portForward: {namespace: n, service: s}}\n  credential:\n    tokenFile: /t\n    exec: {apiVersion: client.authentication.k8s.io/v1beta1, command: x}\n", "not both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeFile(t, tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Load = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	cfg, err := Load(writeFile(t, exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Upsert(Context{Name: "new", Endpoint: Endpoint{PortForward: &PortForward{Namespace: "n", Service: "s"}}})
	cfg.CurrentContext = "new"

	path := filepath.Join(t.TempDir(), "sub", "config")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if diff := cmp.Diff(cfg, got, cmpopts.IgnoreUnexported(Config{}), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}
	if got := got.Contexts[1].Credential.Exec.Command; got != "./bin/plugin" {
		t.Errorf("relative command saved as %q, want it kept relative", got)
	}
}

func TestLocate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv(PathEnv, "")
	if path, explicit, _ := Locate(""); path != filepath.Join(home, ".ate", "config") || explicit {
		t.Errorf("Locate default = (%q, %t)", path, explicit)
	}
	t.Setenv(PathEnv, "/env/config")
	if path, explicit, _ := Locate(""); path != "/env/config" || explicit {
		t.Errorf("Locate env = (%q, %t)", path, explicit)
	}
	if path, explicit, _ := Locate("/flag/config"); path != "/flag/config" || !explicit {
		t.Errorf("Locate flag = (%q, %t)", path, explicit)
	}
}
