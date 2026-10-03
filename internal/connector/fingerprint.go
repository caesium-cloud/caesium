package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

// Fingerprint is the canonical SHA-256 of enabled configuration, connection
// identity, credential references, bindings, schemas, and limits. Resolved
// secret bytes are not an argument: rotation under an unchanged reference
// does not change the result, including when the secret text also appears in
// a public field such as the provider name.
func Fingerprint(cfg *Config) (string, error) {
	if cfg == nil {
		return "", errors.New("connector config is required")
	}
	body, err := canonicalBytes(cfg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

type canonicalDocument struct {
	Version     int                   `json:"version"`
	Connections []canonicalConnection `json:"connections"`
}

type canonicalConnection struct {
	ID               string             `json:"id"`
	Enabled          bool               `json:"enabled"`
	Provider         string             `json:"provider"`
	Endpoint         string             `json:"endpoint"`
	Scope            string             `json:"scope"`
	SecretRefs       []string           `json:"secretRefs"`
	CertificatePaths []string           `json:"certificatePaths"`
	Limits           canonicalLimits    `json:"limits"`
	Bindings         []canonicalBinding `json:"bindings"`
}

type canonicalLimits struct {
	ConsoleRefreshNanos             int64 `json:"consoleRefreshNanos"`
	ReadsPerMinute                  int   `json:"readsPerMinute"`
	QueriesPerMinute                int   `json:"queriesPerMinute"`
	MaxInFlightRPCs                 int   `json:"maxInFlightRPCs"`
	RPCDeadlineNanos                int64 `json:"rpcDeadlineNanos"`
	MaxPageEntries                  int   `json:"maxPageEntries"`
	MaxPageMetadataBytes            int   `json:"maxPageMetadataBytes"`
	UnreferencedSnapshotMaxAgeNanos int64 `json:"unreferencedSnapshotMaxAgeNanos"`
	MaxUnreferencedSnapshots        int   `json:"maxUnreferencedSnapshots"`
}

type canonicalBinding struct {
	Name              string              `json:"name"`
	Version           string              `json:"version"`
	DisplayName       string              `json:"displayName"`
	StatusQuery       string              `json:"statusQuery"`
	ActivityAllowlist []canonicalActivity `json:"activityAllowlist"`
	Actions           []canonicalAction   `json:"actions"`
}

type canonicalActivity struct {
	ActivityType string   `json:"activityType"`
	Jobs         []string `json:"jobs"`
}

type canonicalAction struct {
	Name         string          `json:"name"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	ResultSchema json.RawMessage `json:"resultSchema"`
}

func canonicalBytes(cfg *Config) ([]byte, error) {
	connections := append([]Connection(nil), cfg.Connections...)
	sort.Slice(connections, func(i, j int) bool { return connections[i].ID < connections[j].ID })
	document := canonicalDocument{
		Version:     cfg.Version,
		Connections: make([]canonicalConnection, 0, len(connections)),
	}
	for _, conn := range connections {
		canonicalConn, err := canonicalConnectionFrom(conn)
		if err != nil {
			return nil, err
		}
		document.Connections = append(document.Connections, canonicalConn)
	}
	return json.Marshal(document)
}

func canonicalConnectionFrom(conn Connection) (canonicalConnection, error) {
	bindings := append([]Binding(nil), conn.Bindings...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	canonicalBindings := make([]canonicalBinding, 0, len(bindings))
	for _, binding := range bindings {
		canonicalBinding, err := canonicalBindingFrom(binding)
		if err != nil {
			return canonicalConnection{}, err
		}
		canonicalBindings = append(canonicalBindings, canonicalBinding)
	}
	secretRefs := sortedCopy(conn.SecretRefs)
	if secretRefs == nil {
		secretRefs = []string{}
	}
	certPaths := sortedCopy(conn.CertificatePaths)
	if certPaths == nil {
		certPaths = []string{}
	}
	return canonicalConnection{
		ID:               conn.ID,
		Enabled:          conn.Enabled,
		Provider:         conn.Provider,
		Endpoint:         conn.Endpoint,
		Scope:            conn.Scope,
		SecretRefs:       secretRefs,
		CertificatePaths: certPaths,
		Limits: canonicalLimits{
			ConsoleRefreshNanos:             int64(conn.Limits.ConsoleRefresh),
			ReadsPerMinute:                  conn.Limits.ReadsPerMinute,
			QueriesPerMinute:                conn.Limits.QueriesPerMinute,
			MaxInFlightRPCs:                 conn.Limits.MaxInFlightRPCs,
			RPCDeadlineNanos:                int64(conn.Limits.RPCDeadline),
			MaxPageEntries:                  conn.Limits.MaxPageEntries,
			MaxPageMetadataBytes:            conn.Limits.MaxPageMetadataBytes,
			UnreferencedSnapshotMaxAgeNanos: int64(conn.Limits.UnreferencedSnapshotMaxAge),
			MaxUnreferencedSnapshots:        conn.Limits.MaxUnreferencedSnapshots,
		},
		Bindings: canonicalBindings,
	}, nil
}

func canonicalBindingFrom(binding Binding) (canonicalBinding, error) {
	activities := append([]ActivityJob(nil), binding.ActivityAllowlist...)
	sort.Slice(activities, func(i, j int) bool { return activities[i].ActivityType < activities[j].ActivityType })
	canonicalActivities := make([]canonicalActivity, 0, len(activities))
	for _, activity := range activities {
		jobs := sortedCopy(activity.Jobs)
		if jobs == nil {
			jobs = []string{}
		}
		canonicalActivities = append(canonicalActivities, canonicalActivity{
			ActivityType: activity.ActivityType,
			Jobs:         jobs,
		})
	}
	actions := append([]Action(nil), binding.Actions...)
	sort.Slice(actions, func(i, j int) bool { return actions[i].Name < actions[j].Name })
	canonicalActions := make([]canonicalAction, 0, len(actions))
	for _, action := range actions {
		// compileSchema already stored canonical JSON. Re-parsing it as
		// float64 would collapse integers above 2^53 onto the same digest.
		if len(action.InputSchema) == 0 || len(action.ResultSchema) == 0 {
			return canonicalBinding{}, errors.New("connector schema is required")
		}
		canonicalActions = append(canonicalActions, canonicalAction(action))
	}
	if canonicalActivities == nil {
		canonicalActivities = []canonicalActivity{}
	}
	return canonicalBinding{
		Name:              binding.Name,
		Version:           binding.Version,
		DisplayName:       binding.DisplayName,
		StatusQuery:       binding.StatusQuery,
		ActivityAllowlist: canonicalActivities,
		Actions:           canonicalActions,
	}, nil
}
