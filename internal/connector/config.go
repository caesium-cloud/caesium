package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// idPattern keeps connection, binding, action, and adapter names stable tokens.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// canonicalDecimal is a JSON integer: no hex, octal, leading zeros, or fraction.
var canonicalDecimal = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// jsonNumber is a JSON number literal. YAML hex, timestamps, and ".inf" are not.
var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

var yamlQuoted = regexp.MustCompile("`[^`]*`")

// minRedactedSecretLen skips substring replacement for values so short that
// they occur inside ordinary error text. "e" must not rewrite "readsPerMinute".
const minRedactedSecretLen = 8

// Config is a validated, versioned connector file. Secret material is stored
// only as references and mounted certificate paths.
type Config struct {
	Version     int
	Connections []Connection
}

// Connection is one configured provider endpoint and its bindings.
type Connection struct {
	ID               string
	Enabled          bool
	Provider         string
	Endpoint         string
	Scope            string
	SecretRefs       []string
	CertificatePaths []string
	Limits           Limits
	Bindings         []Binding
}

// Identity returns the immutable connection identity.
func (c Connection) Identity() ConnectionIdentity {
	return ConnectionIdentity{
		ID:       c.ID,
		Provider: c.Provider,
		Endpoint: c.Endpoint,
		Scope:    c.Scope,
	}
}

// Limits is the effective observation budget. Omitted file fields become the
// hard ceilings.
type Limits struct {
	ConsoleRefresh             time.Duration
	ReadsPerMinute             int
	QueriesPerMinute           int
	MaxInFlightRPCs            int
	RPCDeadline                time.Duration
	MaxPageEntries             int
	MaxPageMetadataBytes       int
	UnreferencedSnapshotMaxAge time.Duration
	MaxUnreferencedSnapshots   int
}

// DefaultLimits returns the v1 ceilings.
func DefaultLimits() Limits {
	return Limits{
		ConsoleRefresh:             MinConsoleRefresh,
		ReadsPerMinute:             MaxReadsPerMinute,
		QueriesPerMinute:           MaxQueriesPerMinute,
		MaxInFlightRPCs:            MaxInFlightRPCs,
		RPCDeadline:                MaxRPCDeadline,
		MaxPageEntries:             MaxPageEntries,
		MaxPageMetadataBytes:       MaxPageMetadataBytes,
		UnreferencedSnapshotMaxAge: MaxUnreferencedSnapshotAge,
		MaxUnreferencedSnapshots:   MaxUnreferencedSnapshots,
	}
}

// Binding is a versioned workflow binding: one status query, an activity-type
// to local-job allowlist, and named actions. It does not declare the actor
// envelope or the correlation field list.
type Binding struct {
	Name              string
	Version           string
	DisplayName       string
	StatusQuery       string
	ActivityAllowlist []ActivityJob
	Actions           []Action
}

// ActivityJob maps one external activity type to local job aliases.
type ActivityJob struct {
	ActivityType string
	Jobs         []string
}

// Action is a binding-declared operation with canonical JSON schemas.
type Action struct {
	Name         string
	InputSchema  json.RawMessage
	ResultSchema json.RawMessage
}

type fileDoc struct {
	Version     int              `yaml:"version"`
	Connections []fileConnection `yaml:"connections"`
}

type fileConnection struct {
	ID          string           `yaml:"id"`
	Enabled     *bool            `yaml:"enabled"`
	Provider    string           `yaml:"provider"`
	Endpoint    string           `yaml:"endpoint"`
	Scope       string           `yaml:"scope"`
	Credentials *fileCredentials `yaml:"credentials"`
	Limits      *fileLimits      `yaml:"limits"`
	Bindings    []fileBinding    `yaml:"bindings"`
}

type fileCredentials struct {
	SecretRefs       []string `yaml:"secretRefs"`
	CertificatePaths []string `yaml:"certificatePaths"`
}

type fileLimits struct {
	ConsoleRefresh             *string `yaml:"consoleRefresh"`
	ReadsPerMinute             *int    `yaml:"readsPerMinute"`
	QueriesPerMinute           *int    `yaml:"queriesPerMinute"`
	MaxInFlightRPCs            *int    `yaml:"maxInFlightRPCs"`
	RPCDeadline                *string `yaml:"rpcDeadline"`
	MaxPageEntries             *int    `yaml:"maxPageEntries"`
	MaxPageMetadataBytes       *int    `yaml:"maxPageMetadataBytes"`
	UnreferencedSnapshotMaxAge *string `yaml:"unreferencedSnapshotMaxAge"`
	MaxUnreferencedSnapshots   *int    `yaml:"maxUnreferencedSnapshots"`
}

type fileBinding struct {
	Name              string         `yaml:"name"`
	Version           string         `yaml:"version"`
	DisplayName       string         `yaml:"displayName"`
	StatusQuery       string         `yaml:"statusQuery"`
	ActivityAllowlist []fileActivity `yaml:"activityAllowlist"`
	Actions           []fileAction   `yaml:"actions"`
}

type fileActivity struct {
	ActivityType string   `yaml:"activityType"`
	Jobs         []string `yaml:"jobs"`
}

type fileAction struct {
	Name         string         `yaml:"name"`
	InputSchema  map[string]any `yaml:"inputSchema"`
	ResultSchema map[string]any `yaml:"resultSchema"`
}

// Parse validates connector YAML. resolver, when set, resolves secret://env
// references on enabled connections through the existing env resolver. That
// check stays inside Parse: resolved bytes are not returned and are not part
// of the fingerprint. Non-env providers are shape-checked and are not dialed.
// A connection with enabled false is kept in the config and is not resolved,
// so a parked connection does not fail startup when its env var is unset.
func Parse(data []byte, resolver secret.Resolver) (*Config, error) {
	root, err := decodeDocument(data)
	if err != nil {
		return nil, err
	}
	redact := newRedactor(root)
	if err := walkDocument(root, ""); err != nil {
		return nil, redact.wrap(err)
	}
	raw, err := decodeKnown(data)
	if err != nil {
		return nil, redact.wrap(err)
	}
	// Resolve before the remaining checks so a later rejection cannot echo
	// secret bytes that the env resolver already produced.
	resolved, err := resolveRawEnvSecrets(raw, resolver)
	redact.addSecrets(resolved)
	if err != nil {
		return nil, redact.wrap(err)
	}
	cfg, err := compile(raw)
	if err != nil {
		return nil, redact.wrap(err)
	}
	return cfg, nil
}

func decodeDocument(data []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		return nil, sanitizeYAMLError(err)
	}
	var extra yaml.Node
	err := dec.Decode(&extra)
	if err == nil {
		return nil, errors.New("connector config must be a single YAML document")
	}
	if !errors.Is(err, io.EOF) {
		return nil, sanitizeYAMLError(err)
	}
	content := documentContent(&root)
	if content == nil || content.Kind != yaml.MappingNode {
		return nil, errors.New("connector config must be a YAML mapping")
	}
	return content, nil
}

func decodeKnown(data []byte) (fileDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw fileDoc
	if err := dec.Decode(&raw); err != nil {
		return fileDoc{}, sanitizeYAMLError(err)
	}
	return raw, nil
}

func compile(raw fileDoc) (*Config, error) {
	if raw.Version != 1 {
		return nil, errors.New("connector config version must be 1")
	}
	cfg := &Config{Version: raw.Version, Connections: []Connection{}}
	seenIDs := make(map[string]struct{}, len(raw.Connections))
	for i, fileConn := range raw.Connections {
		conn, err := compileConnection(i, fileConn)
		if err != nil {
			return nil, err
		}
		if _, exists := seenIDs[conn.ID]; exists {
			return nil, fmt.Errorf("connections[%d]: duplicate connection id %q", i, conn.ID)
		}
		seenIDs[conn.ID] = struct{}{}
		cfg.Connections = append(cfg.Connections, conn)
	}
	return cfg, nil
}

func compileConnection(index int, raw fileConnection) (Connection, error) {
	where := fmt.Sprintf("connections[%d]", index)
	if !idPattern.MatchString(raw.ID) {
		return Connection{}, fmt.Errorf("%s: id must be a stable token", where)
	}
	if raw.Provider != ProviderTemporal {
		return Connection{}, fmt.Errorf("%s: unsupported provider %q", where, raw.Provider)
	}
	if err := validateEndpoint(raw.Endpoint); err != nil {
		return Connection{}, fmt.Errorf("%s: %w", where, err)
	}
	if strings.TrimSpace(raw.Scope) == "" || strings.ContainsAny(raw.Scope, " \t\r\n") {
		return Connection{}, fmt.Errorf("%s: scope is required", where)
	}
	if raw.Credentials == nil {
		return Connection{}, fmt.Errorf("%s: credentials are required", where)
	}
	refs, paths, err := compileCredentials(where, raw.Credentials)
	if err != nil {
		return Connection{}, err
	}
	limits, err := compileLimits(where, raw.Limits)
	if err != nil {
		return Connection{}, err
	}
	bindings, err := compileBindings(where, raw.Bindings)
	if err != nil {
		return Connection{}, err
	}
	enabled := true
	if raw.Enabled != nil {
		enabled = *raw.Enabled
	}
	return Connection{
		ID:               raw.ID,
		Enabled:          enabled,
		Provider:         raw.Provider,
		Endpoint:         raw.Endpoint,
		Scope:            raw.Scope,
		SecretRefs:       refs,
		CertificatePaths: paths,
		Limits:           limits,
		Bindings:         bindings,
	}, nil
}

func compileCredentials(where string, raw *fileCredentials) ([]string, []string, error) {
	if len(raw.SecretRefs) == 0 && len(raw.CertificatePaths) == 0 {
		return nil, nil, fmt.Errorf("%s.credentials: a secret:// reference or mounted certificate path is required", where)
	}
	refs := make([]string, 0, len(raw.SecretRefs))
	seenRefs := make(map[string]struct{}, len(raw.SecretRefs))
	for i, ref := range raw.SecretRefs {
		if err := validateSecretRef(ref); err != nil {
			return nil, nil, fmt.Errorf("%s.credentials.secretRefs[%d]: %w", where, i, err)
		}
		if _, exists := seenRefs[ref]; exists {
			return nil, nil, fmt.Errorf("%s.credentials.secretRefs[%d]: duplicate secret reference", where, i)
		}
		seenRefs[ref] = struct{}{}
		refs = append(refs, ref)
	}
	paths := make([]string, 0, len(raw.CertificatePaths))
	seenPaths := make(map[string]struct{}, len(raw.CertificatePaths))
	for i, certPath := range raw.CertificatePaths {
		if err := validateCertificatePath(certPath); err != nil {
			return nil, nil, fmt.Errorf("%s.credentials.certificatePaths[%d]: %w", where, i, err)
		}
		if _, exists := seenPaths[certPath]; exists {
			return nil, nil, fmt.Errorf("%s.credentials.certificatePaths[%d]: duplicate certificate path", where, i)
		}
		seenPaths[certPath] = struct{}{}
		paths = append(paths, certPath)
	}
	return refs, paths, nil
}

func validateEndpoint(endpoint string) error {
	if strings.TrimSpace(endpoint) == "" || strings.ContainsAny(endpoint, " \t\r\n") {
		return errors.New("endpoint is required")
	}
	// Userinfo (https://user:password@host) is an inline credential. The
	// error stays generic so the password is not echoed.
	if strings.Contains(endpoint, "@") {
		return errors.New("endpoint must not include userinfo")
	}
	return nil
}

func validateSecretRef(ref string) error {
	if strings.TrimSpace(ref) == "" || strings.ContainsAny(ref, " \t\r\n") {
		return errors.New("credential must be a secret:// reference")
	}
	parsed, err := secret.Parse(ref)
	if err != nil || parsed.URL == nil || parsed.URL.User != nil {
		return errors.New("credential must be a secret:// reference")
	}
	// Neither the env, Kubernetes, nor Vault resolver reads a fragment.
	// secret://vault/kv/data/temporal#token is path kv/data and field temporal.
	if parsed.URL.Fragment != "" {
		return errors.New("secret reference cannot include a fragment")
	}
	switch parsed.Provider {
	case "env":
		if err := validateQueryKeys(parsed, "name"); err != nil {
			return err
		}
		if _, err := secret.EnvVarName(parsed); err != nil {
			return errors.New("env secret reference requires a name")
		}
	case "k8s", "kubernetes":
		if err := validateQueryKeys(parsed, "namespace", "name", "key"); err != nil {
			return err
		}
		// Same segment rules as KubernetesResolver.parseReference. A lone
		// ?key= does not make a one-segment path valid.
		if err := secret.ValidateKubernetesReference(parsed); err != nil {
			return errors.New("kubernetes secret reference must be secret://k8s/<secret>/<key> or secret://k8s/<namespace>/<secret>/<key>")
		}
	case "vault":
		if err := validateQueryKeys(parsed, "field"); err != nil {
			return err
		}
		if err := secret.ValidateVaultReference(parsed); err != nil {
			return errors.New("vault secret reference must include a path and a field")
		}
	default:
		return fmt.Errorf("unsupported secret provider %q", parsed.Provider)
	}
	return nil
}

func validateQueryKeys(ref *secret.Reference, allowed ...string) error {
	if ref == nil || len(ref.Query) == 0 {
		return nil
	}
	permit := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		permit[key] = struct{}{}
	}
	for key := range ref.Query {
		if _, ok := permit[key]; !ok {
			return errors.New("secret reference query contains an unsupported parameter")
		}
	}
	return nil
}

func validateCertificatePath(certPath string) error {
	if certPath == "" || strings.ContainsAny(certPath, " \t\r\n") || strings.Contains(certPath, "\\") {
		return errors.New("certificate material must be a mounted absolute path")
	}
	if strings.Contains(certPath, "://") || strings.Contains(certPath, "-----") {
		return errors.New("certificate material must be a mounted path, not inline material")
	}
	if !strings.HasPrefix(certPath, "/") || path.Clean(certPath) != certPath {
		return errors.New("certificate material must be a mounted absolute path")
	}
	return nil
}

func compileLimits(where string, raw *fileLimits) (Limits, error) {
	limits := DefaultLimits()
	if raw == nil {
		return limits, nil
	}
	var err error
	if raw.ConsoleRefresh != nil {
		limits.ConsoleRefresh, err = parseBoundedDuration(*raw.ConsoleRefresh, MinConsoleRefresh, 0, "consoleRefresh")
		if err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
	}
	if raw.ReadsPerMinute != nil {
		if err := parseMaxCount(*raw.ReadsPerMinute, MaxReadsPerMinute, "readsPerMinute"); err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
		limits.ReadsPerMinute = *raw.ReadsPerMinute
	}
	if raw.QueriesPerMinute != nil {
		if err := parseMaxCount(*raw.QueriesPerMinute, MaxQueriesPerMinute, "queriesPerMinute"); err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
		limits.QueriesPerMinute = *raw.QueriesPerMinute
	}
	if raw.MaxInFlightRPCs != nil {
		if err := parseMaxCount(*raw.MaxInFlightRPCs, MaxInFlightRPCs, "maxInFlightRPCs"); err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
		limits.MaxInFlightRPCs = *raw.MaxInFlightRPCs
	}
	if raw.RPCDeadline != nil {
		limits.RPCDeadline, err = parseBoundedDuration(*raw.RPCDeadline, time.Nanosecond, MaxRPCDeadline, "rpcDeadline")
		if err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
	}
	if raw.MaxPageEntries != nil {
		if err := parseMaxCount(*raw.MaxPageEntries, MaxPageEntries, "maxPageEntries"); err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
		limits.MaxPageEntries = *raw.MaxPageEntries
	}
	if raw.MaxPageMetadataBytes != nil {
		if err := parseMaxCount(*raw.MaxPageMetadataBytes, MaxPageMetadataBytes, "maxPageMetadataBytes"); err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
		limits.MaxPageMetadataBytes = *raw.MaxPageMetadataBytes
	}
	if raw.UnreferencedSnapshotMaxAge != nil {
		limits.UnreferencedSnapshotMaxAge, err = parseBoundedDuration(*raw.UnreferencedSnapshotMaxAge, time.Nanosecond, MaxUnreferencedSnapshotAge, "unreferencedSnapshotMaxAge")
		if err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
	}
	if raw.MaxUnreferencedSnapshots != nil {
		if err := parseMaxCount(*raw.MaxUnreferencedSnapshots, MaxUnreferencedSnapshots, "maxUnreferencedSnapshots"); err != nil {
			return Limits{}, fmt.Errorf("%s.limits: %w", where, err)
		}
		limits.MaxUnreferencedSnapshots = *raw.MaxUnreferencedSnapshots
	}
	return limits, nil
}

func parseBoundedDuration(raw string, minimum, maximum time.Duration, field string) (time.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", field)
	}
	if minimum > 0 && parsed < minimum {
		return 0, fmt.Errorf("%s must be at least %s", field, minimum)
	}
	if maximum > 0 && parsed > maximum {
		return 0, fmt.Errorf("%s must be at most %s", field, maximum)
	}
	return parsed, nil
}

func parseMaxCount(value, maximum int, field string) error {
	if value < 1 {
		return fmt.Errorf("%s must be at least 1", field)
	}
	if value > maximum {
		return fmt.Errorf("%s must be at most %d", field, maximum)
	}
	return nil
}

func compileBindings(where string, raw []fileBinding) ([]Binding, error) {
	bindings := make([]Binding, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	seenActivity := make(map[string]struct{})
	for i, fileBinding := range raw {
		binding, err := compileBinding(fmt.Sprintf("%s.bindings[%d]", where, i), fileBinding, seenActivity)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[binding.Name]; exists {
			return nil, fmt.Errorf("%s.bindings[%d]: duplicate binding name %q", where, i, binding.Name)
		}
		seen[binding.Name] = struct{}{}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

func compileBinding(where string, raw fileBinding, seenActivity map[string]struct{}) (Binding, error) {
	if !idPattern.MatchString(raw.Name) {
		return Binding{}, fmt.Errorf("%s: name must be a stable token", where)
	}
	if strings.TrimSpace(raw.Version) == "" || strings.ContainsAny(raw.Version, " \t\r\n") {
		return Binding{}, fmt.Errorf("%s: version is required", where)
	}
	if strings.TrimSpace(raw.DisplayName) == "" {
		return Binding{}, fmt.Errorf("%s: displayName is required", where)
	}
	if strings.TrimSpace(raw.StatusQuery) == "" || strings.ContainsAny(raw.StatusQuery, " \t\r\n") {
		return Binding{}, fmt.Errorf("%s: statusQuery is required", where)
	}
	allowlist, err := compileAllowlist(where, raw.ActivityAllowlist, seenActivity)
	if err != nil {
		return Binding{}, err
	}
	actions, err := compileActions(where, raw.Actions)
	if err != nil {
		return Binding{}, err
	}
	return Binding{
		Name:              raw.Name,
		Version:           raw.Version,
		DisplayName:       raw.DisplayName,
		StatusQuery:       raw.StatusQuery,
		ActivityAllowlist: allowlist,
		Actions:           actions,
	}, nil
}

func compileAllowlist(where string, raw []fileActivity, seen map[string]struct{}) ([]ActivityJob, error) {
	allowlist := make([]ActivityJob, 0, len(raw))
	for i, activity := range raw {
		activityWhere := fmt.Sprintf("%s.activityAllowlist[%d]", where, i)
		activityType := strings.TrimSpace(activity.ActivityType)
		if activityType == "" || strings.ContainsAny(activityType, " \t\r\n") {
			return nil, fmt.Errorf("%s: activityType is required", activityWhere)
		}
		if len(activity.Jobs) == 0 {
			return nil, fmt.Errorf("%s: at least one local job is required", activityWhere)
		}
		if _, exists := seen[activityType]; exists {
			return nil, fmt.Errorf("%s: duplicate activity type", activityWhere)
		}
		seen[activityType] = struct{}{}
		jobs := make([]string, 0, len(activity.Jobs))
		jobSeen := make(map[string]struct{}, len(activity.Jobs))
		for j, job := range activity.Jobs {
			alias := strings.TrimSpace(job)
			if alias == "" || strings.ContainsAny(alias, " \t\r\n") {
				return nil, fmt.Errorf("%s.jobs[%d]: local job alias is required", activityWhere, j)
			}
			if _, exists := jobSeen[alias]; exists {
				return nil, fmt.Errorf("%s.jobs[%d]: duplicate local job alias", activityWhere, j)
			}
			jobSeen[alias] = struct{}{}
			jobs = append(jobs, alias)
		}
		allowlist = append(allowlist, ActivityJob{ActivityType: activityType, Jobs: jobs})
	}
	return allowlist, nil
}

func compileActions(where string, raw []fileAction) ([]Action, error) {
	actions := make([]Action, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, action := range raw {
		actionWhere := fmt.Sprintf("%s.actions[%d]", where, i)
		if !idPattern.MatchString(action.Name) {
			return nil, fmt.Errorf("%s: name must be a stable token", actionWhere)
		}
		if _, exists := seen[action.Name]; exists {
			return nil, fmt.Errorf("%s: duplicate action name %q", actionWhere, action.Name)
		}
		seen[action.Name] = struct{}{}
		input, err := compileSchema(actionWhere+".inputSchema", action.InputSchema)
		if err != nil {
			return nil, err
		}
		result, err := compileSchema(actionWhere+".resultSchema", action.ResultSchema)
		if err != nil {
			return nil, err
		}
		actions = append(actions, Action{Name: action.Name, InputSchema: input, ResultSchema: result})
	}
	return actions, nil
}

func compileSchema(where string, schema map[string]any) (json.RawMessage, error) {
	if len(schema) == 0 {
		return nil, fmt.Errorf("%s: schema is required", where)
	}
	if err := validateSchemaDocument(schema); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	schemaType, _ := schema["type"].(string)
	if schemaType != "object" {
		return nil, fmt.Errorf("%s: schema must be a JSON object schema", where)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("%s: schema is not valid JSON schema", where)
	}
	if err := compileJSONSchema(raw); err != nil {
		return nil, fmt.Errorf("%s: schema is not valid JSON schema", where)
	}
	return raw, nil
}

func validateSchemaDocument(schema map[string]any) error {
	// The default additionalProperties is true, which would let a caller
	// submit the reserved actor field even when no property declares it.
	if schema["additionalProperties"] != false {
		return errors.New("schema must set additionalProperties to false")
	}
	if schemaDeclaresReserved(schema) {
		return fmt.Errorf("reserved field %q cannot be declared or overridden", ReservedActorField)
	}
	if err := rejectExternalRefs(schema); err != nil {
		return err
	}
	return nil
}

func schemaDeclaresReserved(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == ReservedActorField {
				return true
			}
			if key == "patternProperties" {
				patterns, _ := child.(map[string]any)
				for pattern := range patterns {
					re, err := regexp.Compile(pattern)
					if err == nil && re.MatchString(ReservedActorField) {
						return true
					}
				}
			}
			if key == "propertyNames" && propertyNamesMatchReserved(child) {
				return true
			}
			if schemaDeclaresReserved(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if schemaDeclaresReserved(child) {
				return true
			}
		}
	case string:
		if typed == ReservedActorField {
			return true
		}
	}
	return false
}

func propertyNamesMatchReserved(schema any) bool {
	obj, ok := schema.(map[string]any)
	if !ok {
		return false
	}
	pattern, _ := obj["pattern"].(string)
	if pattern == "" {
		return false
	}
	re, err := regexp.Compile(pattern)
	return err == nil && re.MatchString(ReservedActorField)
}

func rejectExternalRefs(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" || key == "$id" || key == "$schema" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return errors.New("schema references must be fragments inside this document")
				}
			}
			if err := rejectExternalRefs(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := rejectExternalRefs(child); err != nil {
				return err
			}
		}
	}
	return nil
}

type refuseExternalSchema struct{}

func (refuseExternalSchema) Load(string) (any, error) {
	return nil, errors.New("external schema references are not allowed")
}

func compileJSONSchema(raw json.RawMessage) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(refuseExternalSchema{})
	const resource = "https://caesium.local/connector-schema.json"
	if err := compiler.AddResource(resource, doc); err != nil {
		return err
	}
	_, err = compiler.Compile(resource)
	return err
}

func resolveRawEnvSecrets(raw fileDoc, resolver secret.Resolver) (map[string]string, error) {
	resolved := map[string]string{}
	if resolver == nil {
		return resolved, nil
	}
	for _, conn := range raw.Connections {
		if conn.Credentials == nil || (conn.Enabled != nil && !*conn.Enabled) {
			continue
		}
		for _, ref := range conn.Credentials.SecretRefs {
			parsed, err := secret.Parse(ref)
			if err != nil || parsed.URL == nil || parsed.URL.User != nil || parsed.Provider != "env" {
				continue
			}
			value, err := resolver.Resolve(context.Background(), ref)
			if err != nil {
				return resolved, fmt.Errorf("connection %q: resolve env secret reference: %w", conn.ID, err)
			}
			if strings.TrimSpace(value) == "" {
				return resolved, fmt.Errorf("connection %q: env secret reference resolved to an empty value", conn.ID)
			}
			resolved[ref] = value
		}
	}
	return resolved, nil
}

func documentContent(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		node = node.Content[0]
	}
	return node
}

func walkDocument(node *yaml.Node, fieldPath string) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.AliasNode:
		return fmt.Errorf("%s: YAML aliases are not allowed", fieldPath)
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" {
				return fmt.Errorf("%s: mapping key must be a string", fieldPath)
			}
			if _, exists := seen[key.Value]; exists {
				return fmt.Errorf("%s: duplicate field %q", fieldPath, key.Value)
			}
			seen[key.Value] = struct{}{}
			childPath := key.Value
			if fieldPath != "" {
				childPath = fieldPath + "." + key.Value
			}
			if key.Value == ReservedActorField {
				return fmt.Errorf("%s: reserved field %q cannot be declared or overridden", childPath, ReservedActorField)
			}
			if err := walkDocument(node.Content[i+1], childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			if err := walkDocument(child, fmt.Sprintf("%s[%d]", fieldPath, i)); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if err := validateScalar(node, fieldPath); err != nil {
			return err
		}
	}
	return nil
}

func validateScalar(node *yaml.Node, fieldPath string) error {
	if inSchema(fieldPath) {
		return validateSchemaScalar(node, fieldPath)
	}
	if !isConfigInteger(fieldPath) {
		return nil
	}
	if node.ShortTag() != "!!int" || !canonicalDecimal.MatchString(node.Value) {
		return fmt.Errorf("%s must be a base-10 integer", fieldPath)
	}
	return nil
}

func inSchema(fieldPath string) bool {
	return strings.Contains(fieldPath, ".inputSchema") || strings.Contains(fieldPath, ".resultSchema") ||
		fieldPath == "inputSchema" || fieldPath == "resultSchema"
}

func isConfigInteger(fieldPath string) bool {
	if fieldPath == "version" {
		return true
	}
	if !strings.Contains(fieldPath, ".limits.") {
		return false
	}
	switch lastComponent(fieldPath) {
	case "readsPerMinute", "queriesPerMinute", "maxInFlightRPCs", "maxPageEntries", "maxPageMetadataBytes", "maxUnreferencedSnapshots":
		return true
	default:
		return false
	}
}

func lastComponent(fieldPath string) string {
	last := fieldPath
	if i := strings.LastIndex(last, "."); i >= 0 {
		last = last[i+1:]
	}
	if i := strings.Index(last, "["); i >= 0 {
		last = last[:i]
	}
	return last
}

func validateSchemaScalar(node *yaml.Node, fieldPath string) error {
	switch node.ShortTag() {
	case "!!str":
		return nil
	case "!!bool":
		if node.Value == "true" || node.Value == "false" {
			return nil
		}
	case "!!null":
		return nil
	case "!!int":
		if canonicalDecimal.MatchString(node.Value) {
			return nil
		}
	case "!!float":
		if jsonNumber.MatchString(node.Value) {
			return nil
		}
	}
	return fmt.Errorf("%s: schema scalar must be a JSON string, number, boolean, or null", fieldPath)
}

func isCredentialValue(fieldPath string) bool {
	return strings.Contains(fieldPath, ".credentials.secretRefs") ||
		strings.Contains(fieldPath, ".credentials.certificatePaths") ||
		strings.HasPrefix(fieldPath, "credentials.secretRefs") ||
		strings.HasPrefix(fieldPath, "credentials.certificatePaths")
}

type redactor struct {
	secrets []string
	public  []string
}

func newRedactor(root *yaml.Node) *redactor {
	redact := &redactor{}
	collectRedaction(root, "", redact)
	return redact
}

func collectRedaction(node *yaml.Node, fieldPath string, redact *redactor) {
	if node == nil || redact == nil {
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Value != "" {
				redact.public = append(redact.public, key.Value)
			}
			childPath := key.Value
			if fieldPath != "" {
				childPath = fieldPath + "." + key.Value
			}
			collectRedaction(node.Content[i+1], childPath, redact)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			collectRedaction(child, fmt.Sprintf("%s[%d]", fieldPath, i), redact)
		}
	case yaml.ScalarNode:
		if node.Value == "" {
			return
		}
		if isCredentialValue(fieldPath) {
			redact.secrets = append(redact.secrets, node.Value)
			return
		}
		redact.public = append(redact.public, node.Value)
	}
}

func (r *redactor) addSecrets(resolved map[string]string) {
	if r == nil {
		return
	}
	for _, value := range resolved {
		r.secrets = append(r.secrets, value)
	}
}

func (r *redactor) wrap(err error) error {
	if err == nil || r == nil {
		return err
	}
	message := err.Error()
	for _, value := range r.secrets {
		if len(value) < minRedactedSecretLen || r.publicContains(value) {
			continue
		}
		message = strings.ReplaceAll(message, value, "[redacted]")
	}
	if message == err.Error() {
		return err
	}
	return errors.New(message)
}

func (r *redactor) publicContains(value string) bool {
	for _, public := range r.public {
		if strings.Contains(public, value) {
			return true
		}
	}
	return false
}

func sanitizeYAMLError(err error) error {
	if err == nil {
		return nil
	}
	message := yamlQuoted.ReplaceAllString(err.Error(), "`[redacted]`")
	return fmt.Errorf("connector config rejected: %s", message)
}

// sortedCopy returns a sorted copy and leaves the input order unchanged.
func sortedCopy(values []string) []string {
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}
