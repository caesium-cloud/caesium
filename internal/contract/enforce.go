package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	EnforcementModeOff  = ""
	EnforcementModeWarn = "warn"
	EnforcementModeFail = "fail"
)

var (
	ErrContractBreakBlocked = errors.New("contract breaking change blocked")
	ErrInvalidContractAck   = errors.New("invalid contract breaking acknowledgement")
	outputKeyDetailPattern  = regexp.MustCompile(`(?:output key|key) "([^"]+)"`)
)

// EnforcementError is returned when fail-mode enforcement sees an unacknowledged
// breaking edge set.
type EnforcementError struct {
	Response EnforcementResponse
}

func (e *EnforcementError) Error() string {
	if strings.TrimSpace(e.Response.Message) != "" {
		return e.Response.Message
	}
	return ErrContractBreakBlocked.Error()
}

func (e *EnforcementError) Unwrap() error {
	return ErrContractBreakBlocked
}

// EnforcementResponse is the stable machine-readable 409 payload consumed by
// CLI, REST, and future UI surfaces.
type EnforcementResponse struct {
	Error    string            `json:"error"`
	Message  string            `json:"message"`
	Findings []ContractFinding `json:"findings"`
}

// ContractWarning is returned on successful apply calls that rely on an active
// deprecation acknowledgement.
type ContractWarning struct {
	Type              string    `json:"type"`
	Message           string    `json:"message"`
	Subject           string    `json:"subject"`
	Dataset           string    `json:"dataset,omitempty"`
	OutputKey         string    `json:"output_key,omitempty"`
	Producer          string    `json:"producer"`
	Consumer          string    `json:"consumer,omitempty"`
	ConsumerTeam      string    `json:"consumer_team,omitempty"`
	EdgeSetDigest     string    `json:"edge_set_digest"`
	AcknowledgementID string    `json:"acknowledgement_id"`
	DeprecationUntil  time.Time `json:"deprecation_until"`
}

// ContractFinding names one impacted consumer for a breaking contract edge.
type ContractFinding struct {
	Subject           string `json:"subject"`
	Dataset           string `json:"dataset,omitempty"`
	OutputKey         string `json:"output_key,omitempty"`
	Producer          string `json:"producer"`
	Consumer          string `json:"consumer"`
	ConsumerTeam      string `json:"consumer_team,omitempty"`
	TeamAttribution   string `json:"team_attribution,omitempty"`
	EdgeClass         string `json:"edge_class"`
	Path              string `json:"path"`
	Detail            string `json:"detail"`
	Verdict           string `json:"verdict"`
	EdgeID            string `json:"edge_id"`
	EdgeSetDigest     string `json:"edge_set_digest"`
	Acknowledged      bool   `json:"acknowledged"`
	AcknowledgementID string `json:"acknowledgement_id,omitempty"`
}

type breakingGroup struct {
	subject  string
	producer string
	findings []ContractFinding
	digest   string
}

// AllowBreaking scopes one intentional break acknowledgement requested by an
// apply caller. The CLI grammar is --allow-breaking dataset=<subject>, where
// subject is a declared dataset name or an inferred producer.output.<key>
// subject.
type AllowBreaking struct {
	Dataset string
	Reason  string
	Actor   string
}

// ApplyOptions controls acknowledgement creation and warning attribution.
type ApplyOptions struct {
	AllowBreaking      *AllowBreaking
	DeprecationWindow  time.Duration
	EventStore         EventAppender
	IncomingAliases    map[string]struct{}
	SuppressAckNoMatch bool
}

// EnforcementResult carries non-blocking warnings produced by enforcement.
type EnforcementResult struct {
	Warnings []ContractWarning
}

// EventAppender is the transactional subset of internal/event.Store used to
// persist contract-break notifications without tying tests to the concrete type.
type EventAppender interface {
	AppendTx(*gorm.DB, *event.Event) error
}

type digestInput struct {
	Version int             `json:"version"`
	Subject string          `json:"subject"`
	Edges   []digestFinding `json:"edges"`
}

type digestFinding struct {
	EdgeID    string `json:"edge_id"`
	EdgeClass string `json:"edge_class"`
	Dataset   string `json:"dataset,omitempty"`
	OutputKey string `json:"output_key,omitempty"`
	Producer  string `json:"producer"`
	Consumer  string `json:"consumer"`
	Path      string `json:"path"`
	Detail    string `json:"detail"`
	Verdict   string `json:"verdict"`
}

// EnforceApply derives the merged contract graph for incoming definitions and
// applies warn/fail enforcement. Off-mode returns before graph derivation so the
// apply path is fully inert when CAESIUM_CONTRACT_ENFORCEMENT is unset.
func EnforceApply(ctx context.Context, db *gorm.DB, incoming []schema.Definition, mode string, now time.Time) error {
	_, err := EnforceApplyWithOptions(ctx, db, incoming, mode, now, ApplyOptions{})
	return err
}

// EnforceApplyWithOptions derives the graph and applies enforcement, optionally
// recording a bounded acknowledgement for one matching breaking subject.
func EnforceApplyWithOptions(ctx context.Context, db *gorm.DB, incoming []schema.Definition, mode string, now time.Time, opts ApplyOptions) (EnforcementResult, error) {
	mode = normalizeMode(mode)
	if mode == EnforcementModeOff {
		return EnforcementResult{}, nil
	}
	if mode != EnforcementModeWarn && mode != EnforcementModeFail {
		return EnforcementResult{}, fmt.Errorf("unsupported contract enforcement mode %q", mode)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if opts.IncomingAliases == nil {
		opts.IncomingAliases = incomingAliasSet(incoming)
	}

	graph, err := NewGORMDeriver(db).DeriveGraph(ctx, incoming)
	if err != nil {
		return EnforcementResult{}, err
	}
	return EvaluateGraphWithOptions(ctx, db, graph, mode, now, opts)
}

// EvaluateGraph applies enforcement to an already-derived graph. It is split
// out so tests and future lint/diff surfaces can reuse the exact digest logic.
func EvaluateGraph(ctx context.Context, db *gorm.DB, graph Graph, mode string, now time.Time) error {
	_, err := EvaluateGraphWithOptions(ctx, db, graph, mode, now, ApplyOptions{})
	return err
}

// EvaluateGraphWithOptions applies enforcement and optionally writes an
// acknowledgement for a matching breaking subject.
func EvaluateGraphWithOptions(ctx context.Context, db *gorm.DB, graph Graph, mode string, now time.Time, opts ApplyOptions) (EnforcementResult, error) {
	mode = normalizeMode(mode)
	if mode == EnforcementModeOff {
		return EnforcementResult{}, nil
	}
	if mode != EnforcementModeWarn && mode != EnforcementModeFail {
		return EnforcementResult{}, fmt.Errorf("unsupported contract enforcement mode %q", mode)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if opts.DeprecationWindow == 0 {
		opts.DeprecationWindow = 14 * 24 * time.Hour
	}
	if opts.DeprecationWindow < 0 {
		return EnforcementResult{}, fmt.Errorf("contract deprecation window must be greater than 0")
	}

	groups, err := breakingGroupsFromGraph(graph, opts.IncomingAliases)
	if err != nil {
		return EnforcementResult{}, err
	}
	if len(groups) == 0 {
		return EnforcementResult{}, nil
	}

	if mode == EnforcementModeWarn {
		for _, group := range groups {
			log.Warn("contract breaking change detected in warn mode",
				"subject", group.subject,
				"edge_set_digest", group.digest,
				"findings", len(group.findings),
			)
		}
		return EnforcementResult{}, nil
	}

	result := EnforcementResult{}
	blocked := make([]ContractFinding, 0)
	allowMatched := false
	for _, group := range groups {
		ack, ok, err := validAckForDigest(ctx, db, group.digest, now)
		if err != nil {
			return EnforcementResult{}, err
		}
		if ok {
			for idx := range group.findings {
				group.findings[idx].Acknowledged = true
				group.findings[idx].AcknowledgementID = ack.ID.String()
			}
			log.Warn("contract breaking change allowed by active acknowledgement",
				"subject", group.subject,
				"edge_set_digest", group.digest,
				"acknowledgement_id", ack.ID.String(),
				"findings", len(group.findings),
			)
			if opts.AllowBreaking != nil && allowBreakingMatches(opts.AllowBreaking, group) {
				allowMatched = true
			}
			result.Warnings = append(result.Warnings, warningsForAcknowledgedGroup(group, ack, opts.IncomingAliases)...)
			continue
		}

		if opts.AllowBreaking != nil && allowBreakingMatches(opts.AllowBreaking, group) {
			allowMatched = true
			ack, err := createContractAck(ctx, db, group, opts.AllowBreaking, opts.DeprecationWindow, now)
			if err != nil {
				return EnforcementResult{}, err
			}
			for idx := range group.findings {
				group.findings[idx].Acknowledged = true
				group.findings[idx].AcknowledgementID = ack.ID.String()
			}
			if err := emitContractBreakDeclaredEvents(ctx, db, opts.EventStore, group, ack); err != nil {
				return EnforcementResult{}, err
			}
			result.Warnings = append(result.Warnings, warningForDeclaredGroup(group, ack))
			continue
		}

		metrics.ContractBreaksBlockedTotal.WithLabelValues(group.subject).Inc()
		blocked = append(blocked, group.findings...)
	}
	if len(blocked) == 0 {
		if opts.AllowBreaking != nil && !allowMatched && !opts.SuppressAckNoMatch {
			return EnforcementResult{}, fmt.Errorf("%w: dataset %q did not match any breaking contract subject", ErrInvalidContractAck, strings.TrimSpace(opts.AllowBreaking.Dataset))
		}
		sortContractWarnings(result.Warnings)
		return result, nil
	}
	sortContractFindings(blocked)
	return EnforcementResult{}, &EnforcementError{
		Response: EnforcementResponse{
			Error:    "contract_breaking_change",
			Message:  contractBreakMessage(blocked),
			Findings: blocked,
		},
	}
}

func breakingGroupsFromGraph(graph Graph, incomingAliases map[string]struct{}) ([]breakingGroup, error) {
	nodesByID := make(map[string]Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodesByID[node.ID] = node
	}

	grouped := map[string]*breakingGroup{}
	for _, edge := range graph.Edges {
		if edge.Verdict != schemacompat.VerdictBreaking {
			continue
		}
		for _, finding := range edge.Findings {
			if finding.Verdict != schemacompat.VerdictBreaking {
				continue
			}
			cf := contractFindingFromEdge(edge, finding, nodesByID)
			if !contractFindingInIncomingScope(cf, incomingAliases) {
				continue
			}
			key := cf.Producer + "\x00" + cf.Subject
			group := grouped[key]
			if group == nil {
				group = &breakingGroup{subject: cf.Subject, producer: cf.Producer}
				grouped[key] = group
			}
			group.findings = append(group.findings, cf)
		}
	}

	groups := make([]breakingGroup, 0, len(grouped))
	for _, group := range grouped {
		sortContractFindings(group.findings)
		digest, err := edgeSetDigest(group.subject, group.findings)
		if err != nil {
			return nil, err
		}
		group.digest = digest
		for idx := range group.findings {
			group.findings[idx].EdgeSetDigest = digest
		}
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].subject == groups[j].subject {
			return groups[i].digest < groups[j].digest
		}
		return groups[i].subject < groups[j].subject
	})
	return groups, nil
}

func contractFindingInIncomingScope(finding ContractFinding, incomingAliases map[string]struct{}) bool {
	if incomingAliases == nil {
		return true
	}
	if _, ok := incomingAliases[strings.TrimSpace(finding.Producer)]; ok {
		return true
	}
	if _, ok := incomingAliases[strings.TrimSpace(finding.Consumer)]; ok {
		return true
	}
	return false
}

func contractFindingFromEdge(edge Edge, finding schemacompat.Finding, nodesByID map[string]Node) ContractFinding {
	producer := strings.TrimPrefix(strings.TrimSpace(edge.From), "job:")
	consumer := strings.TrimPrefix(strings.TrimSpace(edge.To), "job:")
	consumerTeam := strings.TrimSpace(nodesByID[edge.To].Labels["team"])
	teamAttribution := ""
	if consumerTeam == "" {
		teamAttribution = "deferred: metadata.labels.team not set"
	}

	dataset := datasetKey(edge.Dataset)
	outputKey := outputKeyFromFinding(finding)
	subject := dataset
	if subject == "" {
		if outputKey != "" {
			subject = producer + ".output." + outputKey
		} else {
			subject = producer + ".contract"
		}
	}

	return ContractFinding{
		Subject:         subject,
		Dataset:         dataset,
		OutputKey:       outputKey,
		Producer:        producer,
		Consumer:        consumer,
		ConsumerTeam:    consumerTeam,
		TeamAttribution: teamAttribution,
		EdgeClass:       string(edge.Class),
		Path:            finding.Path,
		Detail:          finding.Detail,
		Verdict:         string(finding.Verdict),
		EdgeID:          edge.ID,
	}
}

func outputKeyFromFinding(finding schemacompat.Finding) string {
	if key := strings.TrimSpace(finding.Key); key != "" {
		return key
	}
	return outputKeyFromDetail(finding.Detail)
}

func outputKeyFromDetail(detail string) string {
	matches := outputKeyDetailPattern.FindStringSubmatch(detail)
	if len(matches) != 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func edgeSetDigest(subject string, findings []ContractFinding) (string, error) {
	input := digestInput{
		Version: 2,
		Subject: subject,
		Edges:   make([]digestFinding, 0, len(findings)),
	}
	for _, finding := range findings {
		input.Edges = append(input.Edges, digestFinding{
			EdgeID:    finding.EdgeID,
			EdgeClass: finding.EdgeClass,
			Dataset:   finding.Dataset,
			OutputKey: finding.OutputKey,
			Producer:  finding.Producer,
			Consumer:  finding.Consumer,
			Path:      finding.Path,
			Detail:    finding.Detail,
			Verdict:   finding.Verdict,
		})
	}
	sort.Slice(input.Edges, func(i, j int) bool {
		left := input.Edges[i]
		right := input.Edges[j]
		for _, cmp := range []int{
			strings.Compare(left.Producer, right.Producer),
			strings.Compare(left.Consumer, right.Consumer),
			strings.Compare(left.EdgeID, right.EdgeID),
			strings.Compare(left.Path, right.Path),
			strings.Compare(left.Detail, right.Detail),
		} {
			if cmp < 0 {
				return true
			}
			if cmp > 0 {
				return false
			}
		}
		return false
	})

	payload, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func validAckForDigest(ctx context.Context, db *gorm.DB, digest string, now time.Time) (*models.ContractAck, bool, error) {
	if db == nil {
		return nil, false, nil
	}
	var ack models.ContractAck
	err := db.WithContext(ctx).
		Where("edge_set_digest = ?", digest).
		Where("expires_at > ?", now).
		Order("expires_at DESC").
		First(&ack).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &ack, true, nil
}

func createContractAck(ctx context.Context, db *gorm.DB, group breakingGroup, allow *AllowBreaking, window time.Duration, now time.Time) (*models.ContractAck, error) {
	if db == nil {
		return nil, errors.New("contract acknowledgement requires database connection")
	}
	if allow == nil {
		return nil, errors.New("contract acknowledgement request is required")
	}
	actor := strings.TrimSpace(allow.Actor)
	if actor == "" {
		actor = "anonymous"
	}
	ack := &models.ContractAck{
		ID:            uuid.New(),
		Dataset:       ackDatasetForGroup(group, allow),
		EdgeSetDigest: group.digest,
		Actor:         actor,
		Reason:        strings.TrimSpace(allow.Reason),
		CreatedAt:     now,
		ExpiresAt:     now.Add(window),
	}
	if err := db.WithContext(ctx).Create(ack).Error; err != nil {
		return nil, err
	}
	return ack, nil
}

func emitContractBreakDeclaredEvents(ctx context.Context, db *gorm.DB, store EventAppender, group breakingGroup, ack *models.ContractAck) error {
	if store == nil || ack == nil {
		return nil
	}
	for _, consumer := range contractBreakConsumers(ctx, db, group) {
		payload, err := json.Marshal(map[string]any{
			"job_alias":           consumer.Alias,
			"job_labels":          consumer.Labels,
			"producer":            group.producer,
			"dataset":             displayDatasetName(datasetForGroup(group)),
			"subject":             group.subject,
			"offending_keys":      offendingKeys(group.findings),
			"window_end":          ack.ExpiresAt,
			"deprecation_until":   ack.ExpiresAt,
			"actor":               ack.Actor,
			"reason":              ack.Reason,
			"edge_set_digest":     group.digest,
			"acknowledgement_id":  ack.ID.String(),
			"consumer":            consumer.Alias,
			"consumer_team":       consumer.Labels["team"],
			"impacted_consumers":  impactedConsumers(group.findings),
			"breaking_findings":   group.findings,
			"notification_target": "consumer",
		})
		if err != nil {
			return err
		}
		evt := event.Event{
			Type:      event.TypeContractBreakDeclared,
			JobID:     consumer.ID,
			Timestamp: ack.CreatedAt,
			Payload:   payload,
		}
		if err := store.AppendTx(db, &evt); err != nil {
			return err
		}
	}
	return nil
}

type contractBreakConsumer struct {
	ID     uuid.UUID
	Alias  string
	Labels map[string]string
}

func contractBreakConsumers(ctx context.Context, db *gorm.DB, group breakingGroup) []contractBreakConsumer {
	byAlias := map[string]contractBreakConsumer{}
	for _, finding := range group.findings {
		alias := strings.TrimSpace(finding.Consumer)
		if alias == "" {
			continue
		}
		labels := map[string]string{}
		if finding.ConsumerTeam != "" {
			labels["team"] = finding.ConsumerTeam
		}
		byAlias[alias] = contractBreakConsumer{Alias: alias, Labels: labels}
	}
	if db == nil || len(byAlias) == 0 {
		return sortedContractBreakConsumers(byAlias)
	}

	aliases := make([]string, 0, len(byAlias))
	for alias := range byAlias {
		aliases = append(aliases, alias)
	}
	var jobs []models.Job
	if err := db.WithContext(ctx).Where("alias IN ?", aliases).Find(&jobs).Error; err != nil {
		log.Warn("contract break declared event could not load consumer jobs", "error", err)
		return sortedContractBreakConsumers(byAlias)
	}
	for _, job := range jobs {
		consumer := byAlias[job.Alias]
		consumer.ID = job.ID
		if len(job.Labels) > 0 {
			consumer.Labels = map[string]string{}
			for key, value := range job.Labels {
				if stringValue, ok := value.(string); ok {
					consumer.Labels[key] = stringValue
				}
			}
		}
		byAlias[job.Alias] = consumer
	}
	return sortedContractBreakConsumers(byAlias)
}

func sortedContractBreakConsumers(byAlias map[string]contractBreakConsumer) []contractBreakConsumer {
	aliases := make([]string, 0, len(byAlias))
	for alias := range byAlias {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	out := make([]contractBreakConsumer, 0, len(aliases))
	for _, alias := range aliases {
		out = append(out, byAlias[alias])
	}
	return out
}

func impactedConsumers(findings []ContractFinding) []map[string]string {
	seen := map[string]map[string]string{}
	for _, finding := range findings {
		consumer := strings.TrimSpace(finding.Consumer)
		if consumer == "" {
			continue
		}
		seen[consumer] = map[string]string{
			"alias": consumer,
			"team":  finding.ConsumerTeam,
		}
	}
	aliases := make([]string, 0, len(seen))
	for alias := range seen {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	out := make([]map[string]string, 0, len(aliases))
	for _, alias := range aliases {
		out = append(out, seen[alias])
	}
	return out
}

func offendingKeys(findings []ContractFinding) []string {
	seen := map[string]struct{}{}
	for _, finding := range findings {
		key := strings.TrimSpace(finding.OutputKey)
		if key != "" {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func datasetForGroup(group breakingGroup) string {
	for _, finding := range group.findings {
		if strings.TrimSpace(finding.Dataset) != "" {
			return strings.TrimSpace(finding.Dataset)
		}
	}
	return ""
}

func displayDatasetName(dataset string) string {
	dataset = strings.TrimSpace(dataset)
	if strings.HasPrefix(dataset, "/") && !strings.Contains(strings.TrimPrefix(dataset, "/"), "/") {
		return strings.TrimPrefix(dataset, "/")
	}
	return dataset
}

func ackDatasetForGroup(group breakingGroup, allow *AllowBreaking) string {
	if allow != nil && strings.TrimSpace(allow.Dataset) != "" {
		return strings.TrimSpace(allow.Dataset)
	}
	if dataset := displayDatasetName(datasetForGroup(group)); dataset != "" {
		return dataset
	}
	return group.subject
}

func warningsForAcknowledgedGroup(group breakingGroup, ack *models.ContractAck, incomingAliases map[string]struct{}) []ContractWarning {
	if ack == nil {
		return nil
	}
	warnings := make([]ContractWarning, 0, len(group.findings))
	subject := warningSubjectForGroup(group)
	for _, finding := range group.findings {
		warningType := "contract_break_acknowledged"
		message := fmt.Sprintf("breaking contract %s from %s is in a deprecation window until %s", subject, finding.Producer, ack.ExpiresAt.Format(time.RFC3339))
		if _, ok := incomingAliases[finding.Consumer]; ok {
			warningType = "deprecated_contract_consumed"
			message = fmt.Sprintf("consuming a deprecated contract %s from %s; migrate before %s", subject, finding.Producer, ack.ExpiresAt.Format(time.RFC3339))
		}
		warnings = append(warnings, contractWarning(warningType, message, finding, ack))
	}
	return warnings
}

func warningForDeclaredGroup(group breakingGroup, ack *models.ContractAck) ContractWarning {
	finding := group.findings[0]
	keys := offendingKeys(group.findings)
	keyDetail := ""
	if len(keys) > 0 {
		keyDetail = " for key(s) " + strings.Join(keys, ", ")
	}
	message := fmt.Sprintf("contract break declared for %s%s from %s until %s", warningSubjectForGroup(group), keyDetail, group.producer, ack.ExpiresAt.Format(time.RFC3339))
	return contractWarning("contract_break_declared", message, finding, ack)
}

func contractWarning(warningType, message string, finding ContractFinding, ack *models.ContractAck) ContractWarning {
	return ContractWarning{
		Type:              warningType,
		Message:           message,
		Subject:           finding.Subject,
		Dataset:           displayDatasetName(finding.Dataset),
		OutputKey:         finding.OutputKey,
		Producer:          finding.Producer,
		Consumer:          finding.Consumer,
		ConsumerTeam:      finding.ConsumerTeam,
		EdgeSetDigest:     finding.EdgeSetDigest,
		AcknowledgementID: ack.ID.String(),
		DeprecationUntil:  ack.ExpiresAt,
	}
}

func warningSubjectForGroup(group breakingGroup) string {
	if dataset := displayDatasetName(datasetForGroup(group)); dataset != "" {
		return dataset
	}
	return group.subject
}

func allowBreakingMatches(allow *AllowBreaking, group breakingGroup) bool {
	if allow == nil {
		return false
	}
	requested := strings.TrimSpace(allow.Dataset)
	for _, candidate := range []string{
		group.subject,
		datasetForGroup(group),
		displayDatasetName(datasetForGroup(group)),
	} {
		if requested != "" && requested == strings.TrimSpace(candidate) {
			return true
		}
	}
	return false
}

func incomingAliasSet(incoming []schema.Definition) map[string]struct{} {
	aliases := make(map[string]struct{}, len(incoming))
	for _, def := range incoming {
		alias := strings.TrimSpace(def.Metadata.Alias)
		if alias != "" {
			aliases[alias] = struct{}{}
		}
	}
	return aliases
}

func contractBreakMessage(findings []ContractFinding) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		team := "team attribution deferred: metadata.labels.team not set"
		if finding.ConsumerTeam != "" {
			team = "team: " + finding.ConsumerTeam
		}
		subject := finding.Subject
		if finding.Dataset != "" && finding.OutputKey != "" {
			subject = displayDatasetName(finding.Dataset) + " key " + finding.OutputKey
		} else if finding.OutputKey != "" {
			subject = "output key " + finding.OutputKey
		}
		parts = append(parts, fmt.Sprintf("%s from %s consumed by %s (%s)", subject, finding.Producer, finding.Consumer, team))
	}
	sort.Strings(parts)
	return fmt.Sprintf("contract enforcement blocked %d breaking finding(s): %s", len(findings), strings.Join(parts, "; "))
}

func sortContractFindings(findings []ContractFinding) {
	sort.Slice(findings, func(i, j int) bool {
		left := findings[i]
		right := findings[j]
		for _, cmp := range []int{
			strings.Compare(left.Subject, right.Subject),
			strings.Compare(left.Producer, right.Producer),
			strings.Compare(left.Consumer, right.Consumer),
			strings.Compare(left.Path, right.Path),
			strings.Compare(left.Detail, right.Detail),
		} {
			if cmp < 0 {
				return true
			}
			if cmp > 0 {
				return false
			}
		}
		return false
	})
}

func sortContractWarnings(warnings []ContractWarning) {
	sort.Slice(warnings, func(i, j int) bool {
		left := warnings[i]
		right := warnings[j]
		for _, cmp := range []int{
			strings.Compare(left.Subject, right.Subject),
			strings.Compare(left.Type, right.Type),
			strings.Compare(left.Producer, right.Producer),
			strings.Compare(left.Consumer, right.Consumer),
			strings.Compare(left.Message, right.Message),
		} {
			if cmp < 0 {
				return true
			}
			if cmp > 0 {
				return false
			}
		}
		return false
	})
}

func normalizeMode(mode string) string {
	return strings.ToLower(strings.TrimSpace(mode))
}
