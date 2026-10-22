package run

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestDiff_KubernetesMappingChangesAreNamed: why / run diff attribute a miss
// caused by a namespace remap or a changed default service account to the
// mapping fields themselves.
func TestDiff_KubernetesMappingChangesAreNamed(t *testing.T) {
	base := cache.HashInput{TaskName: "load", Image: "busybox:1.36.1", KubernetesTarget: &atom.KubeTarget{Namespace: "caesium-marketing", ServiceAccountName: "jobs"}}
	remapped := base
	remapped.KubernetesTarget = &atom.KubeTarget{Namespace: "caesium-marketing-v2", ServiceAccountName: "jobs-v2"}
	d, err := DiffHashInputBlobs(blobFor(t, remapped), blobFor(t, base)) // (subject, baseline)
	require.NoError(t, err)
	require.False(t, d.HashEqual)
	ns, ok := findChange(d.Changes, "kubernetesTarget.namespace")
	require.True(t, ok, "%+v", d.Changes)
	require.Equal(t, "caesium-marketing", ns.Before)
	require.Equal(t, "caesium-marketing-v2", ns.After)
	sa, ok := findChange(d.Changes, "kubernetesTarget.serviceAccountName")
	require.True(t, ok)
	require.Equal(t, "jobs-v2", sa.After)

	unmapped := base
	unmapped.KubernetesTarget = nil
	d, err = DiffHashInputBlobs(blobFor(t, base), blobFor(t, unmapped))
	require.NoError(t, err)
	ns, ok = findChange(d.Changes, "kubernetesTarget.namespace")
	require.True(t, ok)
	require.Equal(t, "", ns.Before)

	docker := cache.HashInput{TaskName: "load", Image: "busybox:1.36.1", KubernetesSecretNamespace: "caesium-marketing"}
	dockerRemapped := docker
	dockerRemapped.KubernetesSecretNamespace = "caesium-marketing-v2"
	d, err = DiffHashInputBlobs(blobFor(t, dockerRemapped), blobFor(t, docker))
	require.NoError(t, err)
	_, ok = findChange(d.Changes, "kubernetesSecretNamespace")
	require.True(t, ok)

	d, err = DiffHashInputBlobs(blobFor(t, base), blobFor(t, base))
	require.NoError(t, err)
	require.True(t, d.HashEqual)
	require.Empty(t, d.Changes)
}

func seedFailedWhyTask(t *testing.T, row models.TaskRun) (*Store, uuid.UUID) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	jobID, taskID, runID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.JobRun{ID: runID, JobID: jobID, Status: string(StatusFailed), TriggerType: "manual", StartedAt: now}).Error)
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "use"}).Error)
	row.ID, row.JobRunID, row.TaskID, row.AtomID = uuid.New(), runID, taskID, uuid.New()
	row.Engine, row.Image, row.Command = models.AtomEngineDocker, "busybox:1.36.1", `["true"]`
	row.StartedAt, row.CreatedAt, row.UpdatedAt = &now, now, now
	require.NoError(t, db.Create(&row).Error)
	return NewStore(db), runID
}

// TestWhyTask_FailureClass: a denied secret reads as the config class; an
// unclassifiable failure and a success keep the explanation without the field,
// byte-for-byte the JSON shape they had before.
func TestWhyTask_FailureClass(t *testing.T) {
	ctx := context.Background()

	denied := (&secret.SecretDeniedError{Namespace: "marketing", Ref: "env/FINANCE_IT_TOKEN/…", Reason: "no rule matches"}).Error()
	store, runID := seedFailedWhyTask(t, models.TaskRun{Status: string(TaskStatusFailed), Error: "env TOKEN: " + denied})
	exp, err := store.WhyTask(ctx, runID, "use")
	require.NoError(t, err)
	require.Equal(t, "config", exp.FailureClass)
	body, err := json.Marshal(exp)
	require.NoError(t, err)
	require.Contains(t, string(body), `"failureClass":"config"`)

	store, runID = seedFailedWhyTask(t, models.TaskRun{Status: string(TaskStatusFailed), Error: "exit status 3"})
	exp, err = store.WhyTask(ctx, runID, "use")
	require.NoError(t, err)
	require.Empty(t, exp.FailureClass, "the unknown class is omitted")
	body, err = json.Marshal(exp)
	require.NoError(t, err)
	require.NotContains(t, string(body), "failureClass", "an unclassified failure keeps today's JSON")

	store, runID = seedFailedWhyTask(t, models.TaskRun{Status: string(TaskStatusSucceeded)})
	exp, err = store.WhyTask(ctx, runID, "use")
	require.NoError(t, err)
	body, err = json.Marshal(exp)
	require.NoError(t, err)
	require.NotContains(t, string(body), "failureClass")

	require.Empty(t, whyFailureClass(nil))
	require.Equal(t, "", whyFailureClass(&models.TaskRun{Status: string(TaskStatusSucceeded), Error: denied}))
}

func TestRecordTaskKubernetesTarget(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskRunID := uuid.New(), uuid.New()
	original := []byte(`{"schemaVersion":1,"runtime":{"engine":"kubernetes","image":"busybox:1.36.1","retryCount":0,"retryDelay":0,"retryBackoff":false}}`)
	require.NoError(t, db.Create(&models.TaskRun{
		ID: taskRunID, JobRunID: runID, TaskID: uuid.New(), AtomID: uuid.New(), Engine: models.AtomEngineKubernetes,
		Image: "busybox:1.36.1", Status: string(TaskStatusPending), ExecutionDescriptor: datatypes.JSON(original),
	}).Error)
	read := func() []byte {
		var row models.TaskRun
		require.NoError(t, db.Select("execution_descriptor").First(&row, "id = ?", taskRunID).Error)
		return row.ExecutionDescriptor
	}

	// Zero with nothing recorded: no write (known and unknown current alike).
	require.NoError(t, store.RecordTaskKubernetesTarget(runID, taskRunID, atom.KubeTarget{}, []byte{}))
	require.NoError(t, store.RecordTaskKubernetesTarget(runID, taskRunID, atom.KubeTarget{}, nil))
	require.Equal(t, string(original), string(read()), "an unmapped descriptor is byte-identical")

	target := atom.KubeTarget{Namespace: "caesium-marketing", ServiceAccountName: "marketing-jobs"}
	require.NoError(t, store.RecordTaskKubernetesTarget(runID, taskRunID, target, read()))
	desc, err := store.TaskExecutionDescriptor(context.Background(), runID, taskRunID)
	require.NoError(t, err)
	require.Equal(t, &target, desc.Runtime.KubernetesTarget)

	// A later execution that resolved no mapping clears it.
	require.NoError(t, store.RecordTaskKubernetesTarget(runID, taskRunID, atom.KubeTarget{}, nil))
	require.False(t, strings.Contains(string(read()), "kubernetesTarget"))
	require.NoError(t, store.RecordTaskKubernetesTarget(runID, uuid.New(), atom.KubeTarget{}, nil), "an unknown row is not an error")
}
