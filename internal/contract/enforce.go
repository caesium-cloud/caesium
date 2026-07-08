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

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/caesium-cloud/caesium/pkg/log"
	"gorm.io/gorm"
)

const (
	EnforcementModeOff  = ""
	EnforcementModeWarn = "warn"
	EnforcementModeFail = "fail"
)

var (
	ErrContractBreakBlocked = errors.New("contract breaking change blocked")
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
	findings []ContractFinding
	digest   string
}

type digestInput struct {
	Version int             `json:"version"`
	Subject string          `json:"subject"`
	Edges   []digestFinding `json:"edges"`
}

type digestFinding struct {
	EdgeID       string `json:"edge_id"`
	EdgeClass    string `json:"edge_class"`
	Dataset      string `json:"dataset,omitempty"`
	OutputKey    string `json:"output_key,omitempty"`
	Producer     string `json:"producer"`
	Consumer     string `json:"consumer"`
	ConsumerTeam string `json:"consumer_team,omitempty"`
	Path         string `json:"path"`
	Detail       string `json:"detail"`
	Verdict      string `json:"verdict"`
}

// EnforceApply derives the merged contract graph for incoming definitions and
// applies warn/fail enforcement. Off-mode returns before graph derivation so the
// apply path is fully inert when CAESIUM_CONTRACT_ENFORCEMENT is unset.
func EnforceApply(ctx context.Context, db *gorm.DB, incoming []schema.Definition, mode string, now time.Time) error {
	mode = normalizeMode(mode)
	if mode == EnforcementModeOff {
		return nil
	}
	if mode != EnforcementModeWarn && mode != EnforcementModeFail {
		return fmt.Errorf("unsupported contract enforcement mode %q", mode)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	graph, err := NewGORMDeriver(db).DeriveGraph(ctx, incoming)
	if err != nil {
		return err
	}
	return EvaluateGraph(ctx, db, graph, mode, now)
}

// EvaluateGraph applies enforcement to an already-derived graph. It is split
// out so tests and future lint/diff surfaces can reuse the exact digest logic.
func EvaluateGraph(ctx context.Context, db *gorm.DB, graph Graph, mode string, now time.Time) error {
	mode = normalizeMode(mode)
	if mode == EnforcementModeOff {
		return nil
	}
	if mode != EnforcementModeWarn && mode != EnforcementModeFail {
		return fmt.Errorf("unsupported contract enforcement mode %q", mode)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	groups, err := breakingGroupsFromGraph(graph)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}

	if mode == EnforcementModeWarn {
		for _, group := range groups {
			log.Warn("contract breaking change detected in warn mode",
				"subject", group.subject,
				"edge_set_digest", group.digest,
				"findings", len(group.findings),
			)
		}
		return nil
	}

	blocked := make([]ContractFinding, 0)
	for _, group := range groups {
		ackID, ok, err := validAckForDigest(ctx, db, group.digest, now)
		if err != nil {
			return err
		}
		if ok {
			for idx := range group.findings {
				group.findings[idx].Acknowledged = true
				group.findings[idx].AcknowledgementID = ackID
			}
			log.Warn("contract breaking change allowed by active acknowledgement",
				"subject", group.subject,
				"edge_set_digest", group.digest,
				"acknowledgement_id", ackID,
				"findings", len(group.findings),
			)
			continue
		}

		metrics.ContractBreaksBlockedTotal.WithLabelValues(group.subject).Inc()
		blocked = append(blocked, group.findings...)
	}
	if len(blocked) == 0 {
		return nil
	}
	sortContractFindings(blocked)
	return &EnforcementError{
		Response: EnforcementResponse{
			Error:    "contract_breaking_change",
			Message:  contractBreakMessage(blocked),
			Findings: blocked,
		},
	}
}

func breakingGroupsFromGraph(graph Graph) ([]breakingGroup, error) {
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
			key := cf.Producer + "\x00" + cf.Subject
			group := grouped[key]
			if group == nil {
				group = &breakingGroup{subject: cf.Subject}
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

func contractFindingFromEdge(edge Edge, finding schemacompat.Finding, nodesByID map[string]Node) ContractFinding {
	producer := strings.TrimPrefix(strings.TrimSpace(edge.From), "job:")
	consumer := strings.TrimPrefix(strings.TrimSpace(edge.To), "job:")
	consumerTeam := strings.TrimSpace(nodesByID[edge.To].Labels["team"])
	teamAttribution := ""
	if consumerTeam == "" {
		teamAttribution = "deferred: metadata.labels.team not set"
	}

	dataset := datasetKey(edge.Dataset)
	outputKey := ""
	subject := dataset
	if subject == "" {
		outputKey = outputKeyFromDetail(finding.Detail)
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

func outputKeyFromDetail(detail string) string {
	matches := outputKeyDetailPattern.FindStringSubmatch(detail)
	if len(matches) != 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func edgeSetDigest(subject string, findings []ContractFinding) (string, error) {
	input := digestInput{
		Version: 1,
		Subject: subject,
		Edges:   make([]digestFinding, 0, len(findings)),
	}
	for _, finding := range findings {
		input.Edges = append(input.Edges, digestFinding{
			EdgeID:       finding.EdgeID,
			EdgeClass:    finding.EdgeClass,
			Dataset:      finding.Dataset,
			OutputKey:    finding.OutputKey,
			Producer:     finding.Producer,
			Consumer:     finding.Consumer,
			ConsumerTeam: finding.ConsumerTeam,
			Path:         finding.Path,
			Detail:       finding.Detail,
			Verdict:      finding.Verdict,
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

func validAckForDigest(ctx context.Context, db *gorm.DB, digest string, now time.Time) (string, bool, error) {
	if db == nil {
		return "", false, nil
	}
	var ack models.ContractAck
	err := db.WithContext(ctx).
		Where("edge_set_digest = ?", digest).
		Where("expires_at > ?", now).
		Order("expires_at DESC").
		First(&ack).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return ack.ID.String(), true, nil
}

func contractBreakMessage(findings []ContractFinding) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		team := "team attribution deferred: metadata.labels.team not set"
		if finding.ConsumerTeam != "" {
			team = "team: " + finding.ConsumerTeam
		}
		subject := finding.Subject
		if finding.OutputKey != "" {
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

func normalizeMode(mode string) string {
	return strings.ToLower(strings.TrimSpace(mode))
}
