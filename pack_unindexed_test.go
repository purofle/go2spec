package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNeedsGitSourceFallback(t *testing.T) {
	const gopkg = "github.com/x/pkg"
	related := &pkgsiteInfo{Module: pkgsiteModule{Path: gopkg, RepoURL: "https://" + gopkg}}
	unrelated := &pkgsiteInfo{Module: pkgsiteModule{Path: "github.com/orig/sdk", RepoURL: "https://github.com/orig/sdk"}}
	serverErr := fmt.Errorf("get pkgsite package: %w", &pkgsiteAPIError{Code: 503, Message: "unavailable"})
	notFound := fmt.Errorf("get pkgsite package: %w", &pkgsiteAPIError{Code: 404, Message: "not found"})

	for _, tt := range []struct {
		name         string
		gopkg        string
		info         *pkgsiteInfo
		err          error
		wantFallback bool
		wantErr      error
	}{
		{name: "found", gopkg: gopkg, info: related},
		{name: "subpackage", gopkg: gopkg + "/sub", info: related},
		{name: "not-found", gopkg: gopkg, err: notFound, wantFallback: true},
		{name: "server-error", gopkg: gopkg, err: serverErr, wantErr: serverErr},
		{name: "unrelated-module", gopkg: gopkg, info: unrelated, wantFallback: true},
		{name: "no-repo-url", gopkg: gopkg, info: &pkgsiteInfo{Module: pkgsiteModule{Path: gopkg}}, wantFallback: true},
		{name: "blank-repo-url", gopkg: gopkg, info: &pkgsiteInfo{Module: pkgsiteModule{Path: gopkg, RepoURL: "   "}}, wantFallback: true},
		{name: "dotgit-only-repo-url", gopkg: gopkg, info: &pkgsiteInfo{Module: pkgsiteModule{Path: gopkg, RepoURL: ".git"}}, wantFallback: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reason, err := needsGitSourceFallback(tt.gopkg, tt.info, tt.err)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := reason != ""; got != tt.wantFallback {
				t.Fatalf("fallback = %v (%q), want %v", got, reason, tt.wantFallback)
			}
		})
	}
}

func TestSourceTarballLicenseFilesSkipsExcludedDirs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"internal/LICENSE",
		".git/LICENSE",
		"Godeps/_workspace/src/dep/LICENSE",
		"sub/Godeps/_workspace/LICENSE",
		"sub/vendor/dep/LICENSE",
	} {
		aggregateWriteFile(t, filepath.Join(dir, name), []byte("license"))
	}
	if got, err := sourceTarballLicenseFiles(dir); err != nil || strings.Join(got, ",") != "internal/LICENSE" {
		t.Fatalf("nested licenses = %q, %v; want internal/LICENSE", got, err)
	}
	aggregateWriteFile(t, filepath.Join(dir, "LICENSE"), []byte("license"))
	if got, err := sourceTarballLicenseFiles(dir); err != nil || strings.Join(got, ",") != "LICENSE" {
		t.Fatalf("licenses = %q, %v; want root LICENSE only", got, err)
	}
}

func TestReadSpecMetadataLocal(t *testing.T) {
	dir := t.TempDir()
	aggregateWriteFile(t, filepath.Join(dir, "README.markdown"), []byte("# Fork\n\nA standalone fork description.\n"))
	m := readSpecMetadata("github.com/example/sdk-fork", dir)
	if m.assetFiles.readmeFile != "README.markdown" {
		t.Fatalf("readmeFile = %q, want README.markdown", m.assetFiles.readmeFile)
	}
	if m.description != "A standalone fork description" {
		t.Fatalf("description = %q, want README summary", m.description)
	}
}

func TestValidateRepoURL(t *testing.T) {
	for _, valid := range []string{
		"https://github.com/yeongaori/discordgo-fork",
		"https://gitlab.com/group/sub/repo.git",
		"http://git.internal.example/team/repo",
	} {
		if _, err := validateRepoURL(valid); err != nil {
			t.Fatalf("validateRepoURL(%q) unexpected error: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"",
		"--upload-pack=evil",
		"ssh://git@github.com/user/repo",
		"https://user:pass@github.com/user/repo",
		"https://github.com/user/repo?foo=bar",
		"https://github.com/user/repo#frag",
		"https://github.com/user/repo\nfoo",
	} {
		if _, err := validateRepoURL(invalid); err == nil {
			t.Fatalf("validateRepoURL(%q) succeeded, want error", invalid)
		}
	}
}

func createTestGitRepo(t *testing.T, files map[string]string, tag string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	for name, content := range files {
		aggregateWriteFile(t, filepath.Join(repo, name), []byte(content))
	}
	runGit(t, repo, nil, "init")
	runGit(t, repo, nil, "config", "commit.gpgsign", "false")
	runGit(t, repo, nil, "config", "tag.gpgsign", "false")
	runGit(t, repo, nil, "add", ".")
	runGit(t, repo, []string{
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	}, "commit", "-m", "initial")
	if tag != "" {
		runGit(t, repo, nil, "tag", tag)
	}
	hash := strings.TrimSpace(runGit(t, repo, nil, "rev-parse", "HEAD"))
	return repo, hash
}

func installGitInsteadOfWrapper(t *testing.T, fromURL, toDir string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	bin := t.TempDir()
	wrapper := "#!/bin/sh\nexec " + quote(git) + " -c " + quote("url.file://"+toDir+".insteadOf="+fromURL) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPackForkWithoutCustomProvidesUsesGoModImportPath(t *testing.T) {
	const fork = "github.com/go2spec-fixtures/sdk-fork"
	const original = "github.com/go2spec-fixtures/sdk"
	repo, _ := createTestGitRepo(t, map[string]string{
		"go.mod":    "module " + original + "\n\ngo 1.18\n",
		"sdk.go":    "package sdk\n",
		"LICENSE":   "MIT\n",
		"README.md": "Fork library description.\n",
	}, "v1.0.0")
	archive := runGit(t, repo, nil, "archive", "--format=tar.gz", "--prefix=sdk-fork-1.0.0/", "v1.0.0")
	installGitInsteadOfWrapper(t, "https://"+fork, repo)

	oldHTTPClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "https://"+fork+"/archive/v1.0.0.tar.gz" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(archive)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldHTTPClient })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":404,"message":"not found"}`)
	}))
	t.Cleanup(srv.Close)
	oldBase, oldClient, oldCache := pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache
	pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache = srv.URL, srv.Client(), make(map[string]*pkgsiteInfo)
	t.Cleanup(func() {
		pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache = oldBase, oldClient, oldCache
	})

	t.Chdir(t.TempDir())
	if code := mainPack([]string{fork + "@v1.0.0"}, nil); code != 0 {
		t.Fatalf("mainPack() = %d, want 0", code)
	}
	const rpmName = "go-github-go2spec-fixtures-sdk-fork"
	data, err := os.ReadFile(filepath.Join(rpmName, rpmName+".spec"))
	if err != nil {
		t.Fatal(err)
	}
	spec := string(data)
	for _, want := range []string{
		"%define go_import_path  " + original + "\n",
		"Provides:       go(" + original + ") = %{version}\n",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("missing %q in spec:\n%s", want, spec)
		}
	}
	if strings.Contains(spec, "Provides:       go("+fork+")") {
		t.Fatalf("unexpected custom Provides when -provides was omitted:\n%s", spec)
	}
}

func TestPackExplicitRepoURLBypassesPkgsite(t *testing.T) {
	const original = "github.com/go2spec-fixtures/sdk"
	const customRepo = "https://github.com/go2spec-fixtures/custom-fork"
	const customProvides = "github.com/go2spec-fixtures/custom-fork"
	repo, _ := createTestGitRepo(t, map[string]string{
		"go.mod":    "module " + original + "\n\ngo 1.18\n",
		"sdk.go":    "package sdk\n",
		"LICENSE":   "MIT\n",
		"README.md": "Custom fork repository.\n",
	}, "v1.0.0")
	archive := runGit(t, repo, nil, "archive", "--format=tar.gz", "--prefix=custom-fork-1.0.0/", "v1.0.0")
	installGitInsteadOfWrapper(t, customRepo, repo)

	oldHTTPClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == customRepo+"/archive/v1.0.0.tar.gz" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(archive)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldHTTPClient })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected pkgsite request when -repo_url is set: %s", r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	oldBase, oldClient, oldCache := pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache
	pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache = srv.URL, srv.Client(), make(map[string]*pkgsiteInfo)
	t.Cleanup(func() {
		pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache = oldBase, oldClient, oldCache
	})

	t.Chdir(t.TempDir())
	if code := mainPack([]string{"-type", "library", "-repo_url", customRepo, "-provides", customProvides, customProvides + "@v1.0.0"}, nil); code != 0 {
		t.Fatalf("mainPack() = %d, want 0", code)
	}
	const rpmName = "go-github-go2spec-fixtures-custom-fork"
	data, err := os.ReadFile(filepath.Join(rpmName, rpmName+".spec"))
	if err != nil {
		t.Fatal(err)
	}
	spec := string(data)
	for _, want := range []string{
		"%define go_import_path  " + original + "\n",
		"Provides:       go(" + customProvides + ") = %{version}\n",
		"URL:            " + customRepo + "\n",
		"Source0:        " + customRepo + "/archive/v%{version}.tar.gz#/%{_name}-%{version}.tar.gz\n",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("missing %q in spec:\n%s", want, spec)
		}
	}
}
