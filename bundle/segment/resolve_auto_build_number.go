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

	if !isAuto(b.Config.Variables["build_branch"]) && !isAuto(b.Config.Variables["build_number"]) && !isAuto(b.Config.Variables["build_sha"]) {
		return nil
	}

	var resolvedBranch = b.Config.Variables["build_branch"].Value
	if isAuto(b.Config.Variables["build_branch"]) {
		resolvedBranch = b.Config.Bundle.Git.ActualBranch
		fmt.Printf("Build branch set to 'auto', using: %s\n", resolvedBranch)
	}

	var resolvedBuildNumber = b.Config.Variables["build_number"].Value
	if isAuto(b.Config.Variables["build_number"]) {
		var err error
		resolvedBuildNumber, err = m.getLatestBuild(resolvedBranch.(string))
		if err != nil {
			return diag.FromErr(err)
		}
		fmt.Printf("Build number set to 'auto', using: %s\n", resolvedBuildNumber)
	}

	var resolvedGitSha = b.Config.Variables["build_sha"].Value
	if isAuto(b.Config.Variables["build_sha"]) {
		var err error
		resolvedGitSha, err = m.getBuildSha(resolvedBranch.(string), resolvedBuildNumber.(string), "profiles-"+b.Config.Bundle.Name)
		if err != nil {
			return diag.FromErr(err)
		}
		fmt.Printf("Build SHA set to 'auto', using: %s\n", resolvedGitSha)
	}

	err := b.Config.Mutate(func(v dyn.Value) (dyn.Value, error) {
		return dyn.Map(v, "variables", dyn.Foreach(func(p dyn.Path, variable dyn.Value) (dyn.Value, error) {
			name := p[1].Key()
			v, ok := b.Config.Variables[name]
			if !ok {
				return dyn.InvalidValue, fmt.Errorf(`variable "%s" is not defined`, name)
			}

			if name == "build_branch" && isAuto(v) {
				return dyn.Set(variable, "value", dyn.V(resolvedBranch))
			}

			if name == "build_number" && isAuto(v) {
				return dyn.Set(variable, "value", dyn.V(resolvedBuildNumber))
			}

			if name == "build_sha" && isAuto(v) {
				return dyn.Set(variable, "value", dyn.V(resolvedGitSha))
			}

			return variable, nil

		}))
	})

	fmt.Printf("Build tag set to 'auto', using branch (%s) and build (%s)\n", resolvedBranch, resolvedBuildNumber)

	return diag.FromErr(err)
}

func isAuto(v *variable.Variable) bool {
	if v.HasValue() {
		return v.Value == "auto"
	}

	if v.HasDefault() {
		return v.Default == "auto"
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
