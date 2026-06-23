package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

type PageData struct {
	Title         string
	Placeholder   string
	StylesVersion string
}

type ShareErrorCardData struct {
	URL   string
	Error string
	Hint  string
}

const artifactDelimiter = "-"
const artifactHashLength = 64
const artifactRandomSuffixLength = 6

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("system is initializing")

	if err := checkoutRepositoryOnStartup(); err != nil {
		slog.Error("startup checkout failed", "error", err)
		os.Exit(1)
	}

	tmpl := template.Must(template.ParseFiles("web/templates/index.html"))
	shareResultTmpl := template.Must(template.New("share-result").Parse(`
<article class="card bg-base-100 shadow-md border border-success/30">
	<div class="card-body p-4">
		<p class="text-sm text-base-content/70">Shared page</p>
		<p><a class="link link-primary break-all" href="{{ .URL }}" target="_blank" rel="noopener noreferrer">{{ .URL }}</a></p>
		<p class="text-sm text-base-content/70 mt-2">Pull request</p>
		<p><a class="link link-secondary break-all" href="{{ .PRURL }}" target="_blank" rel="noopener noreferrer">{{ .PRURL }}</a></p>
	</div>
</article>
`))
	errorResultTmpl := template.Must(template.New("error-result").Parse(`
<article class="card bg-base-100 shadow-md border border-error/40">
	<div class="card-body p-4">
		<p class="text-sm text-base-content/70">Shared page</p>
		<p class="break-all">{{ .URL }}</p>
		<p class="text-sm text-base-content/70 mt-2">Error</p>
		<p class="text-error break-words">{{ .Error }}</p>
		{{ if .Hint }}
		<p class="text-sm text-warning mt-2">{{ .Hint }}</p>
		{{ end }}
	</div>
</article>
`))

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		data := PageData{
			Title:         "Knowledge Database",
			Placeholder:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			StylesVersion: currentStylesVersion(),
		}

		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, "failed to render template", http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/share", func(w http.ResponseWriter, r *http.Request) {
		writeShareError := func(status int, card ShareErrorCardData) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
			if err := errorResultTmpl.Execute(w, card); err != nil {
				slog.Error("failed to render error card", "error", err)
			}
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeShareError(http.StatusMethodNotAllowed, ShareErrorCardData{
				URL:   strings.TrimSpace(r.FormValue("url")),
				Error: "method not allowed",
			})
			return
		}

		if err := r.ParseForm(); err != nil {
			writeShareError(http.StatusBadRequest, ShareErrorCardData{
				URL:   strings.TrimSpace(r.FormValue("url")),
				Error: "invalid form payload",
			})
			return
		}

		rawURL := strings.TrimSpace(r.FormValue("url"))
		normalizedURL, err := validateAndNormalizeURL(rawURL)
		if err != nil {
			writeShareError(http.StatusBadRequest, ShareErrorCardData{URL: rawURL, Error: err.Error()})
			return
		}

		fileName, err := buildArtifactNameFromURL(normalizedURL)
		if err != nil {
			slog.Error("failed to generate artifact name", "error", err)
			writeShareError(http.StatusInternalServerError, ShareErrorCardData{URL: normalizedURL, Error: err.Error()})
			return
		}
		branchName := "add/" + fileName
		prURL, err := saveURLInDatabaseAndPushBranch(normalizedURL, fileName, branchName)
		if err != nil {
			slog.Error("failed to save shared URL", "error", err)
			writeShareError(http.StatusInternalServerError, ShareErrorCardData{
				URL:   normalizedURL,
				Error: err.Error(),
				Hint:  shareErrorHint(err),
			})
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = shareResultTmpl.Execute(w, map[string]string{
			"URL":        normalizedURL,
			"FileName":   fileName,
			"BranchName": branchName,
			"PRURL":      prURL,
		})
	})

	server := &http.Server{
		Addr:    "0.0.0.0:8080",
		Handler: mux,
	}

	slog.Info("server listening", "addr", "0.0.0.0:8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func shareErrorHint(err error) string {
	if err == nil {
		return ""
	}

	message := strings.ToLower(err.Error())
	if strings.Contains(message, "failed to push add branch") {
		if strings.Contains(message, "object not found") || strings.Contains(message, "already exists") || strings.Contains(message, "non-fast-forward") || strings.Contains(message, "failed to update ref") {
			return "This URL was probably already shared"
		}
	}

	return ""
}

func currentStylesVersion() string {
	info, err := os.Stat("web/static/output.css")
	if err == nil {
		return strconv.FormatInt(info.ModTime().Unix(), 10)
	}
	return strconv.FormatInt(time.Now().Unix(), 10)
}

func checkoutRepositoryOnStartup() error {
	pat := strings.TrimSpace(os.Getenv("GITHUB_PAT"))
	repoURL := strings.TrimSpace(os.Getenv("GITHUB_REPO_URL"))

	if pat == "" {
		return errors.New("missing GITHUB_PAT environment variable")
	}
	if repoURL == "" {
		return errors.New("missing GITHUB_REPO_URL environment variable")
	}

	normalizedRepoURL, err := validateGitHubRepoURL(repoURL)
	if err != nil {
		return err
	}

	checkoutPath := filepath.Clean("repo")
	if checkoutPath == "." || checkoutPath == string(filepath.Separator) {
		return errors.New("invalid checkout path")
	}

	if err := os.MkdirAll(checkoutPath, 0o755); err != nil {
		return fmt.Errorf("failed to prepare checkout path: %w", err)
	}

	if err := clearDirectoryContents(checkoutPath); err != nil {
		return fmt.Errorf("failed to reset checkout path: %w", err)
	}

	slog.Info("repo is being cloned", "repo_url", normalizedRepoURL, "target", checkoutPath)
	_, err = git.PlainClone(checkoutPath, false, &git.CloneOptions{
		URL:   normalizedRepoURL,
		Depth: 1,
		Auth: &githttp.BasicAuth{
			Username: "x-access-token",
			Password: pat,
		},
	})
	if err != nil {
		return fmt.Errorf("clone failed: %w", err)
	}
	slog.Info("repo cloned successfully", "target", checkoutPath)
	if err := ensureDatabaseMarker(checkoutPath); err != nil {
		return err
	}

	return nil
}

func ensureDatabaseMarker(repoRoot string) error {
	databaseDir := filepath.Join(repoRoot, "database")
	if err := os.MkdirAll(databaseDir, 0o755); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	markerPath := filepath.Join(databaseDir, ".database")
	if err := os.WriteFile(markerPath, []byte("database\n"), 0o644); err != nil {
		return fmt.Errorf("failed to write database marker: %w", err)
	}

	return nil
}

func buildArtifactNameFromURL(rawURL string) (string, error) {
	hash := sha256.Sum256([]byte(rawURL))
	hashHex := fmt.Sprintf("%x", hash)
	randomSuffix, err := randomAlphaNumeric(artifactRandomSuffixLength)
	if err != nil {
		return "", fmt.Errorf("failed to generate random suffix: %w", err)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Sprintf("fallback%s%s%s%s", artifactDelimiter, hashHex, artifactDelimiter, randomSuffix), nil
	}

	host := strings.TrimPrefix(u.Hostname(), "www.")
	cleanStr := host + u.Path
	cleanStr = strings.ReplaceAll(cleanStr, "/", "-")
	cleanStr = strings.ReplaceAll(cleanStr, ":", "-")
	cleanStr = strings.Trim(cleanStr, "-.")
	if cleanStr == "" {
		cleanStr = "fallback"
	}

	runes := []rune(cleanStr)
	if len(runes) > 100 {
		cleanStr = string(runes[:100])
	}

	return fmt.Sprintf("%s%s%s%s%s", cleanStr, artifactDelimiter, hashHex, artifactDelimiter, randomSuffix), nil
}

func randomAlphaNumeric(length int) (string, error) {
	if length <= 0 {
		return "", errors.New("random suffix length must be positive")
	}

	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	randomBytes := make([]byte, length)
	if _, err := crand.Read(randomBytes); err != nil {
		return "", err
	}

	for i, b := range randomBytes {
		randomBytes[i] = alphabet[int(b)%len(alphabet)]
	}

	return string(randomBytes), nil
}

func saveURLInDatabaseAndPushBranch(sharedURL string, fileName string, branchName string) (prURL string, retErr error) {
	slog.Info("starting repository persistence", "file_name", fileName, "branch", branchName)

	pat := strings.TrimSpace(os.Getenv("GITHUB_PAT"))
	if pat == "" {
		return "", errors.New("missing GITHUB_PAT environment variable")
	}
	baseBranch := getBaseBranch()

	repoRoot := filepath.Clean("repo")
	if repoRoot == "." || repoRoot == string(filepath.Separator) {
		return "", errors.New("invalid repository path")
	}

	repository, err := git.PlainOpen(repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to open repository: %w", err)
	}

	if err := ensureDatabaseMarker(repoRoot); err != nil {
		return "", err
	}

	worktree, err := repository.Worktree()
	if err != nil {
		return "", fmt.Errorf("failed to load repository worktree: %w", err)
	}

	defer func() {
		syncErr := checkoutBaseBranchAndPull(repository, worktree, pat, baseBranch)
		if syncErr != nil {
			slog.Error("failed to return repository to base branch", "branch", baseBranch, "error", syncErr)
			if retErr == nil {
				retErr = syncErr
			}
			return
		}
		slog.Info("repository synchronized", "branch", baseBranch)

		deletedCount, cleanupErr := cleanupLocalAddBranches(repository, "add/")
		if cleanupErr != nil {
			slog.Error("failed to cleanup add branches", "error", cleanupErr)
			if retErr == nil {
				retErr = cleanupErr
			}
			return
		}
		if deletedCount > 0 {
			slog.Info("cleaned up add branches", "count", deletedCount)
		}
	}()

	branchRef := plumbing.NewBranchReferenceName(branchName)
	slog.Info("creating review branch", "branch", branchRef.String())
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: branchRef,
		Create: true,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create add branch: %w", err)
	}
	slog.Info("add branch ready", "branch", branchRef.String())

	if err := ensureDatabaseMarker(repoRoot); err != nil {
		return "", err
	}

	databaseDir := filepath.Join(repoRoot, "database")
	if err := os.MkdirAll(databaseDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create database directory: %w", err)
	}

	filePath := filepath.Join(databaseDir, fileName)
	slog.Info("writing shared URL file", "path", filePath)
	if err := os.WriteFile(filePath, []byte(sharedURL+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("failed to write URL file: %w", err)
	}
	slog.Info("shared URL file written", "path", filePath)

	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("database", ".database"))); err != nil {
		return "", fmt.Errorf("failed to stage database marker: %w", err)
	}
	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("database", fileName))); err != nil {
		return "", fmt.Errorf("failed to stage URL file: %w", err)
	}
	slog.Info("repository files staged", "file_name", fileName)

	author := &object.Signature{
		Name:  "knowledge-database-bot",
		Email: "bot@knowledge-database.local",
		When:  time.Now(),
	}

	commitMessage := "Add shared URL: " + fileName
	commitHash, err := worktree.Commit(commitMessage, &git.CommitOptions{Author: author})
	if err != nil {
		return "", fmt.Errorf("failed to commit URL file: %w", err)
	}
	slog.Info("commit created", "commit", commitHash.String(), "message", commitMessage)

	slog.Info("pushing add branch", "branch", branchRef.String())
	err = repository.Push(&git.PushOptions{
		Auth: &githttp.BasicAuth{
			Username: "x-access-token",
			Password: pat,
		},
		RefSpecs: []config.RefSpec{
			config.RefSpec(branchRef.String() + ":" + branchRef.String()),
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to push add branch: %w", err)
	}
	slog.Info("add branch pushed", "branch", branchRef.String())

	prTitle := fmt.Sprintf("Add %s", deriveShortName(fileName))
	prURL, err = createPullRequest(pat, branchName, prTitle)
	if err != nil {
		return "", err
	}
	slog.Info("pull request created", "url", prURL, "title", prTitle)

	return prURL, nil
}

func checkoutBaseBranchAndPull(repository *git.Repository, worktree *git.Worktree, pat string, baseBranch string) error {
	if repository == nil || worktree == nil {
		return errors.New("repository worktree is not initialized")
	}

	baseRef := plumbing.NewBranchReferenceName(baseBranch)
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: baseRef}); err != nil {
		remoteRef := plumbing.NewRemoteReferenceName("origin", baseBranch)
		remoteBranchRef, refErr := repository.Reference(remoteRef, true)
		if refErr != nil {
			return fmt.Errorf("failed to checkout base branch %q: %w", baseBranch, err)
		}
		if err := worktree.Checkout(&git.CheckoutOptions{Branch: baseRef, Hash: remoteBranchRef.Hash(), Create: true}); err != nil {
			return fmt.Errorf("failed to create local base branch %q: %w", baseBranch, err)
		}
	}

	err := worktree.Pull(&git.PullOptions{
		RemoteName:    "origin",
		ReferenceName: baseRef,
		SingleBranch:  true,
		Auth: &githttp.BasicAuth{
			Username: "x-access-token",
			Password: pat,
		},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("failed to pull base branch %q: %w", baseBranch, err)
	}

	return nil
}

func cleanupLocalAddBranches(repository *git.Repository, prefix string) (int, error) {
	if repository == nil {
		return 0, errors.New("repository is not initialized")
	}

	branches, err := repository.Branches()
	if err != nil {
		return 0, fmt.Errorf("failed to list local branches: %w", err)
	}

	toDelete := make([]plumbing.ReferenceName, 0)
	err = branches.ForEach(func(ref *plumbing.Reference) error {
		if ref == nil {
			return nil
		}
		if strings.HasPrefix(ref.Name().Short(), prefix) {
			toDelete = append(toDelete, ref.Name())
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("failed to iterate local branches: %w", err)
	}

	deleted := 0
	for _, refName := range toDelete {
		if err := repository.Storer.RemoveReference(refName); err != nil {
			return deleted, fmt.Errorf("failed to delete local branch %q: %w", refName.Short(), err)
		}
		deleted++
	}

	return deleted, nil
}

func deriveShortName(fileName string) string {
	sanitized := strings.TrimSpace(fileName)
	hashPart := ""
	randomSuffixLen := len(artifactDelimiter) + artifactRandomSuffixLength
	if len(sanitized) > randomSuffixLen {
		suffixStart := len(sanitized) - artifactRandomSuffixLength
		delimiterStart := suffixStart - len(artifactDelimiter)
		candidateSuffix := sanitized[suffixStart:]
		delimiterPart := sanitized[delimiterStart:suffixStart]
		if delimiterPart == artifactDelimiter && isAlphaNumericString(candidateSuffix) {
			sanitized = strings.TrimSpace(sanitized[:delimiterStart])
		}
	}

	suffixLen := len(artifactDelimiter) + artifactHashLength
	if len(sanitized) > suffixLen {
		hashStart := len(sanitized) - artifactHashLength
		delimiterStart := hashStart - len(artifactDelimiter)
		candidateHash := sanitized[hashStart:]
		delimiterPart := sanitized[delimiterStart:hashStart]
		if delimiterPart == artifactDelimiter && isHexString(candidateHash) {
			hashPart = candidateHash
			sanitized = strings.TrimSpace(sanitized[:delimiterStart])
		}
	}

	if sanitized == "" {
		return "fallback"
	}

	runes := []rune(sanitized)
	if len(runes) > 30 {
		sanitized = string(runes[:30])
	}

	if hashPart != "" {
		return sanitized + artifactDelimiter + hashPart
	}

	return sanitized
}

func isAlphaNumericString(value string) bool {
	if len(value) == 0 {
		return false
	}

	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}

	return true
}

func isHexString(value string) bool {
	if len(value) == 0 {
		return false
	}

	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}

	return true
}

func createPullRequest(pat string, branchName string, title string) (string, error) {
	repoURL := strings.TrimSpace(os.Getenv("GITHUB_REPO_URL"))
	owner, repo, err := parseGitHubOwnerRepo(repoURL)
	if err != nil {
		return "", err
	}

	baseBranch := getBaseBranch()

	payload := map[string]string{
		"title": title,
		"head":  branchName,
		"base":  baseBranch,
		"body":  "Automated PR for shared URL persistence.",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to encode pull request payload: %w", err)
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, repo)
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to build pull request request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call GitHub pull request API: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("failed to create pull request: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var prResp struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return "", fmt.Errorf("failed to parse pull request response: %w", err)
	}
	if prResp.HTMLURL == "" {
		return "", errors.New("pull request response did not include html_url")
	}

	return prResp.HTMLURL, nil
}

func getBaseBranch() string {
	baseBranch := strings.TrimSpace(os.Getenv("GITHUB_BASE_BRANCH"))
	if baseBranch == "" {
		return "main"
	}
	return baseBranch
}

func parseGitHubOwnerRepo(repoURL string) (string, string, error) {
	normalized, err := validateGitHubRepoURL(repoURL)
	if err != nil {
		return "", "", err
	}

	u, err := url.Parse(normalized)
	if err != nil {
		return "", "", errors.New("invalid repository URL format")
	}

	path := strings.Trim(u.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return "", "", errors.New("repository URL must include owner and repository")
	}

	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if owner == "" || repo == "" {
		return "", "", errors.New("repository URL owner/repository is invalid")
	}

	return owner, repo, nil
}

func clearDirectoryContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		entryPath := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(entryPath); err != nil {
			return err
		}
	}

	return nil
}

func validateGitHubRepoURL(raw string) (string, error) {
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", errors.New("repository URL contains invalid control characters")
		}
	}

	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("invalid repository URL format")
	}

	if strings.ToLower(parsed.Scheme) != "https" {
		return "", errors.New("repository URL must start with https://")
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", errors.New("repository host must be github.com")
	}
	if parsed.User != nil {
		return "", errors.New("repository URL must not include credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("repository URL must not include query params or fragments")
	}

	path := strings.Trim(parsed.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("repository URL must include owner and repository")
	}

	if !strings.HasSuffix(parts[1], ".git") {
		parts[1] += ".git"
	}

	return "https://github.com/" + parts[0] + "/" + parts[1], nil
}

func validateAndNormalizeURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("please provide a URL")
	}

	if len(trimmed) > 2048 {
		return "", errors.New("URL is too long")
	}

	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", errors.New("URL contains invalid control characters")
		}
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("invalid URL format")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("URL must start with http:// or https://")
	}

	if parsed.Host == "" {
		return "", errors.New("URL must include a valid host")
	}

	if parsed.User != nil {
		return "", errors.New("URL must not include user credentials")
	}

	parsed.Fragment = ""
	return parsed.String(), nil
}
