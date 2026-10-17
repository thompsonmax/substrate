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

package ateclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.ate.dev/substrate/internal/ateclient/ateconfig"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientauthv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// The test binary doubles as an exec credential plugin when pluginEnv is set.
// Each run increments a counter in pluginStateEnv's directory and returns
// "token-<n>", or always "token-1" when pluginSameTokenEnv is set.
const (
	pluginEnv          = "ATECLIENT_TEST_PLUGIN"
	pluginStateEnv     = "ATECLIENT_TEST_PLUGIN_STATE"
	pluginTTLEnv       = "ATECLIENT_TEST_PLUGIN_TTL"
	pluginSameTokenEnv = "ATECLIENT_TEST_PLUGIN_SAME_TOKEN"
	pluginCertEnv      = "ATECLIENT_TEST_PLUGIN_CERT"
)

func TestMain(m *testing.M) {
	if os.Getenv(pluginEnv) != "" {
		if err := runTestPlugin(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runTestPlugin() error {
	dir := os.Getenv(pluginStateEnv)
	n := 1
	if b, err := os.ReadFile(filepath.Join(dir, "count")); err == nil {
		n, _ = strconv.Atoi(string(b))
		n++
	}
	if err := os.WriteFile(filepath.Join(dir, "count"), []byte(strconv.Itoa(n)), 0o600); err != nil {
		return err
	}
	info := os.Getenv("KUBERNETES_EXEC_INFO")
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("info-%d.json", n)), []byte(info), 0o600); err != nil {
		return err
	}
	var in clientauthv1.ExecCredential
	if err := json.Unmarshal([]byte(info), &in); err != nil {
		return err
	}

	token := fmt.Sprintf("token-%d", n)
	if os.Getenv(pluginSameTokenEnv) != "" {
		token = "token-1"
	}
	out := clientauthv1.ExecCredential{
		TypeMeta: in.TypeMeta,
		Status:   &clientauthv1.ExecCredentialStatus{Token: token},
	}
	if ttl := os.Getenv(pluginTTLEnv); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return err
		}
		exp := metav1.NewTime(time.Now().Add(d))
		out.Status.ExpirationTimestamp = &exp
	}
	if certDir := os.Getenv(pluginCertEnv); certDir != "" {
		cert, err := os.ReadFile(filepath.Join(certDir, "tls.crt"))
		if err != nil {
			return err
		}
		key, err := os.ReadFile(filepath.Join(certDir, "tls.key"))
		if err != nil {
			return err
		}
		out.Status.ClientCertificateData, out.Status.ClientKeyData = string(cert), string(key)
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

// testPlugin is an exec stanza that runs the test binary as a plugin.
func testPlugin(t *testing.T, apiVersion string, env ...clientcmdapi.ExecEnvVar) (*clientcmdapi.ExecConfig, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &clientcmdapi.ExecConfig{
		APIVersion:         apiVersion,
		Command:            self,
		Env:                append([]clientcmdapi.ExecEnvVar{{Name: pluginEnv, Value: "1"}, {Name: pluginStateEnv, Value: dir}}, env...),
		InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
		ProvideClusterInfo: true,
	}, dir
}

func pluginRuns(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "count"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(string(b))
	return n
}

func authorization(t *testing.T, creds credentials.PerRPCCredentials) string {
	t.Helper()
	md, err := creds.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	return md["authorization"]
}

func TestExecCreds(t *testing.T) {
	for _, apiVersion := range []string{"client.authentication.k8s.io/v1", "client.authentication.k8s.io/v1beta1"} {
		t.Run(apiVersion, func(t *testing.T) {
			plugin, dir := testPlugin(t, apiVersion, clientcmdapi.ExecEnvVar{Name: pluginTTLEnv, Value: "1h"})
			cluster, err := execCluster("https://api.ate-system.svc", "api.ate-system.svc", nil, "my-audience")
			if err != nil {
				t.Fatal(err)
			}
			creds, err := newExecCreds(plugin, cluster)
			if err != nil {
				t.Fatalf("newExecCreds: %v", err)
			}

			if got := authorization(t, creds); got != "Bearer token-1" {
				t.Errorf("authorization = %q, want Bearer token-1", got)
			}
			if got := authorization(t, creds); got != "Bearer token-1" {
				t.Errorf("second authorization = %q, want the cached Bearer token-1", got)
			}
			if n := pluginRuns(t, dir); n != 1 {
				t.Errorf("plugin ran %d times, want 1", n)
			}

			b, err := os.ReadFile(filepath.Join(dir, "info-1.json"))
			if err != nil {
				t.Fatal(err)
			}
			var info clientauthv1.ExecCredential
			if err := json.Unmarshal(b, &info); err != nil {
				t.Fatal(err)
			}
			if info.APIVersion != apiVersion {
				t.Errorf("KUBERNETES_EXEC_INFO apiVersion = %q, want %q", info.APIVersion, apiVersion)
			}
			var config struct{ Audience string }
			if err := json.Unmarshal(info.Spec.Cluster.Config.Raw, &config); err != nil {
				t.Fatalf("spec.cluster.config: %v", err)
			}
			if config.Audience != "my-audience" {
				t.Errorf("spec.cluster.config.audience = %q, want my-audience", config.Audience)
			}

			if !creds.refresh("Bearer token-1") {
				t.Error("refresh = false, want true for a plugin that returns a new token")
			}
			if got := authorization(t, creds); got != "Bearer token-2" {
				t.Errorf("authorization after refresh = %q, want Bearer token-2", got)
			}
			if n := pluginRuns(t, dir); n != 2 {
				t.Errorf("plugin ran %d times, want 2", n)
			}
		})
	}
}

func TestExecCredsExpiry(t *testing.T) {
	plugin, dir := testPlugin(t, "client.authentication.k8s.io/v1", clientcmdapi.ExecEnvVar{Name: pluginTTLEnv, Value: "-1s"})
	creds, err := newExecCreds(plugin, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorization(t, creds)
	if got := authorization(t, creds); got != "Bearer token-2" {
		t.Errorf("authorization after expiry = %q, want Bearer token-2", got)
	}
	if n := pluginRuns(t, dir); n != 2 {
		t.Errorf("plugin ran %d times, want 2", n)
	}
}

func TestExecCredsRefreshSameToken(t *testing.T) {
	plugin, _ := testPlugin(t, "client.authentication.k8s.io/v1", clientcmdapi.ExecEnvVar{Name: pluginSameTokenEnv, Value: "1"})
	creds, err := newExecCreds(plugin, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorization(t, creds)
	if creds.refresh("Bearer token-1") {
		t.Error("refresh = true, want false when the plugin returns the rejected token again")
	}
}

func TestExecCredsRefreshAlreadyRotated(t *testing.T) {
	plugin, dir := testPlugin(t, "client.authentication.k8s.io/v1")
	creds, err := newExecCreds(plugin, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorization(t, creds)
	if !creds.refresh("Bearer token-0") {
		t.Error("refresh = false, want true")
	}
	if n := pluginRuns(t, dir); n != 1 {
		t.Errorf("plugin ran %d times, want 1: the rejected token was already replaced", n)
	}
}

func TestExecCredsClientCertificate(t *testing.T) {
	ca := newTestCA(t)
	certDir := t.TempDir()
	certPEM, keyPEM := ca.issue(t, "client", nil)
	if err := os.WriteFile(filepath.Join(certDir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	plugin, _ := testPlugin(t, "client.authentication.k8s.io/v1", clientcmdapi.ExecEnvVar{Name: pluginCertEnv, Value: certDir})
	creds, err := newExecCreds(plugin, nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := creds.clientCertificate(nil)
	if err != nil {
		t.Fatalf("clientCertificate: %v", err)
	}
	if cert.Leaf == nil || cert.Leaf.Subject.CommonName != "client" {
		t.Errorf("clientCertificate leaf = %v, want CN=client", cert.Leaf)
	}
}

func TestExecCredsMissingPlugin(t *testing.T) {
	creds, err := newExecCreds(&clientcmdapi.ExecConfig{
		APIVersion:      "client.authentication.k8s.io/v1",
		Command:         "ateclient-test-missing-plugin",
		InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
		InstallHint:     "install the missing plugin",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cert, err := creds.clientCertificate(nil); err != nil || len(cert.Certificate) != 0 {
		t.Errorf("clientCertificate = (%v, %v), want an empty certificate and no error", cert, err)
	}
	if _, err := creds.GetRequestMetadata(context.Background()); err == nil || !strings.Contains(err.Error(), "install the missing plugin") {
		t.Errorf("GetRequestMetadata error = %v, want one carrying the install hint", err)
	}
}

func TestMintedTokenCreds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mints := 0
	creds := newMintedTokenCreds(func(context.Context) (string, time.Time, error) {
		mints++
		return fmt.Sprintf("token-%d", mints), now.Add(time.Hour), nil
	})
	creds.now = func() time.Time { return now }

	if got := authorization(t, creds); got != "Bearer token-1" {
		t.Errorf("authorization = %q, want Bearer token-1", got)
	}
	now = now.Add(58 * time.Minute)
	if got := authorization(t, creds); got != "Bearer token-1" {
		t.Errorf("authorization before the expiry margin = %q, want Bearer token-1", got)
	}
	now = now.Add(time.Minute)
	if got := authorization(t, creds); got != "Bearer token-2" {
		t.Errorf("authorization within the expiry margin = %q, want Bearer token-2", got)
	}

	creds.refresh("Bearer token-1")
	if got := authorization(t, creds); got != "Bearer token-2" {
		t.Errorf("authorization after refresh for an old token = %q, want Bearer token-2", got)
	}
	creds.refresh("Bearer token-2")
	if got := authorization(t, creds); got != "Bearer token-3" {
		t.Errorf("authorization after refresh = %q, want Bearer token-3", got)
	}
}

type failingCreds struct{ fakeCreds }

func (failingCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return nil, errors.New("plugin failed")
}

func TestRetryUnauthenticatedCredentialError(t *testing.T) {
	calls := 0
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		calls++
		return nil
	}
	err := retryUnauthenticated(&failingCreds{})(context.Background(), "/m", nil, nil, nil, invoker)
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "plugin failed") {
		t.Errorf("err = %v, want Unauthenticated carrying the credential error", err)
	}
	if calls != 0 {
		t.Errorf("invoker called %d times, want 0", calls)
	}
}

type fakeCreds struct {
	bearerTokenCreds
	refreshes int
	canRetry  bool
}

func (f *fakeCreds) refresh(string) bool {
	f.refreshes++
	return f.canRetry
}

func TestRetryUnauthenticated(t *testing.T) {
	for _, tc := range []struct {
		name      string
		errs      []error
		canRetry  bool
		wantCode  codes.Code
		wantCalls int
	}{
		{"success", []error{nil}, true, codes.OK, 1},
		{"retried", []error{status.Error(codes.Unauthenticated, ""), nil}, true, codes.OK, 2},
		{"retried once", []error{status.Error(codes.Unauthenticated, ""), status.Error(codes.Unauthenticated, "")}, true, codes.Unauthenticated, 2},
		{"cannot retry", []error{status.Error(codes.Unauthenticated, "")}, false, codes.Unauthenticated, 1},
		{"other error", []error{status.Error(codes.PermissionDenied, "")}, true, codes.PermissionDenied, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := &fakeCreds{bearerTokenCreds: "t", canRetry: tc.canRetry}
			calls := 0
			invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
				err := tc.errs[calls]
				calls++
				return err
			}
			err := retryUnauthenticated(creds)(context.Background(), "/m", nil, nil, nil, invoker)
			if got := status.Code(err); got != tc.wantCode {
				t.Errorf("code = %v, want %v", got, tc.wantCode)
			}
			if calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// fakeAuthentication rejects the tokens in rejected and reports the bearer
// token as the caller's ID.
type fakeAuthentication struct {
	ateapipb.UnimplementedAuthenticationServer
	mu       sync.Mutex
	rejected map[string]bool
}

func (f *fakeAuthentication) WhoAmI(ctx context.Context, _ *ateapipb.WhoAmIRequest) (*ateapipb.WhoAmIResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	token := strings.TrimPrefix(strings.Join(md.Get("authorization"), ""), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	if token == "" || f.rejected[token] {
		return nil, status.Error(codes.Unauthenticated, "rejected")
	}
	return &ateapipb.WhoAmIResponse{Id: token, Kind: ateapipb.PrincipalKind_PRINCIPAL_KIND_JWT}, nil
}

func TestDialExecCredential(t *testing.T) {
	ca := newTestCA(t)
	certPEM, keyPEM := ca.issue(t, "ateapi.test", []string{"ateapi.test"})
	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS13})))
	authn := &fakeAuthentication{rejected: map[string]bool{"token-1": true}}
	ateapipb.RegisterAuthenticationServer(srv, authn)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	plugin, dir := testPlugin(t, "client.authentication.k8s.io/v1", clientcmdapi.ExecEnvVar{Name: pluginTTLEnv, Value: "1h"})
	cli, err := Dial(context.Background(), &ateconfig.Context{
		Name:                     "test",
		Endpoint:                 ateconfig.Endpoint{Address: lis.Addr().String()},
		TLSServerName:            "ateapi.test",
		CertificateAuthorityData: ca.pem,
		Credential:               ateconfig.Credential{Exec: plugin},
	}, DialOptions{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	resp, err := cli.Authentication().WhoAmI(context.Background(), &ateapipb.WhoAmIRequest{})
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if resp.GetId() != "token-2" {
		t.Errorf("WhoAmI id = %q, want token-2 after the rejected token-1 was replaced", resp.GetId())
	}
	if n := pluginRuns(t, dir); n != 2 {
		t.Errorf("plugin ran %d times, want 2", n)
	}
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) issue(t *testing.T, cn string, dnsNames []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
