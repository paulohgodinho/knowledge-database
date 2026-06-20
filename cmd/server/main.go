package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	Title       string
	Placeholder string
}

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
<div class="alert alert-success mt-4">
	<div>
		<p>Shared URL: <a class="link link-primary break-all" href="{{ .URL }}" target="_blank" rel="noopener noreferrer">{{ .URL }}</a></p>
		<p class="mt-1">Saved as: <span class="font-mono">{{ .FileName }}</span></p>
		<p class="mt-1">Branch: <span class="font-mono">{{ .BranchName }}</span></p>
	</div>
</div>
`))
	errorResultTmpl := template.Must(template.New("error-result").Parse(`
<div class="alert alert-error mt-4">
  <span>{{ .Error }}</span>
</div>
`))

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		data := PageData{
			Title:       "Knowledge Database",
			Placeholder: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		}

		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, "failed to render template", http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/share", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form payload", http.StatusBadRequest)
			return
		}

		normalizedURL, err := validateAndNormalizeURL(r.FormValue("url"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = errorResultTmpl.Execute(w, map[string]string{"Error": err.Error()})
			return
		}

		fileName := buildArtifactNameFromURL(normalizedURL, ".txt")
		branchName := "review/" + fileName
		if err := saveURLInDatabaseAndPushBranch(normalizedURL, fileName, branchName); err != nil {
			slog.Error("failed to save shared URL", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			_ = errorResultTmpl.Execute(w, map[string]string{"Error": "failed to persist URL to repository"})
			return
		}

		_ = shareResultTmpl.Execute(w, map[string]string{
			"URL":        normalizedURL,
			"FileName":   fileName,
			"BranchName": branchName,
		})
	})

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	slog.Info("server listening", "addr", ":8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
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

func buildArtifactNameFromURL(rawURL string, extension string) string {
	hash := sha256.Sum256([]byte(rawURL))
	hashHex := fmt.Sprintf("%x", hash)

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Sprintf("fallback_%s%s", hashHex, extension)
	}

	cleanStr := u.Host + u.Path
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

	return fmt.Sprintf("%s_%s%s", cleanStr, hashHex, extension)
}

func saveURLInDatabaseAndPushBranch(sharedURL string, fileName string, branchName string) error {
	pat := strings.TrimSpace(os.Getenv("GITHUB_PAT"))
	if pat == "" {
		return errors.New("missing GITHUB_PAT environment variable")
	}

	repoRoot := filepath.Clean("repo")
	if repoRoot == "." || repoRoot == string(filepath.Separator) {
		return errors.New("invalid repository path")
	}

	repository, err := git.PlainOpen(repoRoot)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	if err := ensureDatabaseMarker(repoRoot); err != nil {
		return err
	}

	worktree, err := repository.Worktree()
	if err != nil {
		return fmt.Errorf("failed to load repository worktree: %w", err)
	}

	branchRef := plumbing.NewBranchReferenceName(branchName)
	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: branchRef,
		Create: true,
	})
	if err != nil {
		return fmt.Errorf("failed to create review branch: %w", err)
	}

	if err := ensureDatabaseMarker(repoRoot); err != nil {
		return err
	}

	databaseDir := filepath.Join(repoRoot, "database")
	if err := os.MkdirAll(databaseDir, 0o755); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	filePath := filepath.Join(databaseDir, fileName)
	if err := os.WriteFile(filePath, []byte(sharedURL+"\n"), 0o644); err != nil {
		return fmt.Errorf("failed to write URL file: %w", err)
	}

	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("database", ".database"))); err != nil {
		return fmt.Errorf("failed to stage database marker: %w", err)
	}
	if _, err := worktree.Add(filepath.ToSlash(filepath.Join("database", fileName))); err != nil {
		return fmt.Errorf("failed to stage URL file: %w", err)
	}

	author := &object.Signature{
		Name:  "knowledge-database-bot",
		Email: "bot@knowledge-database.local",
		When:  time.Now(),
	}

	commitMessage := "Add shared URL: " + fileName
	if _, err := worktree.Commit(commitMessage, &git.CommitOptions{Author: author}); err != nil {
		return fmt.Errorf("failed to commit URL file: %w", err)
	}

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
		return fmt.Errorf("failed to push review branch: %w", err)
	}

	return nil
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
