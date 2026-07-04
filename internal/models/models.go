package models

// All lists every model for AutoMigrate. Order matters: parent tables
// must appear before children so that foreign-key constraints can
// reference them.
var All = []interface{}{
	&Atom{},
	&Trigger{},
	&Job{},
	&RunQueue{},
	&IngestedEvent{},
	&WebhookEvent{},
	&Task{},
	&TaskEdge{},
	&Callback{},
	&Backfill{},
	&JobRun{},
	&TaskRun{},
	&LineageDataset{},
	&TaskCache{},
	&CallbackRun{},
	&ExecutionEvent{},
	&EventTriggerMatch{},
	&APIKey{},
	&AuditLog{},
	&User{},
	&Session{},
	&SAMLAssertionReplay{},
	&NotificationChannel{},
	&NotificationPolicy{},
	&RateLimitToken{},
	// Phase 2 run-owner coordination tables (catalog DB, cross-run, low-volume).
	&RunLease{},
	&InternalCAGeneration{},
	&InternalNodeEnrollment{},
	// run_checkpoints is per-run and lives with task_runs (catalog when
	// unsharded, hot shard when sharded — see hotPathModels), so it is listed
	// here for the unsharded case and in hotPathModels for the sharded case.
	&RunCheckpoint{},
	// dag_snapshots is append-only topology history (data-plane-memory B1).
	// Must follow Job (FK parent) so AutoMigrate creates the parent table first.
	&DagSnapshot{},
	// dataset_declarations is the declared dataset graph (freshness A2). Must
	// follow Job (FK parent). Rebuilt from the manifest on every apply; not a
	// hot per-run table.
	&DatasetDeclaration{},
	// dataset_states / dataset_derivations are the freshness state substrate
	// (freshness B1). DatasetState carries a soft last_run_id reference so it
	// follows Job/JobRun; DatasetDerivation follows it. Neither is a hot per-run
	// table (written at run completion + by the evaluator, not on the hot path),
	// so both are deliberately absent from hotPathModels()/hotTables.
	&DatasetState{},
	&DatasetDerivation{},
	// Agent-in-the-loop remediation (Phase 0) incident substrate. These are
	// append-mostly, low-volume catalog tables — NOT hot per-run tables, so they
	// are deliberately absent from hotPathModels()/hotTables. Parents precede
	// children so AutoMigrate can build FK constraints: Incident and AgentProfile
	// first, then AgentSession (FK incident+profile), AgentAction (FK
	// incident+session), ApprovalRequest (FK action), RemediationTimer (FK incident).
	&Incident{},
	&AgentProfile{},
	&AgentSession{},
	&AgentAction{},
	&ApprovalRequest{},
	&RemediationTimer{},
}
