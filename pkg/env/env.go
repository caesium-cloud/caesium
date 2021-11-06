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
	LogLevel               string        `default:"info"`
	Port                   int           `default:"8080"`
	KubernetesConfig       string        `default:""`
	KubernetesNamespace    string        `default:"default"`
	NodeID                 string        `default:""` // hostname
	DBPath                 string        `default:""`
	HttpAddr               string        `default:"localhost:4001"`
	HttpAdvAddr            string        `default:""`
	JoinSrcIP              string        `default:""`
	RaftAddr               string        `default:"localhost:4002"`
	RaftAdvAddr            string        `default:""`
	JoinAddr               string        `default:""`
	JoinAttempts           int           `default:"5"`
	JoinInterval           time.Duration `default:"5s"`
	DSN                    string        `default:""`
	OnDisk                 bool          `default:"true"`
	RaftNonVoter           bool          `default:"false"`
	RaftHeartbeatTimeout   time.Duration `default:"1s"`
	RaftElectionTimeout    time.Duration `default:"1s"`
	RaftApplyTimeout       time.Duration `default:"10s"`
	RaftOpenTimeout        time.Duration `default:"120s"`
	RaftLeaderWait         bool          `default:"true"`
	RaftSnapThreshold      uint64        `default:"8192"`
	RaftSnapInterval       time.Duration `default:"30s"`
	RaftLeaderLeaseTimeout time.Duration `default:"0s"`
	RaftShutdownOrRemove   bool          `default:"false"`
	RaftLogLevel           string        `default:"INFO"`
	CompressionSize        int           `default:"150"`
	CompressionBatch       int           `default:"5"`
	DatabaseType           string        `default:"postgres"`
	DatabaseDSN            string        `default:"host=postgres user=postgres password=postgres dbname=caesium port=5432 sslmode=disable"`
}
