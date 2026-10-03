package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

// Fingerprint is the canonical SHA-256 of enabled configuration, connection
// identity, credential references, bindings, schemas, and limits. resolved
// carries secret bytes from the env resolver and is not read: the digest is
// built only from cfg. A resolved value that also occurs in a public field,
// such as the provider name "temporal", still succeeds. Rotation under an
// unchanged reference does not change the result.
func Fingerprint(cfg *Config, resolved map[string]string) (string, error) {
	if cfg == nil {
		return "", errors.New("connector config is required")
	}
	// Keep the parameter in the signature so callers pass resolver output
	// through this function. Do not search cfg for those bytes: public fields
	// can legitimately contain the same text.
	_ = resolved
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
		input, err := canonicalRaw(action.InputSchema)
		if err != nil {
			return canonicalBinding{}, err
		}
		result, err := canonicalRaw(action.ResultSchema)
		if err != nil {
			return canonicalBinding{}, err
		}
		canonicalActions = append(canonicalActions, canonicalAction{
			Name:         action.Name,
			InputSchema:  input,
			ResultSchema: result,
		})
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

func canonicalRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("connector schema is required")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("connector schema is not canonical JSON")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("connector schema is not canonical JSON")
	}
	return encoded, nil
}
