package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/net/publicsuffix"
)

type packageType int

const (
	typeGuess packageType = iota
	typeLibrary
	typeProgram
	typeLibraryProgram
	typeProgramLibrary
)

// upstream describes the upstream repo we are about to package.
type upstream struct {
	repoURL      string
	modulePath   string
	importPath   string        // module directive from source when it differs or pkgsite cannot locate it
	metadata     *specMetadata // metadata read from local source checkout when pkgsite is not used
	tarPath      string        // path to the downloaded or generated orig tarball tempfile
	compression  string        // compression method, either "gz" or "xz"
	version      string        // upstream version number, e.g. 0.0~git20180204.1d24609
	commitID     string        // full commit hash for commit-pinned snapshot builds
	tag          string        // Latest upstream tag, if any
	commitIsh    string        // commit-ish corresponding to upstream version to be packaged
	remote       string        // git remote, set to short hostname if upstream git history is included
	firstMain    string        // import path of the first main package within repo, if any
	packageName  string        // package name from pkgsite or the downloaded source
	vendorDirs   []string      // all vendor sub directories, relative to the repo directory
	repoDeps     []string      // all non-stdlib imports needed for build or tests
	repoRunDeps  []string      // non-stdlib imports needed by normal builds, excluding test-only imports
	repoTestDeps []string      // non-stdlib imports needed only by tests
	hasGodeps    bool          // whether the Godeps/_workspace directory exists
	hasRelease   bool          // whether any release tags exist, for debian/watch
	isRelease    bool          // whether what we end up packaging is a tagged release
	sha256       string        // SHA256 checksum of the source tarball
}

var errUnsupportedHoster = errors.New("unsupported hoster")

type packRequest struct {
	arg      string
	gopkg    string
	revision string
	prepared *upstream
}

type packOptions struct {
	forcePrerelease    bool
	pkgType            packageType
	customProgPkgName  string
	allowUnknownHoster bool
	providesPath       string
	conflicts          []string
	repoURL            string
}

func passthroughEnv() []string {
	var relevantVariables = []string{
		"HOME",
		"PATH",
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy",
		"NO_PROXY", "no_proxy",
		"GIT_PROXY_COMMAND",
		"GIT_HTTP_PROXY_AUTHMETHOD",
	}
	var result []string
	for _, variable := range relevantVariables {
		if value, ok := os.LookupEnv(variable); ok {
			result = append(result, fmt.Sprintf("%s=%s", variable, value))
		}
	}
	return result
}

// findVendorDirs walks the directory tree rooted at dir and returns
// all vendor/ directories found, relative to dir.
func findVendorDirs(dir string) ([]string, error) {
	var vendorDirs []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info != nil && !info.IsDir() {
			return nil // nothing to do for anything but directories
		}
		if info.Name() == ".git" ||
			info.Name() == ".hg" ||
			info.Name() == ".bzr" {
			return filepath.SkipDir
		}
		if info.Name() == "vendor" {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return fmt.Errorf("filepath.Rel: %w", err)
			}
			vendorDirs = append(vendorDirs, rel)
		}
		return nil
	})
	return vendorDirs, err
}

func downloadFile(filename, url string) error {
	dst, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer dst.Close()

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("response: %s", resp.Status)
	}

	_, err = io.Copy(dst, resp.Body)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}

	return nil
}

// fetchText fetches the content of a URL and returns it as a string.
// Returns an error if the HTTP status is not 200.
func fetchText(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("response: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(body), nil
}

// get downloads the source module into the provided GOPATH,
// checking out the specified revision if non-empty.
func (u *upstream) get(gopath, sourceRepo, rev string) error {
	done := make(chan struct{})
	defer close(done)
	go progressSize("git clone", filepath.Join(gopath, "src"), done)

	dir := filepath.Join(gopath, "src", sourceRepo)
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return fmt.Errorf("mkdir clone parent: %w", err)
	}

	cmd := exec.Command("git", "clone", u.repoURL, dir)
	cmd.Env = passthroughEnv()
	cmd.Stderr = os.Stderr
	log.Println("get: Running", cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	if rev != "" {
		checkoutRev := rev
		if module.IsPseudoVersion(rev) {
			var err error
			checkoutRev, err = module.PseudoVersionRev(rev)
			if err != nil {
				return fmt.Errorf("parse pseudo-version %q: %w", rev, err)
			}
		}
		cmd = exec.Command("git", "-c", "advice.detachedHead=false", "checkout", checkoutRev)
		cmd.Dir = dir
		cmd.Env = passthroughEnv()
		cmd.Stderr = os.Stderr
		log.Println("get: Running", cmd, "in", cmd.Dir)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git checkout %q: %w", checkoutRev, err)
		}
	}
	return nil
}

func guessedPackageType(u *upstream) packageType {
	if u.packageName == "main" || (u.packageName == "" && u.firstMain != "") {
		return typeProgram
	}
	return typeLibrary
}

// sourceImportPathForPackage returns the module root used for source checkout
// and tarball generation, while callers keep the requested import path for
// naming and go_import_path.
func sourceImportPathForPackage(requestedPath string, info *pkgsiteInfo) string {
	if info != nil && info.Package.ModulePath != "" {
		return info.Package.ModulePath
	}
	if info != nil && info.Module.Path != "" {
		return info.Module.Path
	}
	return requestedPath
}

// gitCloneURLFromRepoURL converts repository URLs returned by pkg.go.dev into
// something git clone understands. pkg.go.dev reports golang.org/x/* modules
// with a cs.opensource.google browser URL, so rewrite those to go.googlesource.com.
func gitCloneURLFromRepoURL(repoURL string) string {
	repoURL = strings.TrimSuffix(strings.TrimSpace(repoURL), ".git")
	u, err := url.Parse(repoURL)
	if err != nil || u.Host != "cs.opensource.google" {
		return repoURL
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 3 && parts[0] == "go" && parts[1] == "x" {
		return "https://go.googlesource.com/" + parts[2]
	}
	return repoURL
}

func (u *upstream) tarballURLForRef(ref, compression string) (string, error) {
	repo := strings.TrimSuffix(u.repoURL, ".git")
	if repo == "" {
		return "", fmt.Errorf("repository URL is empty")
	}
	repoU, err := url.Parse(repo)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}

	switch repoU.Host {
	case "github.com":
		return fmt.Sprintf("%s/archive/%s.tar.%s",
			repo, ref, compression), nil
	case "gitlab.com", "salsa.debian.org":
		parts := strings.Split(repoU.Path, "/")
		if len(parts) < 3 {
			return "", fmt.Errorf("incomplete repo URL: %s", u.repoURL)
		}
		project := strings.TrimSuffix(parts[len(parts)-1], ".git")
		return fmt.Sprintf("%s/-/archive/%s/%s-%s.tar.%s",
			repo, ref, project, ref, compression), nil
	case "git.sr.ht":
		return fmt.Sprintf("%s/archive/%s.tar.%s",
			repo, ref, compression), nil
	case "codeberg.org":
		return fmt.Sprintf("%s/archive/%s.tar.%s",
			repo, ref, compression), nil
	default:
		return "", errUnsupportedHoster
	}
}

func (u *upstream) tarballUrl() (string, error) {
	return u.tarballURLForRef(u.tag, u.compression)
}

func moduleProxyEscapedPath(modulePath string) string {
	parts := strings.Split(strings.Trim(modulePath, "/"), "/")
	for i, part := range parts {
		parts[i] = moduleProxyEscapeString(part)
	}
	return strings.Join(parts, "/")
}

// moduleProxyEscapeString applies the case-escape required by the Go module
// proxy protocol: uppercase letters become "!" followed by lowercase.
// See https://go.dev/ref/mod#goproxy-protocol.
func moduleProxyEscapeString(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return strings.ReplaceAll(url.PathEscape(b.String()), "%21", "!")
}

// moduleProxyVersionForSpec leaves RPM macros unchanged and otherwise applies
// module proxy escaping so versions like v1.0.0-RC1 become v1.0.0-!r!c1.
func moduleProxyVersionForSpec(version string) string {
	if strings.Contains(version, "%{") {
		return version
	}
	return moduleProxyEscapeString(version)
}

func repoRootImportPath(repoURL string) string {
	u, err := url.Parse(strings.TrimSuffix(repoURL, ".git"))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch u.Host {
	case "github.com", "gitlab.com", "codeberg.org", "salsa.debian.org":
		if len(parts) >= 2 {
			return u.Host + "/" + strings.Join(parts[:2], "/")
		}
	}
	return ""
}

// moduleUsesRepoSubdir reports whether modulePath lives in a repository
// subdirectory. A trailing /vN semantic import version suffix is treated as
// part of the repo-root module path, not as a subdirectory.
func moduleUsesRepoSubdir(modulePath, repoURL string) bool {
	root := repoRootImportPath(repoURL)
	if root == "" || !strings.HasPrefix(modulePath, root+"/") {
		return false
	}
	rest := strings.TrimPrefix(modulePath, root+"/")
	return !regexp.MustCompile(`^v\d+$`).MatchString(rest)
}

// sourceRefForSpec picks the git ref for Source0, preferring RPM macros when
// the upstream version maps cleanly to %{commit_id} or %{version}.
func (u *upstream) sourceRefForSpec() (string, error) {
	ref := u.tag
	if u.commitID != "" {
		ref = "%{commit_id}"
	} else if u.isRelease && u.tag == "v"+u.version {
		ref = "v%{version}"
	} else if u.isRelease && u.tag == u.version {
		ref = "%{version}"
	} else if ref == "" {
		ref = u.commitIsh
	}
	if ref == "" {
		return "", fmt.Errorf("source reference is empty")
	}
	return ref, nil
}

// sourceURLForSpec returns the Source0 URL for the spec file. It prefers
// hoster tarballs for repo-root modules, and uses proxy.golang.org zips for
// repo subdirectories or unsupported hosters. It returns an error for
// commit-pinned subdirectory modules because the proxy requires a canonical
// pseudo-version.
func (u *upstream) sourceURLForSpec(modulePath string) (string, error) {
	ref, err := u.sourceRefForSpec()
	if err != nil {
		return "", err
	}
	if moduleUsesRepoSubdir(modulePath, u.repoURL) {
		if strings.Contains(ref, "commit_id") {
			return "", fmt.Errorf("module proxy Source0 for commit-pinned subdirectory module %q requires a canonical pseudo-version", modulePath)
		}
		return fmt.Sprintf("https://proxy.golang.org/%s/@v/%s.zip#/%%{_name}-%%{version}.zip",
			moduleProxyEscapedPath(modulePath), moduleProxyVersionForSpec(ref)), nil
	}
	tarURL, err := u.tarballURLForRef(ref, "gz")
	if err == nil {
		return fmt.Sprintf("%s#/%%{_name}-%%{version}.tar.gz", tarURL), nil
	}
	if !errors.Is(err, errUnsupportedHoster) || modulePath == "" || strings.Contains(ref, "commit_id") {
		return "", err
	}
	return fmt.Sprintf("https://proxy.golang.org/%s/@v/%s.zip#/%%{_name}-%%{version}.zip",
		moduleProxyEscapedPath(modulePath), moduleProxyVersionForSpec(ref)), nil
}

func (u *upstream) tarballFromHoster() error {
	tarURL, err := u.tarballUrl()
	if err != nil {
		return err
	}

	done := make(chan struct{})
	go progressSize("Download", u.tarPath, done)

	log.Printf("Downloading %s", tarURL)
	err = downloadFile(u.tarPath, tarURL)

	close(done)

	if err != nil {
		return err
	}

	u.sha256 = u.fetchChecksumFromReleasePage()
	if u.sha256 == "" {
		sum, err := fileSHA256(u.tarPath)
		if err != nil {
			log.Printf("INFO: Could not compute source tarball sha256: %v", err)
		} else {
			u.sha256 = sum
		}
	}

	return nil
}

// fetchChecksumFromReleasePage fetches the GitHub/GitLab release page and
// attempts to extract a SHA-256 checksum for the source tarball from the
// page content. Some projects include the checksum in the release body
// markdown, e.g. "sha256: <hex>" or "SHA256: <hex>".
func (u *upstream) fetchChecksumFromReleasePage() string {
	releaseURL := u.releasePageURL()
	if releaseURL == "" {
		return ""
	}

	log.Printf("Fetching release page %s for checksum", releaseURL)
	content, err := fetchText(releaseURL)
	if err != nil {
		log.Printf("INFO: Could not fetch release page: %v", err)
		return ""
	}

	return extractSHA256FromHTML(content, u.tag)
}

// releasePageURL constructs the release page URL for the current tag.
// For GitHub: https://github.com/{owner}/{repo}/releases/tag/{tag}
// For GitLab: https://gitlab.com/{owner}/{repo}/-/releases/{tag}
func (u *upstream) releasePageURL() string {
	repo := strings.TrimSuffix(u.repoURL, ".git")
	if repo == "" || u.tag == "" {
		return ""
	}
	repoU, err := url.Parse(repo)
	if err != nil {
		return ""
	}

	switch repoU.Host {
	case "github.com":
		return fmt.Sprintf("%s/releases/tag/%s", repo, u.tag)
	case "gitlab.com", "salsa.debian.org":
		return fmt.Sprintf("%s/-/releases/%s", repo, u.tag)
	default:
		return ""
	}
}

// extractSHA256FromHTML searches HTML content for a SHA-256 checksum
// associated with the tarball for the given tag. It looks for patterns like:
//
//	sha256: <64-hex-chars>
//	SHA256: <64-hex-chars>
//	sha256sum: <64-hex-chars>
//
// It also tries to match checksums near tarball-related filenames.
func extractSHA256FromHTML(html, tag string) string {
	lower := strings.ToLower(html)

	patterns := []*regexp.Regexp{
		regexp.MustCompile(`sha-?256(?:sum)?[:\s]+([a-f0-9]{64})`),
		regexp.MustCompile(`sha-?256(?:sum)?[:\s]*([a-f0-9]{64})`),
	}

	for _, re := range patterns {
		if m := re.FindStringSubmatch(lower); len(m) > 1 && isValidSHA256(m[1]) {
			return m[1]
		}
	}

	// Fallback: look for a 64-char hex string near "tar" or "source" keywords
	tarRe := regexp.MustCompile(`(?:tar|source)[^a-f0-9]{0,80}([a-f0-9]{64})`)
	if m := tarRe.FindStringSubmatch(lower); len(m) > 1 {
		if isValidSHA256(m[1]) {
			return m[1]
		}
	}
	revTarRe := regexp.MustCompile(`([a-f0-9]{64})[^a-f0-9]{0,80}(?:tar|source)`)
	if m := revTarRe.FindStringSubmatch(lower); len(m) > 1 {
		if isValidSHA256(m[1]) {
			return m[1]
		}
	}

	return ""
}

// isValidSHA256 reports whether s is a valid lowercase hexadecimal string
// of exactly 64 characters (the length of a SHA-256 digest).
func isValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (u *upstream) tar(gopath, repo string) error {
	f, err := os.CreateTemp("", "pack-tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	u.tarPath = f.Name()
	f.Close()

	if u.isRelease {
		if u.hasGodeps {
			log.Printf("Godeps/_workspace exists, not downloading tarball from hoster.")
		} else {
			u.compression = "gz"
			if err := u.tarballFromHoster(); err == nil {
				return nil
			} else if err == errUnsupportedHoster {
				log.Printf("INFO: Hoster does not provide release tarball\n")
			} else {
				log.Printf("WARNING: Could not download release tarball from hoster, falling back to local git archive: %v\n", err)
			}
		}
	}

	u.compression = "xz"
	base := filepath.Base(repo)
	log.Printf("Generating temp tarball as %q\n", u.tarPath)
	dir := filepath.Dir(repo)
	cmd := exec.Command(
		"tar",
		"cJf",
		u.tarPath,
		"--exclude=.git",
		"--exclude=Godeps/_workspace",
		base)
	cmd.Dir = filepath.Join(gopath, "src", dir)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// findMains finds main packages within the repo (useful to auto-detect the
// package type).
func (u *upstream) findMains(gopath, repo string) error {
	pattern := repo
	if u.importPath != "" {
		pattern = "."
	}
	cmd := exec.Command("go", "list", "-e", "-f", "{{.ImportPath}} {{.Name}}", pattern+"/...")
	cmd.Dir = filepath.Join(gopath, "src", repo)
	cmd.Env = passthroughEnv()
	cmd.Stderr = os.Stderr
	log.Println("findMains: Running", cmd, "in", cmd.Dir)
	out, err := cmd.Output()
	if err != nil {
		log.Println("WARNING: In findMains:", fmt.Errorf("%q: %w", cmd.Args, err))
		log.Printf("Retrying without appending \"/...\" to repo")
		cmd = exec.Command("go", "list", "-e", "-f", "{{.ImportPath}} {{.Name}}", pattern)
		cmd.Dir = filepath.Join(gopath, "src", repo)
		cmd.Env = passthroughEnv()
		cmd.Stderr = os.Stderr
		log.Println("findMains: Running", cmd, "in", cmd.Dir)
		out, err = cmd.Output()
		if err != nil {
			log.Println("WARNING: In findMains:", fmt.Errorf("%q: %w", cmd.Args, err))
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(line, "/vendor/") ||
			strings.Contains(line, "/Godeps/") ||
			strings.Contains(line, "/samples/") ||
			strings.Contains(line, "/examples/") ||
			strings.Contains(line, "/example/") {
			continue
		}
		if strings.HasSuffix(line, " main") {
			u.firstMain = strings.TrimSuffix(line, " main")
			break
		}
	}
	return nil
}

type goListPackage struct {
	ImportPath   string
	Name         string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Error        *struct {
		Err string
	}
}

func shouldIgnoreGoDependency(p, repo string) bool {
	if p == "" {
		return true
	}
	// Strip packages that are included in the repository we are packaging.
	if strings.HasPrefix(p, repo+"/") || p == repo {
		return true
	}
	if p == "C" {
		// TODO: maybe parse the comments to figure out C deps from pkg-config files?
		return true
	}
	return false
}

func addGoDependencies(deps map[string]bool, repo string, imports []string) {
	for _, p := range imports {
		if shouldIgnoreGoDependency(p, repo) {
			continue
		}
		deps[p] = true
	}
}

func sortedDependencySet(deps map[string]bool) []string {
	result := make([]string, 0, len(deps))
	for dep := range deps {
		result = append(result, dep)
	}
	sort.Strings(result)
	return result
}

// setRepoDependencies stores three views of imports: repoDeps is the union for
// BuildRequires, repoRunDeps is non-test imports for library Requires, and
// repoTestDeps is imports used only by tests.
func (u *upstream) setRepoDependencies(runtimeDeps, testDeps map[string]bool) {
	buildDeps := make(map[string]bool, len(runtimeDeps)+len(testDeps))
	for dep := range runtimeDeps {
		buildDeps[dep] = true
	}
	testOnlyDeps := make(map[string]bool)
	for dep := range testDeps {
		buildDeps[dep] = true
		if !runtimeDeps[dep] {
			testOnlyDeps[dep] = true
		}
	}

	u.repoDeps = sortedDependencySet(buildDeps)
	u.repoRunDeps = sortedDependencySet(runtimeDeps)
	u.repoTestDeps = sortedDependencySet(testOnlyDeps)
}

func (u *upstream) findDependencies(gopath, repo string) error {
	log.Printf("Determining dependencies\n")

	pattern, importPath := repo, repo
	if u.importPath != "" {
		pattern, importPath = ".", u.importPath
	}
	cmd := exec.Command("go", "list", "-e", "-json", pattern+"/...")
	cmd.Dir = filepath.Join(gopath, "src", repo)
	cmd.Env = passthroughEnv()
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		log.Println("WARNING: In findDependencies:", fmt.Errorf("%q: %w", cmd.Args, err))
		// See https://bugs.debian.org/992610
		log.Printf("Retrying without appending \"/...\" to repo")
		cmd = exec.Command("go", "list", "-e", "-json", pattern)
		cmd.Dir = filepath.Join(gopath, "src", repo)
		cmd.Env = passthroughEnv()
		cmd.Stderr = os.Stderr
		out, err = cmd.Output()
		if err != nil {
			return fmt.Errorf("go list dependencies: %q: %w", cmd.Args, err)
		}
	}

	runtimeDeps := make(map[string]bool)
	testDeps := make(map[string]bool)
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var pkg goListPackage
		err := dec.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode go list dependency output: %w", err)
		}
		if pkg.Error != nil && pkg.Error.Err != "" {
			log.Printf("WARNING: go list reported package load error for %q: %s\n", pkg.ImportPath, pkg.Error.Err)
		}
		if u.importPath != "" && pkg.ImportPath == u.importPath {
			u.packageName = pkg.Name
		}
		addGoDependencies(runtimeDeps, importPath, pkg.Imports)
		addGoDependencies(testDeps, importPath, pkg.TestImports)
		addGoDependencies(testDeps, importPath, pkg.XTestImports)
	}

	if len(runtimeDeps) == 0 && len(testDeps) == 0 {
		return nil
	}

	// Remove all packages which are in the standard lib.
	cmd = exec.Command("go", "list", "std")
	cmd.Stderr = os.Stderr
	cmd.Env = passthroughEnv()

	out, err = cmd.Output()
	if err != nil {
		return fmt.Errorf("go list std: (args: %v): %w", cmd.Args, err)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		delete(runtimeDeps, line)
		delete(testDeps, line)
	}

	u.setRepoDependencies(runtimeDeps, testDeps)

	return nil
}

// makeUpstreamSourceTarball downloads the source module from the Internet,
// checks out the specified revision, determines the version number, removes
// vendored dependencies, and creates a tarball of the upstream source code.
// It also discovers main packages and dependencies, and may check out a release
// tag different from the requested revision when pkgVersionFromGit selects one.
// The caller must remove the returned tarPath after copying it.
func makeUpstreamSourceTarball(requestedPath, revision string, forcePrerelease bool, info *pkgsiteInfo, explicitRepoURL string) (*upstream, error) {
	sourceRepo := sourceImportPathForPackage(requestedPath, info)
	u := &upstream{modulePath: sourceRepo}
	if info == nil {
		if err := module.CheckPath(requestedPath); err != nil {
			return nil, err
		}
		if explicitRepoURL != "" {
			u.repoURL = gitCloneURLFromRepoURL(explicitRepoURL)
		} else {
			repoPath, _, _ := module.SplitPathVersion(requestedPath)
			u.repoURL = gitCloneURLFromRepoURL("https://" + repoPath)
		}
	} else {
		u.repoURL = gitCloneURLFromRepoURL(info.Module.RepoURL)
		u.packageName = info.Package.Name
	}

	gopath, err := os.MkdirTemp("", "pack-tmp")
	if err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}
	defer os.RemoveAll(gopath)
	repoDir := filepath.Join(gopath, "src", sourceRepo)

	log.Printf("Downloading %q\n", sourceRepo+"/...")
	if err := u.get(gopath, sourceRepo, revision); err != nil {
		return nil, fmt.Errorf("get source: %w", err)
	}

	// Verify early this repository uses git (we call pkgVersionFromGit later):
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); os.IsNotExist(err) {
		return nil, fmt.Errorf("not a git repository; This program currently only supports git")
	}

	u.vendorDirs, err = findVendorDirs(repoDir)
	if err != nil {
		return nil, fmt.Errorf("find vendor dirs: %w", err)
	}
	if len(u.vendorDirs) > 0 {
		log.Printf("Deleting upstream vendor/ directories")
		for _, dir := range u.vendorDirs {
			if err := os.RemoveAll(filepath.Join(repoDir, dir)); err != nil {
				return nil, fmt.Errorf("remove all: %w", err)
			}
		}
	}

	if _, err := os.Stat(filepath.Join(repoDir, "Godeps", "_workspace")); !os.IsNotExist(err) {
		log.Println("Godeps/_workspace detected")
		u.hasGodeps = true
	}

	log.Printf("Determining upstream version number\n")

	preferredRevision := revision
	if preferredRevision == "" && info != nil {
		preferredRevision = info.Package.Version
		log.Printf("Using pkg.go.dev latest version %q as the preferred release", preferredRevision)
	}
	u.version, err = pkgVersionFromGit(repoDir, u, preferredRevision, forcePrerelease)
	if err != nil {
		return nil, fmt.Errorf("get package version from Git: %w", err)
	}

	log.Printf("Package version is %q\n", u.version)

	if u.isRelease && u.tag != "" {
		// pkgVersionFromGit may select a release tag different from the revision
		// passed by the user; dependency and tarball discovery must use that tree.
		cmd := exec.Command("git", "-c", "advice.detachedHead=false", "checkout", u.tag)
		cmd.Dir = repoDir
		cmd.Env = passthroughEnv()
		cmd.Stderr = os.Stderr
		log.Println("makeUpstreamSourceTarball: Running", cmd, "in", cmd.Dir)
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git checkout release tag %q: %w", u.tag, err)
		}
	}

	if info == nil {
		data, err := os.ReadFile(filepath.Join(repoDir, "go.mod"))
		if err != nil {
			return nil, fmt.Errorf("read source go.mod: %w", err)
		}
		u.importPath = modfile.ModulePath(data)
		if err := module.CheckPath(u.importPath); err != nil {
			return nil, fmt.Errorf("source module path: %w", err)
		}
	}

	if err := u.findMains(gopath, sourceRepo); err != nil {
		return nil, fmt.Errorf("find mains: %w", err)
	}

	if err := u.findDependencies(gopath, sourceRepo); err != nil {
		return nil, fmt.Errorf("find dependencies: %w", err)
	}

	if info == nil {
		m := readSpecMetadata(requestedPath, repoDir)
		u.metadata = &m
	}

	if err := u.tar(gopath, sourceRepo); err != nil {
		return nil, fmt.Errorf("tar: %w", err)
	}

	return u, nil
}

func createDirectory(openRuyiSrc string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get cwd: %w", err)
	}
	dir := filepath.Join(wd, openRuyiSrc)

	// Try to create the directory
	err = os.Mkdir(dir, 0755)
	if err != nil {
		// If directory already exists, that's ok (it was verified to be empty)
		if !os.IsExist(err) {
			return "", fmt.Errorf("mkdir: %w", err)
		}
	}
	return dir, nil
}

// Package names (both source and binary, see Package, Section 5.6.7) must
// consist only of lower case letters (a-z), digits (0-9), plus (+) and minus
// (-) signs, and periods (.). They must be at least two characters long and
// must start with an alphanumeric character.
func normalizePackageName(str string) string {
	lowerDigitPlusMinusDot := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z' || '0' <= r && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r == '.' || r == '+' || r == '-':
			return r
		case r == '_':
			return '-'
		}
		return -1
	}

	safe := strings.Trim(strings.Map(lowerDigitPlusMinusDot, str), "-")
	if len(safe) < 2 {
		return "TODO"
	}

	return safe
}

// shortHostName maps a Go package import path to a canonical short hostname
func shortHostName(gopkg string, allowUnknownHoster bool) (host string, err error) {
	knownHosts := map[string]string{
		// keep the list in alphabetical order
		"bazil.org":            "bazil",
		"bitbucket.org":        "bitbucket",
		"blitiri.com.ar":       "blitiri",
		"cloud.google.com":     "googlecloud",
		"code.google.com":      "googlecode",
		"codeberg.org":         "codeberg",
		"filippo.io":           "filippo",
		"fortio.org":           "fortio",
		"fyne.io":              "fyne",
		"git.sr.ht":            "sourcehut",
		"github.com":           "github",
		"gitlab.com":           "gitlab",
		"go.bug.st":            "bugst",
		"go.cypherpunks.ru":    "cypherpunks",
		"go.yaml.in":           "yaml",
		"go.mongodb.org":       "mongodb",
		"go.opentelemetry.io":  "opentelemetry",
		"go.step.sm":           "step",
		"go.uber.org":          "uber",
		"go4.org":              "go4",
		"gocloud.dev":          "gocloud",
		"golang.org":           "golang",
		"google.golang.org":    "google",
		"gopkg.in":             "gopkg",
		"honnef.co":            "honnef",
		"howett.net":           "howett",
		"k8s.io":               "k8s",
		"modernc.org":          "modernc",
		"pault.ag":             "pault",
		"rsc.io":               "rsc",
		"salsa.debian.org":     "debian",
		"sigs.k8s.io":          "k8s-sigs",
		"software.sslmate.com": "sslmate",
		"zgo.at":               "zgoat",
	}
	parts := strings.Split(gopkg, "/")
	fqdn := parts[0]
	if host, ok := knownHosts[fqdn]; ok {
		return host, nil
	}
	if !allowUnknownHoster {
		return "", fmt.Errorf("unknown hoster %q", fqdn)
	}
	suffix, _ := publicsuffix.PublicSuffix(fqdn)
	// 防止 panic: runtime error: slice bounds out of range [:-1]
	// 如果 fqdn 长度不足以包含 suffix 和一个点号（例如 "localhost" 或 "yaml"），则直接使用 fqdn
	if len(fqdn) <= len(suffix)+len(".") {
		log.Printf("WARNING: Cannot strip suffix %q from %q (too short), using %q as hostname.", suffix, fqdn, fqdn)
		return fqdn, nil
	}
	host = fqdn[:len(fqdn)-len(suffix)-len(".")]
	log.Printf("WARNING: Using %q as canonical hostname for %q. If that is not okay, please file a bug against %s.\n", host, fqdn, os.Args[0])
	return host, nil
}

// nameFromGopkg maps a Go package import path to a openRuyi package name.
// e.g. "golang.org/x/text" → "go-golang-x-text".
// This follows https://fedoraproject.org/wiki/PackagingDrafts/Go#Package_Names
func nameFromGopkg(gopkg string, t packageType, customProgPkgName string, allowUnknownHoster bool) string {
	parts := strings.Split(gopkg, "/")

	if t == typeProgram || t == typeProgramLibrary {
		if customProgPkgName != "" {
			return normalizePackageName(customProgPkgName)
		}
		return normalizePackageName(parts[len(parts)-1])
	}

	host, err := shortHostName(gopkg, allowUnknownHoster)
	if err != nil {
		log.Fatalf("Cannot derive package name: %v. See -help output for -allow_unknown_hoster\n", err)
	}
	parts[0] = host

	return normalizePackageName("go-" + strings.Join(parts, "-"))
}

func validateRepoURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil ||
		(u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" ||
		u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" ||
		strings.ContainsAny(raw, " \t") {
		return "", fmt.Errorf("-repo_url requires an HTTP(S) Git repository URL without credentials, query, fragment or spaces")
	}
	return raw, nil
}

func copyFile(src, dest string) error {
	input, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer input.Close()

	output, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if _, err := io.Copy(output, input); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return output.Close()
}

// mainPack is the entry point for the "pack" command.
func mainPack(args []string, usage func()) int {
	flagSet := flag.NewFlagSet("pack", flag.ExitOnError)
	if usage != nil {
		flagSet.Usage = usage
	} else {
		flagSet.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage: %s [pack] [FLAG]... <go-package-importpath>[@revision]...\n", os.Args[0])
			fmt.Fprintf(os.Stderr, "       %s [pack] [FLAG]... -list <go.mod>\n", os.Args[0])
			fmt.Fprintf(os.Stderr, "Example: %s pack golang.org/x/oauth2@v0.30.0\n", os.Args[0])
			fmt.Fprintf(os.Stderr, "Example: %s pack -list packages.go.mod\n", os.Args[0])
			fmt.Fprintf(os.Stderr, "\n")
			fmt.Fprintf(os.Stderr, "\"%s pack\" downloads the specified Go packages from the Internet,\nand creates new files and directories in the current working directory.\n", os.Args[0])
			fmt.Fprintf(os.Stderr, "\n")
			fmt.Fprintf(os.Stderr, "Flags:\n")
			flagSet.PrintDefaults()
		}
	}

	var gitRevision string
	flagSet.StringVar(&gitRevision,
		"git_revision",
		"",
		"git revision (see gitrevisions(7)) for package arguments without @revision\n"+
			"to check out, defaulting to the default behavior of git clone.\n"+
			"Useful in case you do not want to package e.g. current HEAD.")

	var listFile string
	flagSet.StringVar(&listFile,
		"list",
		"",
		"Read packages to pack from the require directives of a go.mod file")

	var forcePrerelease bool
	flagSet.BoolVar(&forcePrerelease,
		"force_prerelease",
		false,
		"Package @master or @tip instead of the latest tagged version")

	var pkgTypeString string
	flagSet.StringVar(&pkgTypeString,
		"type",
		"",
		"Set package type, one of:\n"+
			` * "library" (aliases: "lib", "l", "dev")`+"\n"+
			` * "program" (aliases: "prog", "p")`+"\n"+
			` * "library+program" (aliases: "lib+prog", "l+p", "both")`+"\n"+
			` * "program+library" (aliases: "prog+lib", "p+l", "combined")`)

	var customProgPkgName string
	flagSet.StringVar(&customProgPkgName,
		"program_package_name",
		"",
		"Override the program package name, and the source package name too\n"+
			"when appropriate, e.g. to name github.com/cli/cli as \"gh\"")

	var providesPath string
	flagSet.StringVar(&providesPath,
		"provides",
		"",
		"Override the RPM go(import/path) capability for one library package")

	var repoURL string
	flagSet.StringVar(&repoURL,
		"repo_url",
		"",
		"Override the upstream Git repository URL for one package")

	var conflicts stringListFlag
	flagSet.Var(&conflicts,
		"conflicts",
		"Conflicting RPM package name; may be repeated")

	var allowUnknownHoster bool
	flagSet.BoolVar(&allowUnknownHoster,
		"allow_unknown_hoster",
		false,
		"The pkg-go naming conventions use a canonical identifier for\n"+
			"the hostname (see https://go-team.pages.debian.net/packaging.html),\n"+
			"and the mapping is hardcoded into this program.\n"+
			"In case you want to package a Go package living on an unknown hoster,\n"+
			"you may set this flag to true and double-check that the resulting\n"+
			"package name is sane. Contact pkg-go if unsure.")

	var combineRoot, combineName, combineVersion, combineLicense, combineURL string
	flagSet.StringVar(&combineRoot, "combine", "", "Combine explicit module@version sources into one library RPM under this import root")
	flagSet.StringVar(&combineName, "package_name", "", "RPM name for -combine")
	flagSet.StringVar(&combineVersion, "package_version", "", "Combined RPM Version for -combine, independent of module versions")
	flagSet.StringVar(&combineLicense, "license", "", "Reviewed SPDX expression for all -combine sources; otherwise require matching versioned license metadata")
	flagSet.StringVar(&combineURL, "url", "", "Upstream project homepage for -combine")

	// Actual pack starts here

	// Parse flags
	err := flagSet.Parse(args)
	if err != nil {
		log.Fatalf("parse args: %v", err)
	}

	visited := make(map[string]bool)
	flagSet.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if visited["combine"] {
		for _, name := range []string{"git_revision", "force_prerelease", "list", "program_package_name", "allow_unknown_hoster", "provides", "repo_url", "conflicts"} {
			if visited[name] {
				log.Printf("-%s cannot be combined with -combine", name)
				return 1
			}
		}
		if visited["type"] && pkgTypeString != "library" && pkgTypeString != "lib" && pkgTypeString != "l" && pkgTypeString != "dev" {
			log.Print("-combine supports only library source RPMs")
			return 1
		}
		req, err := parseAggregateRequest(combineRoot, combineName, combineVersion, combineLicense, flagSet.Args())
		if err != nil {
			log.Printf("combine: %v", err)
			return 1
		}
		if err := validateProjectURL(combineURL); err != nil {
			log.Printf("combine: %v", err)
			return 1
		}
		req.URL = combineURL
		dir, err := os.Getwd()
		if err != nil {
			log.Print(err)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		output, err := prepareAggregate(ctx, req, dir)
		if err != nil {
			log.Printf("combine: %v", err)
			return 1
		}
		log.Printf("Created %s; review the spec and external dependencies before building", output)
		return 0
	}
	for _, name := range []string{"package_name", "package_version", "license", "url"} {
		if visited[name] {
			log.Printf("-%s requires -combine", name)
			return 1
		}
	}
	if visited["provides"] {
		providesPath = strings.TrimSpace(providesPath)
		if err := module.CheckImportPath(providesPath); err != nil {
			log.Fatalf("invalid -provides path: %v", err)
		}
		if listFile != "" || flagSet.NArg() != 1 {
			log.Fatal("-provides requires exactly one package argument and cannot be combined with -list")
		}
	}
	if visited["repo_url"] {
		var err error
		repoURL, err = validateRepoURL(repoURL)
		if err != nil {
			log.Print(err)
			return 1
		}
		if listFile != "" || flagSet.NArg() != 1 {
			log.Print("-repo_url requires exactly one package argument and cannot be combined with -list")
			return 1
		}
	}
	if visited["conflicts"] {
		for i, name := range conflicts {
			name = strings.TrimSpace(name)
			if !rpmNamePattern.MatchString(name) {
				log.Printf("invalid -conflicts RPM package name %q", name)
				return 1
			}
			conflicts[i] = name
		}
		if listFile != "" || flagSet.NArg() != 1 {
			log.Print("-conflicts requires exactly one package argument and cannot be combined with -list")
			return 1
		}
	}

	// A package argument or a package-list file is required.
	if flagSet.NArg() < 1 && listFile == "" {
		flagSet.Usage()
		os.Exit(1)
	}

	gitRevision = strings.TrimSpace(gitRevision)
	var pkgType packageType

	switch strings.TrimSpace(pkgTypeString) {
	case "", "guess":
		pkgType = typeGuess
	case "library", "lib", "l", "dev":
		pkgType = typeLibrary
	case "program", "prog", "p":
		pkgType = typeProgram
	case "library+program", "lib+prog", "l+p", "both":
		// Example packages: go-github-alecthomas-chroma,
		// go-github-tdewolff-minify, go-github-spf13-viper
		pkgType = typeLibraryProgram
	case "program+library", "prog+lib", "p+l", "combined":
		// Example package: hugo
		pkgType = typeProgramLibrary
	default:
		log.Fatalf("-type=%q not recognized, aborting\n", pkgTypeString)
	}
	if providesPath != "" && pkgType == typeProgram {
		log.Fatal("-provides requires a package type that includes a library")
	}

	// Now we collect all package's info into packRequest structs
	requests, err := collectPackRequests(flagSet.Args(), listFile, gitRevision)
	if err != nil {
		log.Fatalf("parse package list: %v", err)
	}

	opts := packOptions{
		forcePrerelease:    forcePrerelease,
		pkgType:            pkgType,
		customProgPkgName:  customProgPkgName,
		allowUnknownHoster: allowUnknownHoster,
		providesPath:       providesPath,
		repoURL:            repoURL,
		conflicts:          conflicts,
	}

	// Now we verify that all packages are valid
	if err := preflightPackRequests(context.TODO(), requests, opts); err != nil {
		log.Fatalf("preflight package list: %v", err)
	}

	for _, req := range requests {
		if err := packPackage(req.gopkg, req.revision, opts, req.prepared); err != nil {
			cleanupPreparedRequests(requests)
			log.Fatalf("pack %q: %v", req.arg, err)
		}
	}
	return 0
}

func collectPackRequests(args []string, listFile, defaultRevision string) ([]packRequest, error) {
	// If a list file is specified
	// for example, "pack -list packages.go.mod"
	if listFile != "" {
		if len(args) != 0 {
			return nil, fmt.Errorf("-list cannot be combined with package arguments")
		}
		return parsePackListFile(listFile)
	}

	// if no list file is specified
	// for example, "pack golang.org/x/a@v0.30.0 golang.org/x/b@v0.7.0"
	var requests []packRequest
	for _, arg := range args {
		gopkg, revision, err := parsePackArg(arg, defaultRevision)
		if err != nil {
			return nil, fmt.Errorf("parse package argument %q: %w", arg, err)
		}
		requests = append(requests, packRequest{arg: arg, gopkg: gopkg, revision: revision})
	}
	return requests, nil
}

func parsePackListFile(filename string) ([]packRequest, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", filename, err)
	}
	f, err := modfile.Parse(filename, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", filename, err)
	}

	// TODO: Support go.mod directives that affect module version selection,
	// such as replace, exclude, and retract, before accepting them in pack lists.
	if len(f.Replace) > 0 || len(f.Exclude) > 0 || len(f.Retract) > 0 {
		return nil, fmt.Errorf("%q contains unsupported directives", filename)
	}
	if len(f.Require) == 0 {
		return nil, fmt.Errorf("%q contains no require directives", filename)
	}

	requests := make([]packRequest, 0, len(f.Require))
	for _, require := range f.Require {
		// TODO: Decide whether indirect requirements should be included in pack lists.
		if err := module.Check(require.Mod.Path, require.Mod.Version); err != nil {
			return nil, fmt.Errorf("invalid require %q %q: %w",
				require.Mod.Path, require.Mod.Version, err)
		}
		requests = append(requests, packRequest{
			arg:      require.Mod.Path + "@" + require.Mod.Version,
			gopkg:    require.Mod.Path,
			revision: require.Mod.Version,
		})
	}
	return requests, nil
}

func trimRepoScheme(gopkg string) string {
	for _, prefix := range []string{"https://", "http://", "git://"} {
		gopkg = strings.TrimPrefix(gopkg, prefix)
	}
	return gopkg
}

func checkEmptyOutputDir(dir string) error {
	dirInfo, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %q: %w", dir, err)
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %q: %w", dir, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("%q is non-empty", dir)
	}
	return nil
}

func cleanupPreparedRequests(requests []packRequest) {
	for _, req := range requests {
		if req.prepared != nil && req.prepared.tarPath != "" {
			_ = os.Remove(req.prepared.tarPath)
		}
	}
}

// needsGitSourceFallback reports whether a pkgsite lookup result means that
// gopkg is not on pkg.go.dev, so its Git repository must be used directly.
// Any other lookup error is returned as is.
func needsGitSourceFallback(gopkg string, info *pkgsiteInfo, err error) (string, error) {
	var apiErr *pkgsiteAPIError
	switch {
	case err != nil && errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound:
		return fmt.Sprintf("pkgsite does not know %q", gopkg), nil
	case err != nil:
		return "", err
	case info.Module.Path != gopkg && !strings.HasPrefix(gopkg, info.Module.Path+"/"):
		return fmt.Sprintf("pkgsite resolves %q to unrelated module %q", gopkg, info.Module.Path), nil
	case gitCloneURLFromRepoURL(info.Module.RepoURL) == "":
		return fmt.Sprintf("pkgsite module %q has no repository URL", info.Module.Path), nil
	}
	return "", nil
}

func preflightPackRequests(ctx context.Context, requests []packRequest, opts packOptions) (err error) {
	defer func() {
		if err != nil {
			cleanupPreparedRequests(requests)
		}
	}()

	seenOutputDirs := make(map[string]string)
	checkOutputDir := func(req *packRequest, gopkg string, effectivePkgType packageType) error {
		if opts.providesPath != "" && effectivePkgType == typeProgram {
			return fmt.Errorf("%q: -provides requires a package type that includes a library; use -type to select one", req.arg)
		}
		dir := nameFromGopkg(gopkg, effectivePkgType, opts.customProgPkgName, opts.allowUnknownHoster)
		if previous, ok := seenOutputDirs[dir]; ok {
			return fmt.Errorf("%q and %q use %q", previous, req.arg, dir)
		}
		seenOutputDirs[dir] = req.arg
		if err := checkEmptyOutputDir(dir); err != nil {
			return fmt.Errorf("%q: %w", req.arg, err)
		}
		return nil
	}

	// First run the cheap checks for every request, so that a bad entry fails
	// before any repository is cloned.
	type gitFallback struct {
		req    *packRequest
		gopkg  string
		reason string
	}
	var fallbacks []gitFallback
	seenFallbacks := make(map[string]string)
	for i := range requests {
		req := &requests[i]
		gopkg := trimRepoScheme(req.gopkg)
		if opts.repoURL != "" {
			fallbacks = append(fallbacks, gitFallback{req: req, gopkg: gopkg, reason: fmt.Sprintf("using explicit repository URL %s", opts.repoURL)})
			if opts.pkgType != typeGuess {
				if err := checkOutputDir(req, gopkg, opts.pkgType); err != nil {
					return err
				}
			}
			continue
		}
		info, lookupErr := getPkgsiteInfo(ctx, gopkg)
		reason, err := needsGitSourceFallback(gopkg, info, lookupErr)
		if err != nil {
			return fmt.Errorf("%q: %w", req.arg, err)
		}
		if reason != "" {
			if previous, ok := seenFallbacks[gopkg]; ok {
				return fmt.Errorf("%q and %q both request %q", previous, req.arg, gopkg)
			}
			seenFallbacks[gopkg] = req.arg
			fallbacks = append(fallbacks, gitFallback{req: req, gopkg: gopkg, reason: reason})
			if opts.pkgType != typeGuess {
				if err := checkOutputDir(req, gopkg, opts.pkgType); err != nil {
					return err
				}
			}
			continue
		}
		if req.revision != "" && strings.HasPrefix(req.revision, "v") && !module.IsPseudoVersion(req.revision) {
			if _, err := getPkgsiteModule(ctx, info.Module.Path, req.revision); err != nil {
				return fmt.Errorf("%q@%s: %w", req.arg, req.revision, err)
			}
		}
		effectivePkgType := opts.pkgType
		if effectivePkgType == typeGuess {
			effectivePkgType = typeLibrary
			if info.Package.Name == "main" {
				effectivePkgType = typeProgram
			}
		}
		if err := checkOutputDir(req, gopkg, effectivePkgType); err != nil {
			return err
		}
	}

	// Packages not on pkg.go.dev (or using -repo_url) need their source to
	// determine the guessed package type.
	for _, fb := range fallbacks {
		log.Printf("%s; using Git repository", fb.reason)
		fb.req.prepared, err = makeUpstreamSourceTarball(fb.gopkg, fb.req.revision, opts.forcePrerelease, nil, opts.repoURL)
		if err != nil {
			return fmt.Errorf("%q: %s, and the Git source fallback failed: %w", fb.req.arg, fb.reason, err)
		}
		if opts.pkgType == typeGuess {
			if err := checkOutputDir(fb.req, fb.gopkg, guessedPackageType(fb.req.prepared)); err != nil {
				return err
			}
		}
	}
	return nil
}

func parsePackArg(arg, defaultRevision string) (string, string, error) {
	arg = strings.TrimSpace(arg)
	defaultRevision = strings.TrimSpace(defaultRevision)
	if arg == "" {
		return "", "", fmt.Errorf("package import path is empty")
	}
	if strings.HasPrefix(arg, "-") {
		return "", "", fmt.Errorf("-git_revision must be specified before package arguments")
	}
	if at := strings.LastIndex(arg, "@"); at >= 0 {
		revision := strings.TrimSpace(arg[at+1:])
		if revision == "" {
			return "", "", fmt.Errorf("package revision is empty")
		}
		path := strings.TrimSpace(arg[:at])
		if path == "" {
			return "", "", fmt.Errorf("package import path is empty")
		}
		return path, revision, nil
	}
	return arg, defaultRevision, nil
}

func packPackage(gopkg, gitRevision string, opts packOptions, prepared *upstream) error {
	gopkg = trimRepoScheme(gopkg)

	// Set default source and binary package names.
	openRuyiSrc := nameFromGopkg(gopkg, typeLibrary, opts.customProgPkgName, opts.allowUnknownHoster)
	openRuyiLib := openRuyiSrc
	openRuyiProgram := nameFromGopkg(gopkg, typeProgram, opts.customProgPkgName, opts.allowUnknownHoster)

	pkgType := opts.pkgType
	if pkgType != typeGuess {
		openRuyiSrc = nameFromGopkg(gopkg, pkgType, opts.customProgPkgName, opts.allowUnknownHoster)
	}

	if strings.ToLower(gopkg) != gopkg {
		// Without -git_revision, specifying the package name in the wrong case
		// will lead to two checkouts, i.e. wasting bandwidth. With
		// -git_revision, packaging might fail.
		//
		// In case it turns out that Go package names should never contain any
		// uppercase letters, we can just auto-convert the argument.
		log.Printf("WARNING: Go package names are case-sensitive. Did you really mean %q instead of %q?\n",
			gopkg, strings.ToLower(gopkg))
	}

	// NOTE: directory existence is checked after determining final openRuyiSrc

	// Create a tarball of the upstream source
	u := prepared
	var err error
	if u == nil {
		info, lookupErr := getPkgsiteInfo(context.TODO(), gopkg)
		if lookupErr != nil {
			return fmt.Errorf("get package metadata: %w", lookupErr)
		}
		u, err = makeUpstreamSourceTarball(gopkg, gitRevision, opts.forcePrerelease, info, "")
		if err != nil {
			return fmt.Errorf("create upstream source tarball: %w", err)
		}
	}
	defer os.Remove(u.tarPath)

	if pkgType == typeGuess {
		switch guessedPackageType(u) {
		case typeProgram:
			if u.packageName == "main" {
				log.Printf("Assuming you are packaging a program (because package %q is main), use -type to override\n", gopkg)
			} else {
				log.Printf("Assuming you are packaging a program (because %q defines a main package), use -type to override\n", u.firstMain)
			}
			pkgType = typeProgram
			openRuyiSrc = nameFromGopkg(gopkg, pkgType, opts.customProgPkgName, opts.allowUnknownHoster)
		default:
			if u.firstMain != "" {
				log.Printf("Found main package %q and root package %q; assuming library, use -type to override\n", u.firstMain, u.packageName)
			}
			pkgType = typeLibrary
		}
	}

	// Now that we know the final package name, check output directory
	if err := checkEmptyOutputDir(openRuyiSrc); err != nil {
		return fmt.Errorf("output directory %w", err)
	}

	orig := fmt.Sprintf("%s_%s.orig.tar.%s", openRuyiSrc, u.version, u.compression)
	log.Printf("Moving tempfile to %q\n", orig)
	// We need to copy the file, merely renaming is not enough since the file
	// might be on a different filesystem (/tmp often is a tmpfs).
	if err := copyFile(u.tarPath, orig); err != nil {
		return fmt.Errorf("copy source tarball from %q to %q: %w", u.tarPath, orig, err)
	}

	// Now we create our rpm spec file from the gathered information.
	dir, err := createDirectory(openRuyiSrc)
	if err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	if err := writeSpec(dir, gopkg, openRuyiSrc, openRuyiLib, openRuyiProgram, u.version,
		pkgType, u, opts.providesPath, opts.conflicts); err != nil {
		_ = os.Remove(orig)
		return fmt.Errorf("create spec file: %w", err)
	}

	log.Println("Done!")

	fmt.Printf("\n")
	fmt.Printf("Packaging successfully created in %s\n", dir)
	switch pkgType {
	case typeLibrary:
		fmt.Printf("    Binary: %s\n", openRuyiLib)
	case typeProgram:
		fmt.Printf("    Binary: %s\n", openRuyiProgram)
	case typeLibraryProgram:
		fmt.Printf("    Binary: %s\n", openRuyiLib)
		fmt.Printf("    Binary: %s\n", openRuyiProgram)
	case typeProgramLibrary:
		fmt.Printf("    Binary: %s\n", openRuyiProgram)
		fmt.Printf("    Binary: %s\n", openRuyiLib)
	}
	fmt.Printf("\n")
	return nil
}
