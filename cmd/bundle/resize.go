package bundle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/databricks/cli/bundle"
	"github.com/databricks/cli/bundle/config/mutator"
	"github.com/databricks/cli/bundle/deploy/terraform"
	"github.com/databricks/cli/bundle/phases"
	"github.com/databricks/cli/cmd/bundle/utils"
	"github.com/databricks/cli/libs/cmdio"
	"github.com/databricks/databricks-sdk-go/service/compute"
	"github.com/databricks/databricks-sdk-go/service/jobs"
	"github.com/spf13/cobra"
)

// Slice with functions to override default command behavior.
// Functions can be added from the `init()` function in manually curated files in this directory.
var resizeOverrides []func(
	*cobra.Command,
	*compute.ResizeCluster,
)

func newResize() *cobra.Command {
	cmd := &cobra.Command{}

	var resizeSkipWait bool
	var resizeJobKey string
	var resizeTaskKey string
	var resizeNumWorkers int
	var resizeTimeout time.Duration

	cmd.Flags().BoolVar(&resizeSkipWait, "no-wait", resizeSkipWait, `do not wait to reach RUNNING state`)
	cmd.Flags().DurationVar(&resizeTimeout, "timeout", 20*time.Minute, `maximum amount of time to reach RUNNING state`)
	cmd.Flags().StringVar(&resizeJobKey, "job-key", resizeJobKey, `job key to resize, as defined in the YAML`)
	cmd.Flags().StringVar(&resizeTaskKey, "task-key", resizeTaskKey, `job task key to resize, as defined in the YAML`)
	cmd.Flags().IntVar(&resizeNumWorkers, "num-workers", resizeNumWorkers, `number of target workers for the cluster`)

	cmd.Use = "resize [JOB_NAME]"
	cmd.Short = `Resize job cluster.`
	cmd.Long = `Resize job cluster.
  
  Resizes a cluster to have a desired number of workers. This will fail unless
  the cluster is in a RUNNING state.

  Arguments:
    JOB_NAME: The job to be resized.`

	cmd.Annotations = make(map[string]string)

	var forcePull bool
	cmd.Flags().BoolVar(&forcePull, "force-pull", false, "Skip local cache and load the state from the remote workspace")

	cmd.RunE = func(cmd *cobra.Command, args []string) (err error) {
		ctx := cmd.Context()
		b, diags := utils.ConfigureBundleWithVariables(cmd)
		if err := diags.Error(); err != nil {
			return diags.Error()
		}

		diags = bundle.Apply(ctx, b, phases.Initialize())
		if err := diags.Error(); err != nil {
			return err
		}

		cacheDir, err := terraform.Dir(ctx, b)
		if err != nil {
			return err
		}
		_, stateFileErr := os.Stat(filepath.Join(cacheDir, terraform.TerraformStateFileName))
		_, configFileErr := os.Stat(filepath.Join(cacheDir, terraform.TerraformConfigFileName))
		noCache := errors.Is(stateFileErr, os.ErrNotExist) || errors.Is(configFileErr, os.ErrNotExist)

		if forcePull || noCache {
			diags = bundle.Apply(ctx, b, bundle.Seq(
				terraform.StatePull(),
				terraform.Interpolate(),
				terraform.Write(),
			))
			if err := diags.Error(); err != nil {
				return err
			}
		}

		diags = bundle.Apply(ctx, b,
			bundle.Seq(terraform.Load(), mutator.InitializeURLs()))
		if err := diags.Error(); err != nil {
			return err
		}

		targetJobId, err := collectTargetJobId(ctx, b, resizeJobKey)
		if err != nil {
			return err
		}

		targetClusterId, currentSize, err := collectTargetClusterId(ctx, b, targetJobId, resizeTaskKey)
		if err != nil {
			return err
		}

		targetClusterSize, err := collectTargetClusterSize(ctx, currentSize, resizeNumWorkers)

		wait, err := b.WorkspaceClient().Clusters.Resize(ctx, compute.ResizeCluster{
			ClusterId:  targetClusterId,
			NumWorkers: targetClusterSize,
		})
		if err != nil {
			return fmt.Errorf("unable to resize cluster: %w", err)
		}
		if resizeSkipWait {
			return nil
		}
		spinner := cmdio.Spinner(ctx)
		info, err := wait.OnProgress(func(i *compute.ClusterDetails) {
			statusMessage := i.StateMessage
			spinner <- statusMessage
		}).GetWithTimeout(resizeTimeout)
		close(spinner)
		if err != nil {
			return err
		}
		return cmdio.Render(ctx, info)
	}

	// Disable completions since they are not applicable.
	// Can be overridden by manual implementation in `override.go`.
	cmd.ValidArgsFunction = cobra.NoFileCompletions

	return cmd
}

func collectTargetJobId(ctx context.Context, b *bundle.Bundle, resizeJobKey string) (int64, error) {
	var jobsByKey = make(map[string]int64)
	var jobsKeysByName = make(map[string]string)

	for job := range b.Config.Resources.AllResources()[0].Resources {
		var jobFull = b.Config.Resources.Jobs[job]

		jobId, err := strconv.ParseInt(jobFull.ID, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("unable to parse job ID: %w", err)
		}
		jobsKeysByName[jobFull.Name] = job
		jobsByKey[job] = jobId
	}

	var targetJobId int64
	if resizeJobKey == "" {
		targetJobKey, err := cmdio.Select(ctx, jobsKeysByName, "The job to be resized")
		if err != nil {
			return 0, err
		}

		targetJobId = jobsByKey[targetJobKey]

		fmt.Printf("Targeting job %s (%d)\n", targetJobKey, targetJobId)
	} else {
		if value, ok := jobsByKey[resizeJobKey]; ok {
			targetJobId = value
		} else {
			return 0, fmt.Errorf("job key %s not found", resizeJobKey)
		}
	}

	return targetJobId, nil
}

func collectTargetClusterId(ctx context.Context, b *bundle.Bundle, targetJobId int64, resizeTaskKey string) (string, int, error) {
	runs := b.WorkspaceClient().Jobs.ListRuns(ctx, jobs.ListRunsRequest{
		JobId:       targetJobId,
		ActiveOnly:  true,
		ExpandTasks: true,
	})

	var taskClusters = make(map[string]string)
	var clusterSizes = make(map[string]int)

	for runs.HasNext(ctx) {
		run, err := runs.Next(ctx)
		if err != nil {
			return "", 0, fmt.Errorf("unable to fetch job run %d: %w", targetJobId, err)
		}

		for _, task := range run.Tasks {
			if task.Status.State == jobs.RunLifecycleStateV2StateRunning && task.ClusterInstance != nil {
				taskClusters[task.TaskKey] = task.ClusterInstance.ClusterId
				clusterSizes[task.ClusterInstance.ClusterId] = task.NewCluster.NumWorkers
			}
		}
	}

	if len(taskClusters) == 0 {
		return "", 0, fmt.Errorf("job has no active runs")
	}

	var targetClusterId string
	if resizeTaskKey == "" {
		if len(taskClusters) > 1 {
			clusterId, err := cmdio.Select(ctx, taskClusters, "The task to be resized")
			if err != nil {
				return "", 0, err
			}
			targetClusterId = clusterId

			for k, v := range taskClusters {
				if v == targetClusterId {
					fmt.Printf("Targeting cluster %s (%s)\n", k, v)
				}
			}
		} else {
			for k, v := range taskClusters {
				targetClusterId = v

				fmt.Printf("Targeting cluster %s (%s)\n", k, v)
			}
		}

	} else {
		if value, ok := taskClusters[resizeTaskKey]; ok {
			targetClusterId = value
		} else {
			return "", 0, fmt.Errorf("task key %s not found", resizeTaskKey)
		}
	}

	var currentSize = clusterSizes[targetClusterId]

	return targetClusterId, currentSize, nil
}

func collectTargetClusterSize(ctx context.Context, currentSize int, resizeNumWorkers int) (int, error) {
	targetClusterSize := resizeNumWorkers
	if resizeNumWorkers == 0 {
		targetClusterSizeStr, err := cmdio.Ask(ctx, fmt.Sprintf("Current cluster size: %d. Enter new cluster size", currentSize), strconv.Itoa(currentSize))
		if err != nil {
			return 0, err
		}

		targetClusterSize, err = strconv.Atoi(targetClusterSizeStr)
		if err != nil {
			return 0, fmt.Errorf("unable to parse cluster size: %w", err)
		}
	}

	if targetClusterSize == currentSize {
		return 0, fmt.Errorf("target size matches current size. nothing to do")
	}

	return targetClusterSize, nil
}
