package main

import (
	"context"
	_ "embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/glamour"
)

//go:embed description.json
var descriptionJSONBytes []byte

type specMetadata struct {
	description     string
	longDescription string
	license         string
	assetFiles      specAssetFiles // license and README paths relative to the source root
}

// readSpecMetadata reads metadata from the source checkout in dir when set,
// otherwise from pkgsite. Missing metadata is logged and left as TODO.
func readSpecMetadata(gopkg, dir string) specMetadata {
	m := specMetadata{
		description:     "TODO: short description",
		longDescription: "TODO: long description",
		license:         "TODO",
	}
	if dir == "" {
		var err error
		if m.description, err = getDescriptionForGopkg(gopkg); err != nil {
			log.Printf("Could not determine description for %q: %v\n", gopkg, err)
			m.description = "TODO: short description"
		}
		if m.longDescription, err = getLongDescriptionForGopkg(gopkg); err != nil {
			log.Printf("Could not determine long description for %q: %v\n", gopkg, err)
			m.longDescription = "TODO: long description"
		}
		if m.license, err = getLicenseForGopkg(gopkg); err != nil {
			log.Printf("Could not determine license for %q: %v\n", gopkg, err)
			m.license = "TODO"
		}
		if m.assetFiles, err = getSpecAssetFilesForGopkg(gopkg); err != nil {
			log.Printf("Could not determine license/doc file paths for %q: %v\n", gopkg, err)
		}
		return m
	}

	if licenses, err := sourceTarballLicenseFiles(dir); err != nil {
		log.Printf("Could not determine license file paths in %q: %v\n", dir, err)
	} else {
		m.assetFiles.licenseFiles = licenses
	}
	readme, err := findReadme(dir)
	if err != nil {
		log.Printf("Could not locate README in %q: %v\n", dir, err)
		return m
	}
	if readme == "" {
		return m
	}
	data, err := os.ReadFile(filepath.Join(dir, readme))
	if err != nil {
		log.Printf("Could not read README %q: %v\n", readme, err)
		return m
	}
	m.assetFiles.readmeFile = readme
	if summary := summaryFromReadme(string(data)); summary != "" {
		m.description = summary
	}
	if long, err := longDescriptionFromReadme(readme, string(data)); err != nil {
		log.Printf("Could not determine long description from %q: %v\n", readme, err)
	} else {
		m.longDescription = long
	}
	return m
}

// findReadme returns the name of the README file in dir, if any.
func findReadme(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch strings.ToLower(entry.Name()) {
		case "readme", "readme.md", "readme.markdown", "readme.mdown", "readme.mkdn", "readme.txt":
			return entry.Name(), nil
		}
	}
	return "", nil
}

// sourceTarballLicenseFiles lists the license files that go into the source
// tarball, skipping the directories tar() excludes (tar matches
// --exclude=Godeps/_workspace at any depth) or findVendorDirs removes.
// As with pkgsite, root license files take precedence over nested ones.
func sourceTarballLicenseFiles(dir string) ([]string, error) {
	var root, nested []string
	err := filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == "vendor" ||
				rel == "Godeps/_workspace" || strings.HasSuffix(rel, "/Godeps/_workspace") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isLicenseFilename(entry.Name()) {
			return nil
		}
		if strings.Contains(rel, "/") {
			nested = append(nested, rel)
		} else {
			root = append(root, rel)
		}
		return nil
	})
	if len(root) > 0 {
		nested = root
	}
	slices.Sort(nested)
	return nested, err
}

// reformatForControl reformats wrapped text for the RPM spec's %description.
func reformatForControl(raw string) string {
	output := ""
	next_prefix := ""
	re := regexp.MustCompile(`^ \d+\. `)

	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		// Remove paddings that Glamour currently add to the end of each line
		line = strings.TrimRight(line, " ")

		// Try to add hanging indent for list items that span over one line
		prefix := next_prefix
		if strings.HasPrefix(line, " * ") {
			// unordered list
			prefix = ""
			next_prefix = "  "
		}
		if re.MatchString(line) {
			// ordered list
			prefix = ""
			next_prefix = "   "
		}
		if line == "" {
			// blank line, implying end of list
			line = "."
			prefix = ""
			next_prefix = ""
		}
		output += " " + prefix + line + "\n"
	}
	return output
}

// markdownToLongDescription converts Markdown to plain text for the RPM spec's
// %description section.
func markdownToLongDescription(markdown string) (string, error) {
	r, _ := glamour.NewTermRenderer(
		glamour.WithStylesFromJSONBytes(descriptionJSONBytes),
		glamour.WithWordWrap(72),
	)
	out, err := r.Render(markdown)
	if err != nil {
		return "", fmt.Errorf("fail to render Markdown: %w", err)
	}
	//fmt.Println(out)
	//fmt.Println(reformatForControl(out))
	return reformatForControl(out), nil
}

// getLongDescriptionForGopkg reads README.md (or equivalent) from pkg.go.dev,
// intended for the RPM spec's %description section.
func getLongDescriptionForGopkg(gopkg string) (string, error) {
	info, err := getPkgsiteInfo(context.TODO(), gopkg)
	if err != nil {
		return "", fmt.Errorf("get pkgsite metadata: %w", err)
	}
	if info.Module.Readme == nil || strings.TrimSpace(info.Module.Readme.Contents) == "" {
		return "", fmt.Errorf("pkgsite module %q has no README", info.Module.Path)
	}
	content := info.Module.Readme.Contents

	// Supported filename suffixes are from
	// https://github.com/github/markup/blob/master/README.md
	// NOTE(stapelberg): Ideally, we’d use https://github.com/github/markup
	// itself to render to HTML, then convert HTML to plaintext. That sounds
	// fairly involved, but it’d be the most correct solution to the problem at
	// hand. Our current code just knows markdown, which is good enough since
	// most (Go?) projects in fact use markdown for their README files.
	return longDescriptionFromReadme(info.Module.Readme.Filepath, content)
}

func isMarkdownFilename(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, "md") ||
		strings.HasSuffix(name, "markdown") ||
		strings.HasSuffix(name, "mdown") ||
		strings.HasSuffix(name, "mkdn")
}

// longDescriptionFromReadme converts README contents for %description,
// rendering Markdown files and reflowing anything else as plain text.
func longDescriptionFromReadme(filename, content string) (string, error) {
	if !isMarkdownFilename(filename) {
		return reformatForControl(content), nil
	}
	return markdownToLongDescription(content)
}
