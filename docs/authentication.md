# ate-api Authentication

`ate-api` accepts mTLS client certificates and bearer JWTs. JWT providers are
configured with the file passed to `--authentication-config`:

```yaml
actorIdentityJWTProvider: kubernetes
jwtProviders:
- name: kubernetes
  issuer: https://kubernetes.default.svc.cluster.local
  audiences:
  - api.ate-system.svc
  certificateAuthorityFile: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
  discoveryTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
- name: google
  issuer: https://accounts.google.com
  audiences:
  - 32555940559.apps.googleusercontent.com
```

Provider names and issuers must be unique. `issuer` must be an HTTPS URL and
`audiences` must be non-empty; a token is accepted when any configured audience
matches. `certificateAuthorityFile` and `discoveryTokenFile` are optional and
are needed for OIDC discovery against some private Kubernetes API servers.

`actorIdentityJWTProvider` identifies the provider allowed to call
`ActorIdentity.MintJWT`. What an authenticated caller may do is decided by
authorization; see [authorization.md](authorization.md). Its principal ID does
not record which provider issued the token, so only configure providers you
trust not to issue a token whose subject is another principal's ID.

## Client credentials

`kubectl-ate` and other clients built on `internal/ateclient` get their
credential from the current AteConfig context; see the
[kubectl-ate README](../cmd/kubectl-ate/README.md#connection--authentication).
A context's `credential.exec` is a kubeconfig exec stanza. The client runs the
plugin, caches the token until its `expirationTimestamp`, and runs the plugin
again when `ate-api` returns `UNAUTHENTICATED`.

`ate-api` checks only a JWT's issuer, audience, signature, and expiry. A
credential source is therefore safe only when its issuer limits who can get a
token for the configured audience. Kubernetes ServiceAccount tokens qualify,
because RBAC decides who may create them. Google ID tokens for the Cloud SDK's
shared client ID do not, since any Google account can get one. Neither do
Google service account ID tokens or GitHub Actions OIDC tokens: any service
account or workflow can request one for any audience.

`kubectl ate credential` is the built-in plugin. It mints a ServiceAccount
token through your kubeconfig and caches it under `~/.ate/cache`.
`kubectl ate credential wrap -- COMMAND` adapts a command that prints a JWT.
`kubectl ate whoami` shows the principal `ate-api` authenticated you as.

### Per-user ServiceAccounts

Without a credential, every user acts as the shared `ate-client`
ServiceAccount. To give a user their own revocable identity, let them create
tokens for a ServiceAccount of their own:

```sh
kubectl create namespace ate-users
kubectl -n ate-users create serviceaccount alice
kubectl -n ate-users create role alice-token --verb=create --resource=serviceaccounts/token --resource-name=alice
kubectl -n ate-users create rolebinding alice-token --role=alice-token --user=alice@example.com
```

Alice then runs `kubectl ate config init --service-account=ate-users/alice`,
and `kubectl ate whoami` reports `system:serviceaccount:ate-users:alice`.
Access policies grant to that ID. The token carries the ServiceAccount's
groups, not Alice's.

## Google Cloud CLI tokens

The Google Cloud CLI currently issues user identity tokens with issuer
`https://accounts.google.com` and audience
`32555940559.apps.googleusercontent.com`, the Cloud SDK's shared client ID.
These values are examples rather than built-in defaults; verify the claims
issued by your identity provider and configure them explicitly. Any Google
account can obtain such a token, so this provider admits every Google account.

With the provider configured, wrap the token in an AteConfig context:

```yaml
credential:
  exec:
    apiVersion: client.authentication.k8s.io/v1
    command: kubectl-ate
    args: [credential, wrap, --, gcloud, auth, print-identity-token]
    interactiveMode: Never
```

or pass it for one command with `--token-file`, which accepts a file path or
`-` for stdin:

```sh
gcloud auth print-identity-token | kubectl ate --token-file=- get actors
```

Either way, `kubectl-ate` still uses kubeconfig access to establish its
port-forward and obtain the server trust bundle.

For a manifest-based installation, replace the authentication ConfigMap and
restart the deployment:

```sh
kubectl -n ate-system create configmap ate-api-authentication \
  --from-file=authentication.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n ate-system rollout restart deployment/ate-api-server
```

Configuration is read at process startup. Restart `ate-api` pods after changing
the ConfigMap. OIDC signing keys are cached and refreshed when an unknown key ID
is encountered.
