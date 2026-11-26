package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func setOfflineGoHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	aggregateWriteFile(t, filepath.Join(home, ".config", "go", "env"), []byte("GOPROXY=off\nGOSUMDB=off\nGOWORK=off\n"))
	t.Setenv("HOME", home)
}

func TestFindDependenciesModuleReplacements(t *testing.T) {
	setOfflineGoHome(t)

	gopath := t.TempDir()
	const appMod = "github.com/sipeed/picoclaw"
	repoDir := filepath.Join(gopath, "src", appMod)
	for name, content := range map[string]string{
		"go.mod": strings.Join([]string{
			"module " + appMod,
			"",
			"go 1.24",
			"",
			"require (",
			"	github.com/bwmarrin/discordgo v0.29.0",
			"	github.com/bwmarrin/discordgo-extra v0.1.0",
			"	github.com/bwmarrin/discordgo/nested v0.2.0",
			"	github.com/direct/unreplaced v1.0.0",
			"	github.com/inactive/ver v1.2.0",
			"	github.com/indirect/imported v0.3.0 // indirect",
			"	github.com/indirect/unused v0.4.0 // indirect",
			"	github.com/pinned/sdk/core v1.0.0",
			"	example.com/tools v0.0.0-00010101000000-000000000000",
			"	example.com/proto v0.0.0-00010101000000-000000000000",
			"	github.com/testonly/replaced v0.5.0",
			")",
			"",
			"replace (",
			"	github.com/bwmarrin/discordgo => github.com/yeongaori/discordgo-fork v0.0.0-20260319072544-e8e546f5d532",
			"	github.com/inactive/ver v1.0.0 => github.com/inactive/ver-fork v0.1.0",
			"	github.com/indirect/imported => github.com/myorg/forks/indirect-imported v0.3.1",
			"	github.com/indirect/unused => github.com/indirect/unused-fork v0.4.1",
			"	github.com/testonly/replaced => github.com/testonly/replaced-fork v0.5.1",
			"	github.com/pinned/sdk/core => github.com/pinned/sdk/core v1.0.1",
			"	example.com/tools => ./tools",
			"	example.com/proto => ./proto",
			")",
			"",
		}, "\n"),
		"tools/go.mod":   "module example.com/tools\n\ngo 1.24\n",
		"proto/go.mod":   "module example.com/proto\n\ngo 1.24\n",
		"proto/proto.go": "package proto\n",
		"app.go": strings.Join([]string{
			"package picoclaw",
			"import (",
			`	_ "fmt"`,
			`	_ "github.com/bwmarrin/discordgo"`,
			`	_ "github.com/bwmarrin/discordgo/voice"`,
			`	_ "github.com/bwmarrin/discordgo-extra/sub"`,
			`	_ "github.com/bwmarrin/discordgo/nested/sub"`,
			`	_ "github.com/direct/unreplaced/pkg"`,
			`	_ "github.com/inactive/ver"`,
			`	_ "github.com/indirect/imported/sub"`,
			`	_ "github.com/pinned/sdk/core"`,
			")",
			"",
		}, "\n"),
		"app_test.go": strings.Join([]string{
			"package picoclaw_test",
			`import _ "github.com/testonly/replaced/helper"`,
			"",
		}, "\n"),
	} {
		aggregateWriteFile(t, filepath.Join(repoDir, name), []byte(content))
	}

	u := &upstream{modulePath: appMod}
	if err := u.findDependencies(gopath, appMod); err != nil {
		t.Fatalf("findDependencies() error = %v", err)
	}

	gotBuild := convertDependenciesToRPM(u.repoDeps, u.exactModules)
	wantBuild := []string{
		"go(github.com/bwmarrin/discordgo-extra)",
		"go(github.com/bwmarrin/discordgo/nested)",
		"go(github.com/direct/unreplaced)",
		"go(github.com/inactive/ver)",
		"go(github.com/myorg/forks/indirect-imported)",
		"go(github.com/pinned/sdk)",
		"go(github.com/testonly/replaced-fork)",
		"go(github.com/yeongaori/discordgo-fork)",
	}
	if !slices.Equal(gotBuild, wantBuild) {
		t.Fatalf("BuildRequires = %v, want %v", gotBuild, wantBuild)
	}

	gotRun := convertDependenciesToRPM(u.repoRunDeps, u.exactModules)
	wantRun := []string{
		"go(github.com/bwmarrin/discordgo-extra)",
		"go(github.com/bwmarrin/discordgo/nested)",
		"go(github.com/direct/unreplaced)",
		"go(github.com/inactive/ver)",
		"go(github.com/myorg/forks/indirect-imported)",
		"go(github.com/pinned/sdk)",
		"go(github.com/yeongaori/discordgo-fork)",
	}
	if !slices.Equal(gotRun, wantRun) {
		t.Fatalf("Requires = %v, want %v", gotRun, wantRun)
	}
}

func TestFindDependenciesWithoutReplaceUnchanged(t *testing.T) {
	setOfflineGoHome(t)

	gopath := t.TempDir()
	const mod = "github.com/example/plain"
	repoDir := filepath.Join(gopath, "src", mod)
	for name, content := range map[string]string{
		"go.mod":      "module " + mod + "\n\ngo 1.24\n\nrequire github.com/Azure/azure-sdk-for-go/sdk/azcore v1.22.0\n",
		"lib.go":      "package plain\nimport _ \"github.com/Azure/azure-sdk-for-go/sdk/azcore\"\n",
		"lib_test.go": "package plain_test\nimport _ \"github.com/stretchr/testify/assert\"\n",
	} {
		aggregateWriteFile(t, filepath.Join(repoDir, name), []byte(content))
	}

	u := &upstream{modulePath: mod}
	if err := u.findDependencies(gopath, mod); err != nil {
		t.Fatalf("findDependencies() error = %v", err)
	}
	if u.exactModules != nil {
		t.Fatalf("exactModules = %v, want nil when go.mod has no replace directives", u.exactModules)
	}
	gotBuild := convertDependenciesToRPM(u.repoDeps, u.exactModules)
	wantBuild := []string{"go(github.com/Azure/azure-sdk-for-go)", "go(github.com/stretchr/testify)"}
	if !slices.Equal(gotBuild, wantBuild) {
		t.Fatalf("BuildRequires = %v, want %v", gotBuild, wantBuild)
	}
	gotRun := convertDependenciesToRPM(u.repoRunDeps, u.exactModules)
	wantRun := []string{"go(github.com/Azure/azure-sdk-for-go)"}
	if !slices.Equal(gotRun, wantRun) {
		t.Fatalf("Requires = %v, want %v", gotRun, wantRun)
	}
}

func TestFindDependenciesRejectsUnsupportedReplaces(t *testing.T) {
	setOfflineGoHome(t)

	for _, tt := range []struct {
		name  string
		gomod string
		code  string
	}{
		{
			name:  "local-parent-path",
			gomod: "module example.com/app\n\ngo 1.24\n\nrequire github.com/a/b v1.0.0\n\nreplace github.com/a/b => ../sibling\n",
			code:  "package app\nimport _ \"github.com/a/b\"\n",
		},
		{
			name:  "noncanonical-branch-version",
			gomod: "module example.com/app\n\ngo 1.24\n\nrequire github.com/a/b v1.0.0\n\nreplace github.com/a/b => github.com/c/d main\n",
			code:  "package app\nimport _ \"github.com/a/b\"\n",
		},
		{
			name:  "noncanonical-short-semver",
			gomod: "module example.com/app\n\ngo 1.24\n\nrequire github.com/a/b v1.0.0\n\nreplace github.com/a/b => github.com/c/d v1.2\n",
			code:  "package app\nimport _ \"github.com/a/b\"\n",
		},
		{
			name:  "unresolved-replace-without-require",
			gomod: "module example.com/app\n\ngo 1.24\n\nreplace github.com/a/b => ./local\n",
			code:  "package app\nimport _ \"github.com/a/b/sub\"\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gopath := t.TempDir()
			repoDir := filepath.Join(gopath, "src", "example.com/app")
			aggregateWriteFile(t, filepath.Join(repoDir, "go.mod"), []byte(tt.gomod))
			aggregateWriteFile(t, filepath.Join(repoDir, "app.go"), []byte(tt.code))

			u := &upstream{modulePath: "example.com/app"}
			if err := u.findDependencies(gopath, "example.com/app"); err == nil {
				t.Fatalf("findDependencies() succeeded for %s, want error", tt.name)
			}
		})
	}
}

func TestFindDependenciesSkipsReplaceOutsideModuleRoot(t *testing.T) {
	setOfflineGoHome(t)

	for _, tt := range []struct {
		name  string
		files map[string]string
	}{
		{
			name: "no-go-mod",
			files: map[string]string{
				"app.go": "package app\nimport _ \"github.com/a/b\"\n",
			},
		},
		{
			name: "root-go-mod-declares-other-module",
			files: map[string]string{
				"go.mod": "module example.com\n\ngo 1.24\n\nrequire github.com/a/b v1.0.0\n\nreplace github.com/a/b => ./local\n",
				"app.go": "package app\nimport _ \"github.com/a/b\"\n",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gopath := t.TempDir()
			repoDir := filepath.Join(gopath, "src", "example.com/app")
			for name, content := range tt.files {
				aggregateWriteFile(t, filepath.Join(repoDir, name), []byte(content))
			}

			u := &upstream{modulePath: "example.com/app"}
			if err := u.findDependencies(gopath, "example.com/app"); err != nil {
				t.Fatalf("findDependencies() error = %v", err)
			}
			if u.exactModules != nil {
				t.Fatalf("exactModules = %v, want nil", u.exactModules)
			}
		})
	}
}

func TestPackConsumerWithModuleReplaceEndToEnd(t *testing.T) {
	setOfflineGoHome(t)

	const app = "github.com/go2spec-fixtures/picoclaw"
	const orig = "github.com/bwmarrin/discordgo"
	const fork = "github.com/yeongaori/discordgo-fork"
	repo, _ := createTestGitRepo(t, map[string]string{
		"go.mod": strings.Join([]string{
			"module " + app,
			"",
			"go 1.24",
			"",
			"require " + orig + " v0.29.0",
			"",
			"replace " + orig + " => " + fork + " v0.0.0-20260319072544-e8e546f5d532",
			"",
		}, "\n"),
		"bot.go":    "package picoclaw\nimport (\n\t_ \"" + orig + "\"\n\t_ \"" + orig + "/voice\"\n)\n",
		"LICENSE":   "MIT\n",
		"README.md": "Bot client using replaced DiscordGo fork.\n",
	}, "v0.1.0")
	archive := runGit(t, repo, nil, "archive", "--format=tar.gz", "--prefix=picoclaw-0.1.0/", "v0.1.0")
	installGitInsteadOfWrapper(t, "https://"+app, repo)

	oldHTTPClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "https://"+app+"/archive/v0.1.0.tar.gz" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(archive)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldHTTPClient })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/package/") {
			_, _ = io.WriteString(w, `{"path":"`+app+`","modulePath":"`+app+`","version":"v0.1.0","name":"picoclaw","synopsis":"Bot client"}`)
			return
		}
		_, _ = io.WriteString(w, `{"path":"`+app+`","version":"v0.1.0","repoUrl":"https://`+app+`","readme":{"filepath":"README.md","contents":"Bot client."},"licenses":[{"filePath":"LICENSE","types":["MIT"]}]}`)
	}))
	t.Cleanup(srv.Close)
	oldBase, oldClient, oldCache := pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache
	pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache = srv.URL, srv.Client(), make(map[string]*pkgsiteInfo)
	t.Cleanup(func() {
		pkgsiteAPIBase, pkgsiteHTTPClient, pkgsiteInfoCache = oldBase, oldClient, oldCache
	})

	t.Chdir(t.TempDir())
	if code := mainPack([]string{"-type", "library", app + "@v0.1.0"}, nil); code != 0 {
		t.Fatalf("mainPack() = %d, want 0", code)
	}
	const rpmName = "go-github-go2spec-fixtures-picoclaw"
	data, err := os.ReadFile(filepath.Join(rpmName, rpmName+".spec"))
	if err != nil {
		t.Fatal(err)
	}
	spec := string(data)
	if strings.Count(spec, "BuildRequires:  go("+fork+")\n") != 1 {
		t.Fatalf("expected single BuildRequires for %s:\n%s", fork, spec)
	}
	if strings.Count(spec, "Requires:       go("+fork+")\n") != 1 {
		t.Fatalf("expected single Requires for %s:\n%s", fork, spec)
	}
	if strings.Contains(spec, "go("+orig+")") || strings.Contains(spec, "go("+orig+"/") {
		t.Fatalf("spec unexpectedly references original replaced module %s:\n%s", orig, spec)
	}
}
