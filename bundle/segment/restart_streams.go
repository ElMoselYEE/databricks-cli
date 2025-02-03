package segment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/databricks/cli/bundle"
	"github.com/databricks/cli/libs/diag"
	"github.com/databricks/cli/libs/log"
	"github.com/databricks/databricks-sdk-go/service/jobs"
)

type restartStreams struct {
}

func RestartStreams() *restartStreams {
	return &restartStreams{}
}

func (m *restartStreams) Name() string {
	return "RestartStreams"
}

func (m *restartStreams) Apply(ctx context.Context, b *bundle.Bundle) diag.Diagnostics {

	tf := b.Terraform
	if tf == nil {
		return diag.FromErr(errors.New("terraform not initialized"))
	}

	// read plan file
	plan, err := tf.ShowPlanFile(ctx, b.Plan.Path)
	if err != nil {
		return diag.FromErr(err)
	}

	for _, change := range plan.ResourceChanges {
		if !strings.Contains(change.Name, "stream") {
			continue
		}

		if !change.Change.Actions.Update() {
			continue
		}

		jobIdString, ok := change.Change.After.(map[string]any)["id"]
		if !ok {
			log.Warn(ctx, "unexpected payload during parsing, not restarting job")
			continue
		}

		jobId, err := strconv.ParseInt(jobIdString.(string), 10, 64)
		if err != nil {
			return diag.Errorf("failed to parse job_id while restarting jobs: %s", jobIdString)
		}

		err = b.WorkspaceClient().Jobs.CancelAllRuns(ctx, jobs.CancelAllRuns{JobId: jobId})
		if err != nil {
			return diag.Errorf("failed to CancelAllRuns while after deployment: %s", jobIdString)
		}

		jobName, ok := change.Change.After.(map[string]any)["name"]
		if ok {
			fmt.Println("Restarted job: ", jobName)
			continue
		}
	}

	return nil
}
