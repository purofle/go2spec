package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

type goListModule struct {
	Path    string        `json:"Path"`
	Version string        `json:"Version"`
	Main    bool          `json:"Main"`
	Replace *goListModule `json:"Replace"`
}

// moduleGraphEnv disables workspace mode so a committed go.work does not turn
// in-repository replaced modules into main modules.
func moduleGraphEnv() []string {
	return append(passthroughEnv(), "GOWORK=off")
}

type moduleReplacements struct {
	modules            []string
	replacements       map[string]string
	unsupported        map[string]error
	unresolvedReplaces []string
}

// loadModuleReplacements resolves the active replace directives of the go.mod
// in repoDir against the module graph when that go.mod declares modulePath and
// contains replace directives. Otherwise, including when go.mod is missing, it
// returns (nil, nil).
func loadModuleReplacements(repoDir, modulePath string) (*moduleReplacements, error) {
	gomodPath := filepath.Join(repoDir, "go.mod")
	data, err := os.ReadFile(gomodPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read go.mod: %w", err)
	}
	gomod, err := modfile.Parse(gomodPath, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}
	if len(gomod.Replace) == 0 {
		return nil, nil
	}
	if gomod.Module == nil || gomod.Module.Mod.Path != modulePath {
		log.Printf("WARNING: go.mod in repository root declares module %q, not %q; skipping replace resolution", modfile.ModulePath(data), modulePath)
		return nil, nil
	}

	cmd := exec.Command("go", "list", "-e", "-m", "-mod=readonly", "-json", "all")
	cmd.Dir = repoDir
	cmd.Env = moduleGraphEnv()
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve module replacements: %w", err)
	}

	g := &moduleReplacements{
		replacements: make(map[string]string),
		unsupported:  make(map[string]error),
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	// 循环 decode go list 的输出直到结束
	for {
		var mod goListModule
		err := dec.Decode(&mod)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode go list -m output: %w", err)
		}
		if mod.Main {
			continue
		}
		g.modules = append(g.modules, mod.Path)
		if mod.Replace == nil || mod.Replace.Path == mod.Path {
			continue
		}
		if mod.Replace.Version == "" {
			g.unsupported[mod.Path] = fmt.Errorf("unsupported replace %s => %s: local path replacements are not supported", mod.Path, mod.Replace.Path)
			continue
		}
		if err := module.Check(mod.Replace.Path, mod.Replace.Version); err != nil || module.CanonicalVersion(mod.Replace.Version) != mod.Replace.Version {
			g.unsupported[mod.Path] = fmt.Errorf("unsupported replace %s => %s@%s: target must be a valid module with a canonical fixed version", mod.Path, mod.Replace.Path, mod.Replace.Version)
			continue
		}
		g.replacements[mod.Path] = mod.Replace.Path
	}

	for _, r := range gomod.Replace {
		if !slices.Contains(g.modules, r.Old.Path) && !slices.Contains(g.unresolvedReplaces, r.Old.Path) {
			g.unresolvedReplaces = append(g.unresolvedReplaces, r.Old.Path)
		}
	}
	return g, nil
}

func (g *moduleReplacements) owningModule(importPath string) string {
	best := ""
	for _, modPath := range g.modules {
		if importWithin(importPath, modPath) && len(modPath) > len(best) {
			best = modPath
		}
	}
	return best
}

func (g *moduleReplacements) hasReplacedAncestor(modPath string) bool {
	for oldMod := range g.replacements {
		if strings.HasPrefix(modPath, oldMod+"/") {
			return true
		}
	}
	return false
}

func (g *moduleReplacements) applyToDependencies(deps map[string]bool, exactModules map[string]bool) error {
	if len(deps) == 0 {
		return nil
	}
	mapped := make(map[string]bool, len(deps))
	for dep := range deps {
		owner := g.owningModule(dep)
		if owner != "" {
			if err := g.unsupported[owner]; err != nil {
				return fmt.Errorf("imported package %q: %w", dep, err)
			}
			if repl, ok := g.replacements[owner]; ok {
				mapped[repl] = true
				exactModules[repl] = true
				continue
			}
			if g.hasReplacedAncestor(owner) {
				mapped[owner] = true
				exactModules[owner] = true
				continue
			}
		} else {
			for _, rPath := range g.unresolvedReplaces {
				if importWithin(dep, rPath) {
					return fmt.Errorf("cannot resolve replace directive for imported package %q (module %q is not in the module graph)", dep, rPath)
				}
			}
		}
		mapped[dep] = true
	}
	clear(deps)
	for dep := range mapped {
		deps[dep] = true
	}
	return nil
}
