package env

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/kelseyhightower/envconfig"
)

var variables = new(Environment)

// Process the environment variables set for caesium.
func Process() error {
	variables.MaxParallelTasks = runtime.NumCPU()

	if err := envconfig.Process("caesium", variables); err != nil {
		return fmt.Errorf("failed to process environment variables: %w", err)
	}
	if err := validate(); err != nil {
		return err
	}

	// set the log level
	if err := log.SetLevel(variables.LogLevel); err != nil {
		return fmt.Errorf("failed to set log level: %w", err)
	}

	return nil
}

func validate() error {
	dbType := strings.ToLower(strings.TrimSpace(variables.DatabaseType))
	if variables.DatabaseShards < 1 {
		return fmt.Errorf("CAESIUM_DATABASE_SHARDS must be greater than or equal to 1")
	}
	if variables.DatabaseShards > 1 && dbType != "" && dbType != "internal" && dbType != "dqlite" {
		return fmt.Errorf("CAESIUM_DATABASE_SHARDS greater than 1 requires CAESIUM_DATABASE_TYPE=internal")
	}
	if dbType == "" || dbType == "internal" || dbType == "dqlite" {
		if variables.DatabaseVoters < 3 || variables.DatabaseVoters%2 == 0 {
			return fmt.Errorf("CAESIUM_DATABASE_VOTERS must be an odd number greater than or equal to 3")
		}
		if variables.DatabaseStandbys < 0 {
			return fmt.Errorf("CAESIUM_DATABASE_STANDBYS must be greater than or equal to 0")
		}
	}

	switch strings.ToLower(strings.TrimSpace(variables.WakeupFanoutMode)) {
	case "", "full", "gossip":
	default:
		return fmt.Errorf("CAESIUM_WAKEUP_FANOUT_MODE must be one of: full, gossip")
	}

	return nil
}

// Variables returns the processed environment variables.
func Variables() Environment {
	return *variables
}

// Environment defines the environment variables used
// by caesium.
type Environment struct {
	LogLevel                      string        `default:"info" split_words:"true"`
	LogFormat                     string        `default:"json" split_words:"true"`
	LogConsoleEnabled             bool          `default:"true" split_words:"true"`
	Port                          int           `default:"8080"`
	DockerHost                    string        `default:"" split_words:"true"`
	KubernetesConfig              string        `default:"" split_words:"true"`
	KubernetesNamespace           string        `default:"default" split_words:"true"`
	PodmanURI                     string        `default:"" split_words:"true"`
	NodeAddress                   string        `default:"127.0.0.1:9001" split_words:"true"`
	NodeLabels                    string        `default:"" split_words:"true"`
	DatabaseNodes                 []string      `default:"" split_words:"true"`
	APIExternalURL                string        `envconfig:"API_EXTERNAL_URL" default:""`
	DatabasePath                  string        `default:"/var/lib/caesium/dqlite" split_words:"true"`
	DatabaseType                  string        `default:"internal" split_words:"true"`
	DatabaseDSN                   string        `default:"host=postgres user=postgres password=postgres dbname=caesium port=5432 sslmode=disable" split_words:"true"`
	DatabaseMaxOpenConns          int           `default:"4" split_words:"true"`
	DatabaseMaxIdleConns          int           `default:"2" split_words:"true"`
	DatabaseShards                int           `default:"1" split_words:"true"`
	DatabaseVoters                int           `default:"3" split_words:"true"`
	DatabaseStandbys              int           `default:"3" split_words:"true"`
	DatabaseConsoleEnabled        bool          `default:"false" split_words:"true"`
	ManualTriggerAPIKey           string        `envconfig:"MANUAL_TRIGGER_API_KEY" default:""`
	MaxParallelTasks              int           `split_words:"true"`
	TaskFailurePolicy             string        `default:"halt" split_words:"true"`
	TaskTimeout                   time.Duration `default:"0" split_words:"true"`
	ExecutionMode                 string        `default:"local" split_words:"true"`
	WorkerEnabled                 bool          `default:"true" split_words:"true"`
	WorkerPollInterval            time.Duration `default:"15s" split_words:"true"`
	WorkerReclaimInterval         time.Duration `default:"30s" split_words:"true"`
	WorkerLeaseTTL                time.Duration `default:"5m" split_words:"true"`
	WorkerLeaseRenewInterval      time.Duration `default:"0" split_words:"true"`
	WorkerPoolSize                int           `default:"4" split_words:"true"`
	ShutdownGracePeriod           time.Duration `envconfig:"SHUTDOWN_GRACE_PERIOD" default:"30s"`
	InternalWakeupToken           string        `default:"" split_words:"true"`
	WakeupFanoutMode              string        `default:"full" split_words:"true"`
	AtomPollInterval              time.Duration `default:"1s" split_words:"true"`
	JobdefGitEnabled              bool          `envconfig:"JOBDEF_GIT_ENABLED" default:"false"`
	JobdefGitOnce                 bool          `envconfig:"JOBDEF_GIT_ONCE" default:"false"`
	JobdefGitInterval             time.Duration `envconfig:"JOBDEF_GIT_INTERVAL" default:"1m"`
	JobdefGitSources              GitSources    `envconfig:"JOBDEF_GIT_SOURCES"`
	JobdefSecretsEnableEnv        bool          `envconfig:"JOBDEF_SECRETS_ENABLE_ENV" default:"true"`
	JobdefSecretsEnableKubernetes bool          `envconfig:"JOBDEF_SECRETS_ENABLE_KUBERNETES" default:"false"`
	JobdefSecretsKubeConfig       string        `envconfig:"JOBDEF_SECRETS_KUBECONFIG"`
	JobdefSecretsKubeNamespace    string        `envconfig:"JOBDEF_SECRETS_KUBE_NAMESPACE" default:"default"`
	JobdefSecretsVaultAddress     string        `envconfig:"JOBDEF_SECRETS_VAULT_ADDRESS"`
	JobdefSecretsVaultToken       string        `envconfig:"JOBDEF_SECRETS_VAULT_TOKEN"`
	JobdefSecretsVaultNamespace   string        `envconfig:"JOBDEF_SECRETS_VAULT_NAMESPACE"`
	JobdefSecretsVaultCACert      string        `envconfig:"JOBDEF_SECRETS_VAULT_CA_CERT"`
	JobdefSecretsVaultSkipVerify  bool          `envconfig:"JOBDEF_SECRETS_VAULT_SKIP_VERIFY" default:"false"`
	CacheEnabled                  bool          `default:"false" split_words:"true"`
	CacheTTL                      time.Duration `default:"24h" split_words:"true"`
	CachePruneInterval            time.Duration `default:"1h" split_words:"true"`
	CacheMaxEntries               int           `default:"10000" split_words:"true"`
	OpenLineageEnabled            bool          `envconfig:"OPEN_LINEAGE_ENABLED" default:"false"`
	OpenLineageTransport          string        `envconfig:"OPEN_LINEAGE_TRANSPORT" default:"http"`
	OpenLineageURL                string        `envconfig:"OPEN_LINEAGE_URL" default:""`
	OpenLineageNamespace          string        `envconfig:"OPEN_LINEAGE_NAMESPACE" default:"caesium"`
	OpenLineageHeaders            string        `envconfig:"OPEN_LINEAGE_HEADERS" default:""`
	OpenLineageFilePath           string        `envconfig:"OPEN_LINEAGE_FILE_PATH" default:"/var/lib/caesium/lineage.ndjson"`
	OpenLineageTimeout            time.Duration `envconfig:"OPEN_LINEAGE_TIMEOUT" default:"5s"`
	OpenLineageRetryAttempts      uint          `envconfig:"OPEN_LINEAGE_RETRY_ATTEMPTS" default:"3"`
	WebhookMaxBodySize            ByteSize      `envconfig:"WEBHOOK_MAX_BODY_SIZE" default:"1MB"`
	WebhookRateLimitPerMinute     int           `envconfig:"WEBHOOK_RATE_LIMIT_PER_MINUTE" default:"120"`
	WebhookRateLimitBurst         int           `envconfig:"WEBHOOK_RATE_LIMIT_BURST" default:"20"`

	// Notification Watcher
	NotificationWatcherInterval time.Duration `envconfig:"NOTIFICATION_WATCHER_INTERVAL" default:"15s"`

	// Authentication & Authorization
	AuthMode                string        `envconfig:"AUTH_MODE" default:"none"` // none, api-key
	AuthKeyHashSecret       string        `envconfig:"AUTH_KEY_HASH_SECRET" default:""`
	AuthRequireTLS          bool          `envconfig:"AUTH_REQUIRE_TLS" default:"true"`
	TLSCert                 string        `envconfig:"TLS_CERT" default:""`
	TLSKey                  string        `envconfig:"TLS_KEY" default:""`
	TrustedProxies          string        `envconfig:"TRUSTED_PROXIES" default:""`
	AuthRateLimitPerMinute  int           `envconfig:"AUTH_RATE_LIMIT_PER_MINUTE" default:"10"`
	AuthRateLimitBurstAlert int           `envconfig:"AUTH_RATE_LIMIT_BURST_ALERT" default:"100"`
	AuthPublicBaseURL       string        `envconfig:"AUTH_PUBLIC_BASE_URL" default:""`
	AuthSessionIdleTTL      time.Duration `envconfig:"AUTH_SESSION_IDLE_TTL" default:"8h"`
	AuthSessionAbsoluteTTL  time.Duration `envconfig:"AUTH_SESSION_ABSOLUTE_TTL" default:"24h"`
	AuthSessionCookieName   string        `envconfig:"AUTH_SESSION_COOKIE_NAME" default:"caesium_session"`
	AuthRoleMapping         string        `envconfig:"AUTH_ROLE_MAPPING" default:""`
	AuthDefaultRole         string        `envconfig:"AUTH_DEFAULT_ROLE" default:""`
	AuthOIDCEnabled         bool          `envconfig:"AUTH_OIDC_ENABLED" default:"false"`
	AuthOIDCIssuerURL       string        `envconfig:"AUTH_OIDC_ISSUER_URL" default:""`
	AuthOIDCClientID        string        `envconfig:"AUTH_OIDC_CLIENT_ID" default:""`
	AuthOIDCClientSecret    string        `envconfig:"AUTH_OIDC_CLIENT_SECRET" default:""`
	AuthOIDCScopes          string        `envconfig:"AUTH_OIDC_SCOPES" default:"openid profile email groups"`
	AuthOIDCGroupsClaim     string        `envconfig:"AUTH_OIDC_GROUPS_CLAIM" default:"groups"`
	AuthOIDCRedirectURL     string        `envconfig:"AUTH_OIDC_REDIRECT_URL" default:""`
	AuthSAMLEnabled         bool          `envconfig:"AUTH_SAML_ENABLED" default:"false"`
	AuthLDAPEnabled         bool          `envconfig:"AUTH_LDAP_ENABLED" default:"false"`

	// Run-owner coordination (Phase 2).
	// CAESIUM_RUN_OWNER_ENABLED enables the run-owner coordination mode.
	// Default false — the system behaves byte-identically to Phase 1 when off.
	RunOwnerEnabled bool `envconfig:"RUN_OWNER_ENABLED" default:"false"`
	// CAESIUM_RUN_LEASE_TTL is how long an owner holds a run lease before
	// another node may take over.  Default 30s; must be > 0 when owner mode
	// is enabled.
	RunLeaseTTL time.Duration `envconfig:"RUN_LEASE_TTL" default:"30s"`

	// CAESIUM_RUN_OWNER_DISPATCH_INTERVAL is the polling cadence for the
	// per-node owner dispatch loop.  Smaller values reduce task-start latency
	// at the cost of more DB reads per second.  Default 1s.
	RunOwnerDispatchInterval time.Duration `envconfig:"RUN_OWNER_DISPATCH_INTERVAL" default:"1s"`
	// CAESIUM_RUN_OWNER_DISPATCH_BATCH caps the number of tasks dispatched per
	// tick per owned run.  Prevents a large fan-out from stalling the loop.
	// Default 64.
	RunOwnerDispatchBatch int `envconfig:"RUN_OWNER_DISPATCH_BATCH" default:"64"`
	// CAESIUM_RUN_OWNER_DISPATCH_DEADLINE is the task execution deadline added
	// to time.Now() in each DispatchRequest.  Workers use this to bound how long
	// they hold the claim before returning it.  Default 5m.
	RunOwnerDispatchDeadline time.Duration `envconfig:"RUN_OWNER_DISPATCH_DEADLINE" default:"5m"`

	// CAESIUM_INTERNAL_PORT is the port for the dedicated internal mTLS listener
	// that hosts the run-owner endpoints (/internal/dispatch, /internal/complete).
	// It is separate from the public API port so the public server can remain
	// plain HTTP while node-to-node coordination traffic is mutually
	// authenticated.  Only bound when owner mode is enabled.  Default 8443.
	InternalPort int `envconfig:"INTERNAL_PORT" default:"8443"`
	// CAESIUM_INTERNAL_MTLS_CA/CERT/KEY are PEM file paths configuring mutual TLS
	// on the internal listener.  CA validates peer (client) certificates;
	// CERT/KEY are this node's own identity — presented both as the listener's
	// server certificate and as the client certificate when this node POSTs to a
	// peer's internal endpoints.  All three are required when owner mode is
	// enabled (enforced at startup).
	InternalMTLSCA   string `envconfig:"INTERNAL_MTLS_CA" default:""`
	InternalMTLSCert string `envconfig:"INTERNAL_MTLS_CERT" default:""`
	InternalMTLSKey  string `envconfig:"INTERNAL_MTLS_KEY" default:""`
	// CAESIUM_INTERNAL_MTLS_TOKEN optionally separates the auto-provisioning
	// PKI token from CAESIUM_INTERNAL_WAKEUP_TOKEN. When empty, the wakeup token
	// is used for HKDF key derivation.
	InternalMTLSToken string `envconfig:"INTERNAL_MTLS_TOKEN" default:""`
	// Auto-provisioned internal mTLS rotation settings. These apply only when
	// owner mode is on and explicit mTLS files are not provided.
	InternalMTLSCATTL             time.Duration `envconfig:"INTERNAL_MTLS_CA_TTL" default:"43800h"`
	InternalMTLSLeafTTL           time.Duration `envconfig:"INTERNAL_MTLS_LEAF_TTL" default:"720h"`
	InternalMTLSLeafRenewBefore   time.Duration `envconfig:"INTERNAL_MTLS_LEAF_RENEW_BEFORE" default:"240h"`
	InternalMTLSCARenewBefore     time.Duration `envconfig:"INTERNAL_MTLS_CA_RENEW_BEFORE" default:"720h"`
	InternalMTLSEnrollmentTimeout time.Duration `envconfig:"INTERNAL_MTLS_ENROLLMENT_TIMEOUT" default:"2m"`

	// Run-owner checkpointing (Phase 2 B3).  The owner persists an in-memory
	// state checkpoint whichever comes first of RUN_CHECKPOINT_EVENTS terminal
	// transitions or RUN_CHECKPOINT_INTERVAL elapsed.  RUN_CHECKPOINT_FULL_EVERY
	// is the number of checkpoints between full snapshots (intervening ones are
	// deltas); v1 writes full snapshots only, so it is currently advisory.
	RunCheckpointEvents    int           `envconfig:"RUN_CHECKPOINT_EVENTS" default:"100"`
	RunCheckpointInterval  time.Duration `envconfig:"RUN_CHECKPOINT_INTERVAL" default:"2s"`
	RunCheckpointFullEvery int           `envconfig:"RUN_CHECKPOINT_FULL_EVERY" default:"10"`

	// CAESIUM_RUN_OWNER_IN_MEMORY gates the B3 in-memory advancement path: the
	// owner advances the DAG in memory (run.RunState), writes only terminal
	// task_runs rows (no per-transition predecessor UPDATEs), and checkpoints for
	// fast failover.  Default false — when off, completions take the proven
	// SQL-advancement path (B2), byte-identical.  Requires RUN_OWNER_ENABLED.
	RunOwnerInMemory bool `envconfig:"RUN_OWNER_IN_MEMORY" default:"false"`
}

// SSOEnabled reports whether any SSO provider is configured.
func (e Environment) SSOEnabled() bool {
	return e.AuthOIDCEnabled || e.AuthSAMLEnabled || e.AuthLDAPEnabled
}
