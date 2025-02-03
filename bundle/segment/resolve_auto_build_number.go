package segment

import (
	"context"
	"fmt"
	"github.com/databricks/cli/bundle/config/variable"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/databricks/cli/bundle"
	"github.com/databricks/cli/libs/diag"
	"github.com/databricks/cli/libs/dyn"
)

var (
	artifactsBucket = "segment-pdl-artifacts"
	gitShaRegexp    = regexp.MustCompile(`^.*?-([a-z0-9]+)\.jar$`)
)

const artifactSourceS3 = "s3"
const artifactSourceLocal = "local"

type resolveAutoBuildNumber struct {
	s3Client *s3.S3
}

func ResolveAutoBuildNumber() *resolveAutoBuildNumber {
	sess := session.Must(session.NewSession(aws.NewConfig().WithRegion("us-west-2")))
	s3Client := s3.New(sess)
	return &resolveAutoBuildNumber{
		s3Client,
	}
}

func (m *resolveAutoBuildNumber) Name() string {
	return "ResolveAutoBuildNumber"
}

func (m *resolveAutoBuildNumber) Apply(ctx context.Context, b *bundle.Bundle) diag.Diagnostics {

	if !isEqual(b.Config.Variables["build_branch"], "auto") && !isEqual(b.Config.Variables["build_number"], "auto") && !isEqual(b.Config.Variables["build_sha"], "auto") {
		return nil
	}

	var resolvedBranch = b.Config.Variables["build_branch"].Value
	if isEqual(b.Config.Variables["build_branch"], "auto") {
		resolvedBranch = b.Config.Bundle.Git.ActualBranch
		fmt.Printf("Build branch set to 'auto', using: %s\n", resolvedBranch)
	}

	var resolvedBuildNumber = b.Config.Variables["build_number"].Value
	if isEqual(b.Config.Variables["build_number"], "auto") {
		if isEqual(b.Config.Variables["artifact_source"], artifactSourceS3) {
			var err error
			resolvedBuildNumber, err = m.getLatestBuild(resolvedBranch.(string))
			if err != nil {
				return diag.FromErr(err)
			}
			fmt.Printf("Build number set to 'auto', using: %s\n", resolvedBuildNumber)
		} else if isEqual(b.Config.Variables["artifact_source"], artifactSourceLocal) {
			resolvedBuildNumber = "local"
		}
	}

	var resolvedGitSha = b.Config.Variables["build_sha"].Value
	if isEqual(b.Config.Variables["build_sha"], "auto") && isEqual(b.Config.Variables["artifact_source"], "s3") {
		var err error
		resolvedGitSha, err = m.getBuildSha(resolvedBranch.(string), resolvedBuildNumber.(string), "profiles-"+b.Config.Bundle.Name)
		if err != nil {
			return diag.FromErr(err)
		}
		fmt.Printf("Build SHA set to 'auto', using: %s\n", resolvedGitSha)
	}

	var resolvedArtifactPath = b.Config.Variables["artifact_path"].Value
	if isEqual(b.Config.Variables["artifact_path"], "auto") {
		if isEqual(b.Config.Variables["artifact_source"], artifactSourceS3) {
			resolvedArtifactPath = fmt.Sprintf(
				"s3://%s/profiles-data-lake-spark/${var.build_branch}/${var.build_number}/%s/build/libs/profiles-%s-0.3.0-%s.jar",
				artifactsBucket, b.Config.Bundle.Name, b.Config.Bundle.Name, resolvedGitSha,
			)
		} else if isEqual(b.Config.Variables["artifact_source"], artifactSourceLocal) {
			resolvedArtifactPath = fmt.Sprintf("%s/files/build/libs/%s-0.3.0.jar", b.Config.Workspace.RootPath, b.Config.Bundle.Name)
		} else {
			return diag.Errorf(
				"unsupported artifact source: %s, allowed values: [%s, %s]",
				b.Config.Variables["artifact_source"].Value, artifactSourceS3, artifactSourceLocal,
			)
		}

		fmt.Printf("Artifact path set to 'auto', using: %s\n", resolvedArtifactPath)
	}

	err := b.Config.Mutate(func(v dyn.Value) (dyn.Value, error) {
		return dyn.Map(v, "variables", dyn.Foreach(func(p dyn.Path, variable dyn.Value) (dyn.Value, error) {
			name := p[1].Key()
			v, ok := b.Config.Variables[name]
			if !ok {
				return dyn.InvalidValue, fmt.Errorf(`variable "%s" is not defined`, name)
			}

			if name == "build_branch" && isEqual(v, "auto") {
				return dyn.Set(variable, "value", dyn.V(resolvedBranch))
			}

			if name == "build_number" && isEqual(v, "auto") {
				return dyn.Set(variable, "value", dyn.V(resolvedBuildNumber))
			}

			if name == "build_sha" && isEqual(v, "auto") {
				return dyn.Set(variable, "value", dyn.V(resolvedGitSha))
			}

			if name == "artifact_path" && isEqual(v, "auto") {
				return dyn.Set(variable, "value", dyn.V(resolvedArtifactPath))
			}

			return variable, nil

		}))
	})

	return diag.FromErr(err)
}

func isEqual(v *variable.Variable, value string) bool {
	if v.HasValue() {
		return v.Value == value
	}

	if v.HasDefault() {
		return v.Default == value
	}

	return false
}

func (m *resolveAutoBuildNumber) getLatestBuild(branch string) (string, error) {
	prefix := fmt.Sprintf("profiles-data-lake-spark/%s/", branch)
	delimiter := "/"

	maxBuild := -1

	err := m.s3Client.ListObjectsV2Pages(
		&s3.ListObjectsV2Input{
			Bucket:    aws.String(artifactsBucket),
			Prefix:    aws.String(prefix),
			Delimiter: aws.String(delimiter),
		},
		func(page *s3.ListObjectsV2Output, lastPage bool) bool {
			for _, pfx := range page.CommonPrefixes {
				buildNum, err := strconv.Atoi(path.Base(strings.TrimSuffix(*pfx.Prefix, "/")))
				if err != nil {
					continue
				}

				if buildNum > maxBuild {
					maxBuild = buildNum
				}
			}

			return !lastPage
		},
	)

	return strconv.Itoa(maxBuild), err
}

func (m *resolveAutoBuildNumber) getBuildSha(branch string, buildNumber string, module string) (string, error) {
	var err error

	prefix := fmt.Sprintf("profiles-data-lake-spark/%s/%s/", branch, buildNumber)

	latestModified := time.UnixMilli(0)
	latestArtifact := ""

	err = m.s3Client.ListObjectsV2Pages(
		&s3.ListObjectsV2Input{
			Bucket: aws.String(artifactsBucket),
			Prefix: aws.String(prefix),
		},
		func(page *s3.ListObjectsV2Output, lastPage bool) bool {
			for _, obj := range page.Contents {
				key := *obj.Key
				filename := path.Base(key)

				if path.Ext(filename) == ".jar" && strings.HasPrefix(filename, module) && obj.LastModified.After(latestModified) {
					latestArtifact = key
				}
			}

			return !lastPage
		},
	)

	if err != nil {
		return "", err
	}

	if latestArtifact == "" {
		return "", fmt.Errorf("no S3 artifact found for build %s-%s; build may not have completed yet", buildNumber, branch)
	}

	_, err = m.s3Client.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(artifactsBucket),
		Key:    aws.String(latestArtifact),
	})
	if err != nil {
		return "", fmt.Errorf(
			"artifact s3://%s/%s does not exist; build may not have completed yet: %w",
			artifactsBucket, latestArtifact, err)
	}

	gitSha := ""
	groups := gitShaRegexp.FindStringSubmatch(latestArtifact)
	if len(groups) == 2 {
		gitSha = groups[1]
	}

	return gitSha, nil
}
