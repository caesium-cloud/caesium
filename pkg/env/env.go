package env

import (
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
)

var variables = new(Environment)

// Process the environment variables set for caesium.
func Process() error {
	if err := envconfig.Process("caesium", variables); err != nil {
		return errors.Wrap(err, "failed to process environment variables")
	}

	// set the log level
	if err := log.SetLevel(variables.LogLevel); err != nil {
		return errors.Wrap(err, "failed to set log level")
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
	Port                          int           `default:"8080"`
	DockerHost                    string        `default:"" split_words:"true"`
	KubernetesConfig              string        `default:"" split_words:"true"`
	KubernetesNamespace           string        `default:"default" split_words:"true"`
	PodmanURI                     string        `default:"" split_words:"true"`
	NodeAddress                   string        `default:"127.0.0.1:9001" split_words:"true"`
	NodeLabels                    string        `default:"" split_words:"true"`
	DatabaseNodes                 []string      `default:"" split_words:"true"`
	DatabasePath                  string        `default:"/var/lib/caesium/dqlite" split_words:"true"`
	DatabaseType                  string        `default:"internal" split_words:"true"`
	DatabaseDSN                   string        `default:"host=postgres user=postgres password=postgres dbname=caesium port=5432 sslmode=disable" split_words:"true"`
	MaxParallelTasks              int           `default:"1" split_words:"true"`
	TaskFailurePolicy             string        `default:"halt" split_words:"true"`
	TaskTimeout                   time.Duration `default:"0" split_words:"true"`
	ExecutionMode                 string        `default:"local" split_words:"true"`
	WorkerEnabled                 bool          `default:"true" split_words:"true"`
	WorkerPollInterval            time.Duration `default:"2s" split_words:"true"`
	WorkerLeaseTTL                time.Duration `default:"5m" split_words:"true"`
	WorkerPoolSize                int           `default:"4" split_words:"true"`
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
}
