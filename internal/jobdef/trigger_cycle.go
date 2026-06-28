package jobdef

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrTriggerChainCycle = errors.New("trigger chain cycle detected")

type triggerChainNode struct {
	id            uuid.UUID
	alias         string
	triggerType   string
	configuration map[string]any
}

type triggerChainPattern struct {
	eventType string
	source    string
	filter    map[string]string
}

func (i *Importer) ValidateBatch(ctx context.Context, defs []schema.Definition) error {
	for idx := range defs {
		if err := defs[idx].Validate(); err != nil {
			return fmt.Errorf("definition %s: %w", defs[idx].Metadata.Alias, err)
		}
	}
	return ValidateTriggerChains(ctx, i.db, defs)
}

func ValidateTriggerChains(ctx context.Context, conn *gorm.DB, defs []schema.Definition) error {
	nodes, err := triggerChainNodes(ctx, conn, defs)
	if err != nil {
		return err
	}
	graph, err := triggerChainGraph(nodes)
	if err != nil {
		return err
	}
	return rejectTriggerChainCycle(graph)
}

func triggerChainNodes(ctx context.Context, conn *gorm.DB, defs []schema.Definition) ([]triggerChainNode, error) {
	nodes := make([]triggerChainNode, 0, len(defs))
	incomingAliases := make(map[string]struct{}, len(defs))
	for idx := range defs {
		alias := strings.TrimSpace(defs[idx].Metadata.Alias)
		if alias == "" {
			return nil, fmt.Errorf("definition %d: metadata.alias is required", idx)
		}
		if _, ok := incomingAliases[alias]; ok {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateJob, alias)
		}
		incomingAliases[alias] = struct{}{}
		nodes = append(nodes, triggerChainNode{
			alias:         alias,
			triggerType:   strings.TrimSpace(defs[idx].Trigger.Type),
			configuration: cloneAnyMap(defs[idx].Trigger.Configuration),
		})
	}

	existing, err := existingTriggerChainNodes(ctx, conn, incomingAliases)
	if err != nil {
		return nil, err
	}
	nodes = append(nodes, existing...)
	return nodes, nil
}

func existingTriggerChainNodes(ctx context.Context, conn *gorm.DB, incomingAliases map[string]struct{}) ([]triggerChainNode, error) {
	if conn == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var rows []struct {
		ID            uuid.UUID
		Alias         string
		TriggerType   string
		Configuration string
	}
	err := conn.WithContext(ctx).
		Table("jobs").
		Select("jobs.id, jobs.alias, triggers.type AS trigger_type, triggers.configuration").
		Joins("JOIN triggers ON triggers.id = jobs.trigger_id AND triggers.deleted_at IS NULL").
		Where("jobs.deleted_at IS NULL").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	nodes := make([]triggerChainNode, 0, len(rows))
	for _, row := range rows {
		alias := strings.TrimSpace(row.Alias)
		if alias == "" {
			continue
		}
		if _, replaced := incomingAliases[alias]; replaced {
			continue
		}
		cfg := make(map[string]any)
		if strings.TrimSpace(row.Configuration) != "" {
			if err := json.Unmarshal([]byte(row.Configuration), &cfg); err != nil {
				return nil, fmt.Errorf("existing trigger %s configuration: %w", alias, err)
			}
		}
		nodes = append(nodes, triggerChainNode{
			id:            row.ID,
			alias:         alias,
			triggerType:   strings.TrimSpace(row.TriggerType),
			configuration: cfg,
		})
	}
	return nodes, nil
}

func triggerChainGraph(nodes []triggerChainNode) (map[string][]string, error) {
	graph := make(map[string][]string, len(nodes))
	knownAliases := make([]string, 0, len(nodes))
	knownSet := make(map[string]struct{}, len(nodes))
	aliasByJobID := make(map[string]string, len(nodes))
	for _, node := range nodes {
		if node.alias == "" {
			continue
		}
		if node.id != uuid.Nil {
			aliasByJobID[node.id.String()] = node.alias
		}
		if _, ok := graph[node.alias]; !ok {
			graph[node.alias] = nil
		}
		if _, ok := knownSet[node.alias]; !ok {
			knownSet[node.alias] = struct{}{}
			knownAliases = append(knownAliases, node.alias)
		}
	}
	sort.Strings(knownAliases)

	for _, node := range nodes {
		if node.triggerType != schema.TriggerEvent && node.triggerType != string(models.TriggerTypeEvent) {
			continue
		}
		patterns, err := triggerChainPatterns(node.configuration)
		if err != nil {
			return nil, fmt.Errorf("definition %s: %w", node.alias, err)
		}
		for _, pattern := range patterns {
			if !patternCanMatchCaesiumLifecycle(pattern) {
				continue
			}
			sourceAlias, scoped := triggerChainPatternSourceAlias(pattern, aliasByJobID)
			if scoped {
				addTriggerChainEdge(graph, sourceAlias, node.alias)
				continue
			}
			for _, alias := range knownAliases {
				addTriggerChainEdge(graph, alias, node.alias)
			}
		}
	}
	return graph, nil
}

func triggerChainPatternSourceAlias(pattern triggerChainPattern, aliasByJobID map[string]string) (string, bool) {
	sourceAlias := strings.TrimSpace(pattern.filter["job_alias"])
	sourceJobID := strings.TrimSpace(pattern.filter["job_id"])

	if sourceAlias == "" && sourceJobID == "" {
		return "", false
	}

	if sourceJobID == "" {
		return sourceAlias, true
	}

	resolvedAlias := aliasByJobID[sourceJobID]
	if sourceAlias == "" {
		return resolvedAlias, true
	}
	if resolvedAlias != "" && resolvedAlias != sourceAlias {
		return "", true
	}
	return sourceAlias, true
}

func triggerChainPatterns(cfg map[string]any) ([]triggerChainPattern, error) {
	rawEvents, ok := cfg["events"]
	if !ok || rawEvents == nil {
		return nil, nil
	}
	events, ok := rawEvents.([]any)
	if !ok {
		return nil, fmt.Errorf("trigger.configuration.events must be a list")
	}

	patterns := make([]triggerChainPattern, 0, len(events))
	for idx, rawEvent := range events {
		eventMap, ok := rawEvent.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("trigger.configuration.events[%d] must be an object", idx)
		}
		pattern := triggerChainPattern{filter: map[string]string{}}
		pattern.eventType, _ = eventMap["type"].(string)
		pattern.source, _ = eventMap["source"].(string)
		if rawFilter, ok := eventMap["filter"]; ok && rawFilter != nil {
			filter, ok := rawFilter.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("trigger.configuration.events[%d].filter must be an object", idx)
			}
			for key, value := range filter {
				if stringValue, ok := value.(string); ok {
					pattern.filter[key] = stringValue
				}
			}
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

func patternCanMatchCaesiumLifecycle(pattern triggerChainPattern) bool {
	source := strings.TrimSpace(pattern.source)
	if source != "" && source != "caesium" {
		return false
	}
	for _, lifecycleType := range []event.Type{event.TypeRunCompleted, event.TypeRunFailed, event.TypeRunTerminal} {
		if matchesTriggerChainEventType(pattern.eventType, string(lifecycleType)) {
			return true
		}
	}
	return false
}

func matchesTriggerChainEventType(patternValue, eventType string) bool {
	patternValue = strings.TrimSpace(patternValue)
	eventType = strings.TrimSpace(eventType)
	if patternValue == "" || eventType == "" {
		return false
	}
	if !strings.ContainsAny(patternValue, "*?[") {
		return patternValue == eventType
	}
	matched, err := path.Match(patternValue, eventType)
	return err == nil && matched
}

func addTriggerChainEdge(graph map[string][]string, from, to string) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return
	}
	if _, ok := graph[from]; !ok {
		graph[from] = nil
	}
	for _, existing := range graph[from] {
		if existing == to {
			return
		}
	}
	graph[from] = append(graph[from], to)
}

func rejectTriggerChainCycle(graph map[string][]string) error {
	for node := range graph {
		sort.Strings(graph[node])
	}

	nodes := make([]string, 0, len(graph))
	for node := range graph {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	state := make(map[string]int, len(graph))
	stack := make([]string, 0, len(graph))
	var visit func(string) []string
	visit = func(node string) []string {
		state[node] = 1
		stack = append(stack, node)
		for _, next := range graph[node] {
			switch state[next] {
			case 0:
				if cycle := visit(next); len(cycle) > 0 {
					return cycle
				}
			case 1:
				return triggerChainCyclePath(stack, next)
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = 2
		return nil
	}

	for _, node := range nodes {
		if state[node] != 0 {
			continue
		}
		if cycle := visit(node); len(cycle) > 0 {
			return fmt.Errorf("%w: %s", ErrTriggerChainCycle, strings.Join(cycle, " -> "))
		}
	}
	return nil
}

func triggerChainCyclePath(stack []string, repeated string) []string {
	start := 0
	for idx, node := range stack {
		if node == repeated {
			start = idx
			break
		}
	}
	cycle := append([]string{}, stack[start:]...)
	cycle = append(cycle, repeated)
	return cycle
}
