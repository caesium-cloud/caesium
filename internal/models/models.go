package models

// All lists every model for AutoMigrate. Order matters: parent tables
// must appear before children so that foreign-key constraints can
// reference them.
var All = []interface{}{
	&Atom{},
	&Trigger{},
	&Job{},
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
	&APIKey{},
	&AuditLog{},
	&User{},
	&Session{},
	&SAMLAssertionReplay{},
	&NotificationChannel{},
	&NotificationPolicy{},
	// Phase 2 run-owner coordination tables (catalog DB, cross-run, low-volume).
	&RunLease{},
	&InternalCAGeneration{},
	&InternalNodeEnrollment{},
	// run_checkpoints is per-run and lives with task_runs (catalog when
	// unsharded, hot shard when sharded — see hotPathModels), so it is listed
	// here for the unsharded case and in hotPathModels for the sharded case.
	&RunCheckpoint{},
}
