# internal/config

Loads and validates fj-bellows YAML configuration.

The key design point is **deferred decode**: the `provider_config` subtree is
kept as a raw `yaml.Node` (`Config.ProviderConfig`) so the core never needs to
know provider-specific fields. The selected provider decodes that node into its
own struct.

Secrets (the Forgejo admin token and the provider API token) live **inline** in
config.yaml — keep the file readable only by the daemon. The SSH private key is
referenced by **path** (`ssh.private_key_file`) rather than inlined, to keep the
file tidy.

`Duration` is a thin wrapper over `time.Duration` that unmarshals from strings
like `10s` / `5m`. `Load` applies defaults (tag, max scale, reusable worker
lifecycle, poll interval, idle timeout, hour margin, SSH user/port) and
validates required fields. Set `worker_lifecycle: disposable` to destroy a
worker after its first dispatch attempt and refuse warm adoption after restart.
Set `scale.prewarm` between zero and `scale.max` to keep that many disposable
one-job runners registered and online; zero preserves queue-driven provisioning.

`Redact(*Config)` returns a copy with secret-bearing fields replaced by
`<redacted>`, safe to ship over the operator-facing control plane
(`GetConfig` RPC). It zeros `Forgejo.Token` and walks the opaque
`provider_config` `yaml.Node` tree, replacing any mapping value whose key
matches `token`, `password`, `secret`, `key`, `api_key`, `access_key`, or
`secret_key` (case-insensitive, exact match — no substring). The
`ssh.private_key_file` path is left intact: the file at that path is the
secret, the path itself is operator config.
