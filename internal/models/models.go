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
	&JobRun{},
	&TaskRun{},
	&CallbackRun{},
}
