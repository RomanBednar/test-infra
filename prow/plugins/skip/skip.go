/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package skip implements the `/skip` command which allows users
// to clean up commit statuses of non-blocking presubmits on PRs.
package skip

import (
	"fmt"
	"regexp"

	"github.com/sirupsen/logrus"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/git/v2"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/plugins/trigger"
)

const pluginName = "skip"

var (
	skipRe = regexp.MustCompile(`(?mi)^/skip\s*$`)
)

type githubClient interface {
	CreateComment(owner, repo string, number int, comment string) error
	CreateStatus(org, repo, ref string, s github.Status) error
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	GetRef(org, repo, ref string) (string, error)
}

func init() {
	plugins.RegisterGenericCommentHandler(pluginName, handleGenericComment, helpProvider)
}

func helpProvider(config *plugins.Configuration, _ []config.OrgRepo) (*pluginhelp.PluginHelp, error) {
	pluginHelp := &pluginhelp.PluginHelp{
		Description: "The skip plugin allows users to clean up GitHub stale commit statuses for non-blocking jobs on a PR.",
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/skip",
		Description: "Cleans up GitHub stale commit statuses for non-blocking jobs on a PR.",
		Featured:    false,
		WhoCanUse:   "Anyone can trigger this command on a PR.",
		Examples:    []string{"/skip"},
	})
	return pluginHelp, nil
}

func handleGenericComment(pc plugins.Agent, e github.GenericCommentEvent) error {
	honorOkToTest := trigger.HonorOkToTest(pc.PluginConfig.TriggerFor(e.Repo.Owner.Login, e.Repo.Name))
	return handle(pc.GitHubClient, pc.Logger, &e, pc.Config, pc.GitClient, honorOkToTest)
}

func handle(gc githubClient, log *logrus.Entry, e *github.GenericCommentEvent, c *config.Config, gitClient git.ClientFactory, honorOkToTest bool) error {
	if !e.IsPR || e.IssueState != "open" || e.Action != github.GenericCommentActionCreated {
		return nil
	}

	if !skipRe.MatchString(e.Body) {
		return nil
	}

	org := e.Repo.Owner.Login
	repo := e.Repo.Name
	number := e.Number

	pr, err := gc.GetPullRequest(org, repo, number)
	if err != nil {
		resp := fmt.Sprintf("Cannot get PR #%d in %s/%s: %v", number, org, repo, err)
		log.Warn(resp)
		return gc.CreateComment(org, repo, number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, e.User.Login, resp))
	}
	baseSHAGetter := func() (string, error) {
		baseSHA, err := gc.GetRef(org, repo, "heads/"+pr.Base.Ref)
		if err != nil {
			return "", fmt.Errorf("failed to get baseSHA: %w", err)
		}
		return baseSHA, nil
	}
	headSHAGetter := func() (string, error) {
		return pr.Head.SHA, nil
	}
	presubmits, err := c.GetPresubmits(gitClient, org+"/"+repo, "", baseSHAGetter, headSHAGetter)
	if err != nil {
		return fmt.Errorf("failed to get presubmits: %w", err)
	}

	combinedStatus, err := gc.GetCombinedStatus(org, repo, pr.Head.SHA)
	if err != nil {
		resp := fmt.Sprintf("Cannot get combined commit statuses for PR #%d in %s/%s: %v", number, org, repo, err)
		log.Warn(resp)
		return gc.CreateComment(org, repo, number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, e.User.Login, resp))
	}
	if combinedStatus.State == github.StatusSuccess {
		return nil
	}
	statuses := combinedStatus.Statuses

	filteredPresubmits, err := trigger.FilterPresubmits(honorOkToTest, gc, e.Body, pr, presubmits, log)
	if err != nil {
		resp := fmt.Sprintf("Cannot get combined status for PR #%d in %s/%s: %v", number, org, repo, err)
		log.Warn(resp)
		return gc.CreateComment(org, repo, number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, e.User.Login, resp))
	}
	triggerWillHandle := func(p config.Presubmit) bool {
		for _, presubmit := range filteredPresubmits {
			if p.Name == presubmit.Name && p.Context == presubmit.Context {
				return true
			}
		}
		return false
	}

	for _, job := range presubmits {
		// Only consider jobs that have already posted a failed status
		if !statusExists(job, statuses) || isSuccess(job, statuses) {
			continue
		}
		// Ignore jobs that will be handled by the trigger plugin
		// for this specific comment, regardless of whether they
		// are required or not. This allows a comment like
		// >/skip
		// >/test foo
		// To end up testing foo instead of skipping it
		if triggerWillHandle(job) {
			continue
		}
		// Only skip jobs that are not required
		if job.ContextRequired() {
			continue
		}
		context := job.Context
		status := github.Status{
			State:       github.StatusSuccess,
			Description: "Skipped",
			Context:     context,
		}
		if err := gc.CreateStatus(org, repo, pr.Head.SHA, status); err != nil {
			resp := fmt.Sprintf("Cannot update PR status for context %s: %v", context, err)
			log.Warn(resp)
			return gc.CreateComment(org, repo, number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, e.User.Login, resp))
		}
	}
	return nil
}

func statusExists(job config.Presubmit, statuses []github.Status) bool {
	for _, status := range statuses {
		if status.Context == job.Context {
			return true
		}
	}
	return false
}

func isSuccess(job config.Presubmit, statuses []github.Status) bool {
	for _, status := range statuses {
		if status.Context == job.Context && status.State == github.StatusSuccess {
			return true
		}
	}
	return false
}
