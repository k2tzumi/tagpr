package rcpr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/Songmu/gitsemvers"
	"github.com/google/go-github/v45/github"
)

const (
	gitUser              = "github-actions[bot]"
	gitEmail             = "github-actions[bot]@users.noreply.github.com"
	defaultReleaseBranch = "main"
	autoCommitMessage    = "[rcpr] prepare for the next release"
	autoChangelogMessage = "[rcpr] update CHANGELOG.md"
	autoLableName        = "rcpr"
)

type rcpr struct {
	c                       *commander
	gh                      *github.Client
	cfg                     *config
	gitPath                 string
	remoteName, owner, repo string
}

func (rp *rcpr) latestSemverTag() string {
	vers := (&gitsemvers.Semvers{GitPath: rp.gitPath}).VersionStrings()
	if len(vers) > 0 {
		return vers[0]
	}
	return ""
}

func newRcpr(ctx context.Context, c *commander) (*rcpr, error) {
	rp := &rcpr{c: c, gitPath: c.gitPath}

	var err error
	rp.remoteName, err = rp.detectRemote()
	if err != nil {
		return nil, err
	}
	remoteURL, _, err := rp.c.GitE("config", "remote."+rp.remoteName+".url")
	if err != nil {
		return nil, err
	}
	u, err := parseGitURL(remoteURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse remote")
	}
	m := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(m) < 2 {
		return nil, fmt.Errorf("failed to detect owner and repo from remote URL")
	}
	rp.owner = m[0]
	repo := m[1]
	if u.Scheme == "ssh" || u.Scheme == "git" {
		repo = strings.TrimSuffix(repo, ".git")
	}
	rp.repo = repo

	cli, err := ghClient(ctx, "", u.Hostname())
	if err != nil {
		return nil, err
	}
	rp.gh = cli

	isShallow, _, err := rp.c.GitE("rev-parse", "--is-shallow-repository")
	if err != nil {
		return nil, err
	}
	if isShallow == "true" {
		if _, _, err := rp.c.GitE("fetch", "--unshallow"); err != nil {
			return nil, err
		}
	}
	rp.cfg, err = newConfig(rp.gitPath)
	if err != nil {
		return nil, err
	}
	return rp, nil
}

func isRcpr(pr *github.PullRequest) bool {
	if pr == nil {
		return false
	}
	for _, label := range pr.Labels {
		if label.GetName() == autoLableName {
			return true
		}
	}
	return false
}

func (rp *rcpr) Run(ctx context.Context) error {
	latestSemverTag := rp.latestSemverTag()
	currVerStr := latestSemverTag
	if currVerStr == "" {
		currVerStr = "v0.0.0"
	}
	currVer, err := newSemver(currVerStr)
	if err != nil {
		return err
	}

	if rp.cfg.vPrefix == nil {
		if err := rp.cfg.SetVPrefix(currVer.vPrefix); err != nil {
			return err
		}
	} else {
		currVer.vPrefix = *rp.cfg.vPrefix
	}

	var releaseBranch string
	if r := rp.cfg.ReleaseBranch(); r != nil {
		releaseBranch = r.String()
	}
	if releaseBranch == "" {
		releaseBranch, _ = rp.defaultBranch()
		if releaseBranch == "" {
			releaseBranch = defaultReleaseBranch
		}
		if err := rp.cfg.SetRelaseBranch(releaseBranch); err != nil {
			return err
		}
	}

	branch, _, err := rp.c.GitE("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("failed to git symbolic-ref: %w", err)
	}
	if branch != releaseBranch {
		return fmt.Errorf("you are not on release branch %q, current branch is %q",
			releaseBranch, branch)
	}

	// XXX: should care GIT_*_NAME etc?
	if _, _, err := rp.c.GitE("config", "user.email"); err != nil {
		rp.c.Git("config", "--local", "user.email", gitEmail)
	}
	if _, _, err := rp.c.GitE("config", "user.name"); err != nil {
		rp.c.Git("config", "--local", "user.name", gitUser)
	}

	// If the latest commit is a merge commit of the pull request by rcpr,
	// tag the semver to the commit and create a release and exit.
	if pr, err := rp.latestPullRequest(ctx); err != nil || isRcpr(pr) {
		if err != nil {
			return err
		}
		return rp.tagRelease(ctx, pr, currVer, latestSemverTag)
	}

	rcBranch := fmt.Sprintf("rcpr-from-%s", currVer.Tag())
	rp.c.GitE("branch", "-D", rcBranch)
	rp.c.Git("checkout", "-b", rcBranch)

	head := fmt.Sprintf("%s:%s", rp.owner, rcBranch)
	pulls, _, err := rp.gh.PullRequests.List(ctx, rp.owner, rp.repo,
		&github.PullRequestListOptions{
			Head: head,
			Base: releaseBranch,
		})
	if err != nil {
		return err
	}

	var (
		labels   []*github.Label
		currRcpr *github.PullRequest
	)
	if len(pulls) > 0 {
		currRcpr = pulls[0]
		labels = currRcpr.Labels
	}
	nextVer := currVer.GuessNext(labels)

	var vfiles []string
	if vf := rp.cfg.VersionFile(); vf != nil {
		vfiles = strings.Split(vf.String(), ",")
		for i, v := range vfiles {
			vfiles[i] = strings.TrimSpace(v)
		}
	} else {
		vfile, err := detectVersionFile(".", currVer)
		if err != nil {
			return err
		}
		if err := rp.cfg.SetVersionFile(vfile); err != nil {
			return err
		}
		vfiles = []string{vfile}
	}

	if com := rp.cfg.Command(); com != nil {
		prog := com.String()
		var progArgs []string
		if strings.ContainsAny(prog, " \n") {
			prog = "sh"
			progArgs = []string{"-c", prog}
		}
		rp.c.cmdE(prog, progArgs...)
	}

	if vfiles[0] != "" {
		for _, vfile := range vfiles {
			if err := bumpVersionFile(vfile, currVer, nextVer); err != nil {
				return err
			}
		}
	}
	rp.c.GitE("add", "-f", rp.cfg.conf) // ignore any errors

	const releaseYml = ".github/release.yml"
	// TODO: It would be nice to be able to add an exclude setting even if release.yml already exists.
	if !exists(releaseYml) {
		if err := os.MkdirAll(filepath.Dir(releaseYml), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(releaseYml, []byte(`changelog:
  exclude:
    labels:
      - rcpr
`), 0644); err != nil {
			return err
		}
		rp.c.GitE("add", "-f", releaseYml)
	}

	rp.c.Git("commit", "--allow-empty", "-am", autoCommitMessage)

	// cherry-pick if the remote branch is exists and changed
	// XXX: Do I need to apply merge commits too?
	//     (We ommited merge commits for now, because if we cherry-pick them, we need to add options like "-m 1".
	out, _, err := rp.c.GitE(
		"log", "--no-merges", "--pretty=format:%h %s", "main.."+rp.remoteName+"/"+rcBranch)
	if err == nil {
		var cherryPicks []string
		for _, line := range strings.Split(out, "\n") {
			m := strings.SplitN(line, " ", 2)
			if len(m) < 2 {
				continue
			}
			commitish := m[0]
			subject := strings.TrimSpace(m[1])
			if subject != autoCommitMessage && subject != autoChangelogMessage {
				cherryPicks = append(cherryPicks, commitish)
			}
		}
		if len(cherryPicks) > 0 {
			// Specify a commitish one by one for cherry-pick instead of multiple commitish,
			// and apply it as much as possible.
			for i := len(cherryPicks) - 1; i >= 0; i-- {
				commitish := cherryPicks[i]
				_, _, err := rp.c.GitE(
					"cherry-pick", "--keep-redundant-commits", "--allow-empty", commitish)

				// conflict, etc. / Need error handling in case of non-conflict error?
				if err != nil {
					rp.c.GitE("cherry-pick", "--abort")
				}
			}
		}
	}

	// Reread the configuration file (.rcpr) as it may have been rewritten during the cherry-pick process.
	rp.cfg.Reload()
	if rp.cfg.VersionFile() != nil {
		vfiles = strings.Split(rp.cfg.VersionFile().String(), ",")
		for i, v := range vfiles {
			vfiles[i] = strings.TrimSpace(v)
		}
	}
	if vfiles[0] != "" {
		nVer, _ := retrieveVersionFromFile(vfiles[0], nextVer.vPrefix)
		if nVer != nil && nVer.Naked() != nextVer.Naked() {
			nextVer = nVer
		}
	}
	previousTag := &latestSemverTag
	if *previousTag == "" {
		previousTag = nil
	}
	releases, _, err := rp.gh.Repositories.GenerateReleaseNotes(
		ctx, rp.owner, rp.repo, &github.GenerateNotesOptions{
			TagName:         nextVer.Tag(),
			PreviousTagName: previousTag,
			TargetCommitish: &releaseBranch,
		})
	if err != nil {
		return err
	}

	changelog := convertKeepAChangelogFormat(releases.Body, time.Now())
	changelogMd := "CHANGELOG.md"

	var content string
	if exists(changelogMd) {
		byt, err := os.ReadFile(changelogMd)
		if err != nil {
			return err
		}
		content = strings.TrimSpace(string(byt)) + "\n"
	}

	// If the changelog is not in "keep a changelog" format, or if the file does not exist, re-create everything. Is it rough...?
	if !changelogReg.MatchString(content) {
		// We are concerned that depending on the release history, API requests may become more frequent.
		vers := (&gitsemvers.Semvers{GitPath: rp.gitPath}).VersionStrings()
		logs := []string{"# Changelog\n"}
		for i, ver := range vers {
			if i > 10 {
				break
			}
			date, _, _ := rp.c.GitE("log", "-1", "--format=%ai", "--date=iso", ver)
			d, _ := time.Parse("2006-01-02 15:04:05 -0700", date)
			releases, _, _ := rp.gh.Repositories.GenerateReleaseNotes(
				ctx, rp.owner, rp.repo, &github.GenerateNotesOptions{
					TagName: ver,
				})
			logs = append(logs, strings.TrimSpace(convertKeepAChangelogFormat(releases.Body, d))+"\n")
		}
		content = strings.Join(logs, "\n")
	}

	content = insertNewChangelog(content, changelog)
	if err := os.WriteFile(changelogMd, []byte(content), 0644); err != nil {
		return err
	}
	rp.c.GitE("add", changelogMd)
	rp.c.GitE("commit", "-m", autoChangelogMessage)

	if _, _, err := rp.c.GitE("push", "--force", rp.remoteName, rcBranch); err != nil {
		return err
	}

	var tmpl *template.Template
	if t := rp.cfg.Template(); t != nil {
		tmpTmpl, err := template.ParseFiles(t.String())
		if err == nil {
			tmpl = tmpTmpl
		} else {
			log.Printf("parse configured template failed: %s\n", err)
		}
	}
	pt := newPRTmpl(tmpl)
	prText, err := pt.Render(&tmplArg{
		NextVersion: nextVer.Tag(),
		RCBranch:    rcBranch,
		Changelog:   releases.Body,
	})
	if err != nil {
		return err
	}

	stuffs := strings.SplitN(strings.TrimSpace(prText), "\n", 2)
	title := stuffs[0]
	var body string
	if len(stuffs) > 1 {
		body = strings.TrimSpace(stuffs[1])
	}
	if currRcpr == nil {
		pr, _, err := rp.gh.PullRequests.Create(ctx, rp.owner, rp.repo, &github.NewPullRequest{
			Title: github.String(title),
			Body:  github.String(body),
			Base:  &releaseBranch,
			Head:  github.String(head),
		})
		if err != nil {
			return err
		}
		_, _, err = rp.gh.Issues.AddLabelsToIssue(
			ctx, rp.owner, rp.repo, *pr.Number, []string{autoLableName})
		return err
	}
	currRcpr.Title = github.String(title)
	currRcpr.Body = github.String(mergeBody(*currRcpr.Body, body))
	_, _, err = rp.gh.PullRequests.Edit(ctx, rp.owner, rp.repo, *currRcpr.Number, currRcpr)
	return err
}

var (
	hasSchemeReg  = regexp.MustCompile("^[^:]+://")
	scpLikeURLReg = regexp.MustCompile("^([^@]+@)?([^:]+):(/?.+)$")
)

func parseGitURL(u string) (*url.URL, error) {
	if !hasSchemeReg.MatchString(u) {
		if m := scpLikeURLReg.FindStringSubmatch(u); len(m) == 4 {
			u = fmt.Sprintf("ssh://%s%s/%s", m[1], m[2], strings.TrimPrefix(m[3], "/"))
		}
	}
	return url.Parse(u)
}

func mergeBody(now, update string) string {
	// TODO: If there are check boxes, respect what is checked, etc.
	return update
}

var headBranchReg = regexp.MustCompile(`(?m)^\s*HEAD branch: (.*)$`)

func (rp *rcpr) defaultBranch() (string, error) {
	// `git symbolic-ref refs/remotes/origin/HEAD` sometimes doesn't work
	// So use `git remote show origin` for detecting default branch
	show, _, err := rp.c.GitE("remote", "show", rp.remoteName)
	if err != nil {
		return "", fmt.Errorf("failed to detect defaut branch: %w", err)
	}
	m := headBranchReg.FindStringSubmatch(show)
	if len(m) < 2 {
		return "", fmt.Errorf("failed to detect default branch from remote: %s", rp.remoteName)
	}
	return m[1], nil
}

func (rp *rcpr) detectRemote() (string, error) {
	remotesStr, _, err := rp.c.GitE("remote")
	if err != nil {
		return "", fmt.Errorf("failed to detect remote: %s", err)
	}
	remotes := strings.Fields(remotesStr)
	if len(remotes) < 1 {
		return "", errors.New("failed to detect remote")
	}
	for _, r := range remotes {
		if r == "origin" {
			return r, nil
		}
	}
	// the last output is the first added remote
	return remotes[len(remotes)-1], nil
}
