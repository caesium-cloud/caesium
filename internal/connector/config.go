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

var yamlQuoted = regexp.MustCompile("`[^`]*`")

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
// references through the existing env resolver so callers can prove those
// bytes stay out of errors and fingerprints. Non-env providers are accepted
// as references and are not dialed. Resolved values are returned for the
// fingerprint exclusion check and are not copied onto Config.
func Parse(data []byte, resolver secret.Resolver) (*Config, map[string]string, error) {
	root, err := decodeDocument(data)
	if err != nil {
		return nil, nil, err
	}
	redact := newRedactor(credentialScalars(root))
	if err := walkDocument(root, ""); err != nil {
		return nil, nil, redact.wrap(err)
	}
	raw, err := decodeKnown(data)
	if err != nil {
		return nil, nil, redact.wrap(err)
	}
	// Resolve before the remaining checks so a later rejection cannot echo
	// secret bytes that the env resolver already produced.
	resolved, err := resolveRawEnvSecrets(raw, resolver)
	redact.addMapValues(resolved)
	if err != nil {
		return nil, nil, redact.wrap(err)
	}
	cfg, err := compile(raw)
	if err != nil {
		return nil, nil, redact.wrap(err)
	}
	return cfg, resolved, nil
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
	if strings.TrimSpace(raw.Endpoint) == "" || strings.ContainsAny(raw.Endpoint, " \t\r\n") {
		return Connection{}, fmt.Errorf("%s: endpoint is required", where)
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

func validateSecretRef(ref string) error {
	if strings.TrimSpace(ref) == "" || strings.ContainsAny(ref, " \t\r\n") {
		return errors.New("credential must be a secret:// reference")
	}
	parsed, err := secret.Parse(ref)
	if err != nil || parsed.URL.User != nil {
		return errors.New("credential must be a secret:// reference")
	}
	switch parsed.Provider {
	case "env", "k8s", "kubernetes", "vault":
	default:
		return fmt.Errorf("unsupported secret provider %q", parsed.Provider)
	}
	if parsed.Provider == "env" {
		name := parsed.Query.Get("name")
		if name == "" {
			name = strings.Join(parsed.Segments, "_")
		}
		if strings.TrimSpace(name) == "" {
			return errors.New("env secret reference requires a name")
		}
		return nil
	}
	if parsed.Path == "" && parsed.URL.Fragment == "" {
		return errors.New("secret reference requires a path")
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
	for i, fileBinding := range raw {
		binding, err := compileBinding(fmt.Sprintf("%s.bindings[%d]", where, i), fileBinding)
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

func compileBinding(where string, raw fileBinding) (Binding, error) {
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
	allowlist, err := compileAllowlist(where, raw.ActivityAllowlist)
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

func compileAllowlist(where string, raw []fileActivity) ([]ActivityJob, error) {
	allowlist := make([]ActivityJob, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
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
	if containsReservedKey(schema) {
		return nil, fmt.Errorf("%s: reserved field %q cannot be declared or overridden", where, ReservedActorField)
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

func compileJSONSchema(raw json.RawMessage) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	const resource = "https://caesium.local/connector-schema.json"
	if err := compiler.AddResource(resource, doc); err != nil {
		return err
	}
	_, err = compiler.Compile(resource)
	return err
}

func containsReservedKey(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == ReservedActorField || containsReservedKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsReservedKey(child) {
				return true
			}
		}
	}
	return false
}

func resolveRawEnvSecrets(raw fileDoc, resolver secret.Resolver) (map[string]string, error) {
	resolved := map[string]string{}
	if resolver == nil {
		return resolved, nil
	}
	for _, conn := range raw.Connections {
		if conn.Credentials == nil {
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
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
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
	}
	return nil
}

func credentialScalars(node *yaml.Node) []string {
	var values []string
	collectCredentialScalars(node, "", &values)
	return values
}

func collectCredentialScalars(node *yaml.Node, fieldPath string, values *[]string) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			childPath := key.Value
			if fieldPath != "" {
				childPath = fieldPath + "." + key.Value
			}
			collectCredentialScalars(node.Content[i+1], childPath, values)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			childPath := fmt.Sprintf("%s[%d]", fieldPath, i)
			if child.Kind == yaml.ScalarNode && isCredentialLeaf(fieldPath) {
				*values = append(*values, child.Value)
			}
			collectCredentialScalars(child, childPath, values)
		}
	case yaml.ScalarNode:
		if isCredentialLeaf(fieldPath) {
			*values = append(*values, node.Value)
		}
	}
}

func isCredentialLeaf(fieldPath string) bool {
	return strings.Contains(fieldPath, "secretRefs") || strings.Contains(fieldPath, "certificatePaths")
}

type redactor struct {
	values []string
}

func newRedactor(values []string) *redactor {
	return &redactor{values: append([]string(nil), values...)}
}

func (r *redactor) addMapValues(resolved map[string]string) {
	if r == nil {
		return
	}
	for _, value := range resolved {
		r.values = append(r.values, value)
	}
}

func (r *redactor) wrap(err error) error {
	if err == nil || r == nil {
		return err
	}
	message := err.Error()
	for _, value := range r.values {
		if value == "" {
			continue
		}
		message = strings.ReplaceAll(message, value, "[redacted]")
	}
	if message == err.Error() {
		return err
	}
	return errors.New(message)
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
