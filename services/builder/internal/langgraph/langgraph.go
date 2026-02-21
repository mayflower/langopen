package langgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Graphs        map[string]string `json:"graphs"`
	Dependencies  []string          `json:"dependencies"`
	Env           any               `json:"env,omitempty"`
	PythonVersion string            `json:"python_version,omitempty"`
	PipInstaller  string            `json:"pip_installer,omitempty"`
}

type ResolvedDependency struct {
	Kind    string   `json:"kind"`
	Source  string   `json:"source"`
	Path    string   `json:"path,omitempty"`
	Install string   `json:"install"`
	Files   []string `json:"files,omitempty"`
}

type ValidationResult struct {
	Valid                bool                 `json:"valid"`
	Errors               []string             `json:"errors,omitempty"`
	Warnings             []string             `json:"warnings,omitempty"`
	PythonVersion        string               `json:"python_version,omitempty"`
	PipInstaller         string               `json:"pip_installer,omitempty"`
	ResolvedGraphs       map[string]string    `json:"resolved_graphs,omitempty"`
	ResolvedDependencies []ResolvedDependency `json:"resolved_dependencies,omitempty"`
}

func Parse(path string) (Config, error) {
	var cfg Config
	content, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read langgraph.json: %w", err)
	}
	if err := json.Unmarshal(content, &cfg); err != nil {
		return cfg, fmt.Errorf("parse langgraph.json: %w", err)
	}
	return cfg, nil
}

func Validate(repoRoot, configPath string) ValidationResult {
	res := ValidationResult{
		Valid:          true,
		PythonVersion:  "3.11",
		PipInstaller:   "pip",
		ResolvedGraphs: map[string]string{},
	}
	cfg, err := Parse(configPath)
	if err != nil {
		return ValidationResult{Valid: false, Errors: []string{err.Error()}}
	}
	repoAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		return ValidationResult{Valid: false, Errors: []string{fmt.Sprintf("resolve repo root: %v", err)}}
	}
	if strings.TrimSpace(cfg.PythonVersion) != "" {
		res.PythonVersion = strings.TrimSpace(cfg.PythonVersion)
	}
	if strings.TrimSpace(cfg.PipInstaller) != "" {
		res.PipInstaller = strings.TrimSpace(cfg.PipInstaller)
	}

	if len(cfg.Graphs) == 0 {
		res.Valid = false
		res.Errors = append(res.Errors, "graphs is required and cannot be empty")
	}
	for name, target := range cfg.Graphs {
		target = strings.TrimSpace(target)
		if target == "" {
			res.Valid = false
			res.Errors = append(res.Errors, fmt.Sprintf("graph %q target is empty", name))
			continue
		}
		i := indexRune(target, ':')
		if i <= 0 || i >= len(target)-1 {
			res.Valid = false
			res.Errors = append(res.Errors, fmt.Sprintf("graph %q has invalid target %q", name, target))
			continue
		}
		moduleOrPath := strings.TrimSpace(target[:i])
		if looksLikePath(moduleOrPath) {
			file := filepath.Clean(moduleOrPath)
			if filepath.IsAbs(file) || !isPathWithinRoot(repoAbs, filepath.Join(repoAbs, file)) {
				res.Valid = false
				res.Errors = append(res.Errors, fmt.Sprintf("graph %q file path must be within repo root: %q", name, file))
				continue
			}
			graphPath := filepath.Join(repoAbs, file)
			stat, statErr := os.Stat(graphPath)
			if statErr != nil {
				res.Valid = false
				res.Errors = append(res.Errors, fmt.Sprintf("graph %q file not found: %s", name, graphPath))
				continue
			}
			if stat.IsDir() {
				res.Valid = false
				res.Errors = append(res.Errors, fmt.Sprintf("graph %q file path points to a directory: %s", name, graphPath))
				continue
			}
		}
		res.ResolvedGraphs[name] = target
	}

	deps := cfg.Dependencies
	if len(deps) == 0 {
		deps = []string{"."}
		res.Warnings = append(res.Warnings, "dependencies missing; defaulting to repo root")
	}
	resolved := make([]ResolvedDependency, 0, len(deps))
	for _, dep := range deps {
		item, depErr := normalizeDependency(repoAbs, dep)
		if depErr != nil {
			res.Valid = false
			res.Errors = append(res.Errors, depErr.Error())
			continue
		}
		resolved = append(resolved, item)
	}
	res.ResolvedDependencies = resolved

	if !res.Valid && len(res.Errors) == 0 {
		res.Errors = append(res.Errors, "validation failed")
	}
	return res
}

func normalizeDependency(repoAbs, dep string) (ResolvedDependency, error) {
	cleanDep := strings.TrimSpace(dep)
	if cleanDep == "" {
		return ResolvedDependency{}, errors.New("dependency path cannot be empty")
	}

	if !looksLikePath(cleanDep) {
		return ResolvedDependency{
			Kind:    "package",
			Source:  cleanDep,
			Install: "pip install " + cleanDep,
		}, nil
	}

	depPath := filepath.Join(repoAbs, filepath.Clean(cleanDep))
	if filepath.IsAbs(cleanDep) || !isPathWithinRoot(repoAbs, depPath) {
		return ResolvedDependency{}, fmt.Errorf("dependency path must be within repo root: %q", dep)
	}
	info, statErr := os.Stat(depPath)
	if statErr != nil {
		return ResolvedDependency{}, fmt.Errorf("dependency directory not found: %s", depPath)
	}
	if !info.IsDir() {
		if strings.EqualFold(filepath.Base(depPath), "requirements.txt") {
			rel, _ := filepath.Rel(repoAbs, depPath)
			return ResolvedDependency{
				Kind:    "requirements",
				Source:  cleanDep,
				Path:    filepath.ToSlash(filepath.Dir(rel)),
				Install: "pip install -r " + filepath.ToSlash(rel),
				Files:   []string{filepath.ToSlash(rel)},
			}, nil
		}
		return ResolvedDependency{}, fmt.Errorf("dependency path is not a directory: %s", depPath)
	}

	requirementsPath := filepath.Join(depPath, "requirements.txt")
	if requirementsInfo, err := os.Stat(requirementsPath); err == nil && !requirementsInfo.IsDir() {
		relDir, _ := filepath.Rel(repoAbs, depPath)
		relReq, _ := filepath.Rel(repoAbs, requirementsPath)
		return ResolvedDependency{
			Kind:    "requirements",
			Source:  cleanDep,
			Path:    filepath.ToSlash(relDir),
			Install: "pip install -r " + filepath.ToSlash(relReq),
			Files:   []string{filepath.ToSlash(relReq)},
		}, nil
	}

	pyprojectPath := filepath.Join(depPath, "pyproject.toml")
	if pyprojectInfo, err := os.Stat(pyprojectPath); err == nil && !pyprojectInfo.IsDir() {
		relDir, _ := filepath.Rel(repoAbs, depPath)
		relPyproject, _ := filepath.Rel(repoAbs, pyprojectPath)
		path := filepath.ToSlash(relDir)
		return ResolvedDependency{
			Kind:    "pyproject",
			Source:  cleanDep,
			Path:    path,
			Install: "pip install " + path,
			Files:   []string{filepath.ToSlash(relPyproject)},
		}, nil
	}

	setupPath := filepath.Join(depPath, "setup.py")
	if setupInfo, err := os.Stat(setupPath); err == nil && !setupInfo.IsDir() {
		relDir, _ := filepath.Rel(repoAbs, depPath)
		relSetup, _ := filepath.Rel(repoAbs, setupPath)
		path := filepath.ToSlash(relDir)
		return ResolvedDependency{
			Kind:    "setup",
			Source:  cleanDep,
			Path:    path,
			Install: "pip install " + path,
			Files:   []string{filepath.ToSlash(relSetup)},
		}, nil
	}

	return ResolvedDependency{}, fmt.Errorf("dependency directory missing requirements.txt, pyproject.toml, or setup.py: %s", depPath)
}

func isPathWithinRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".."
}

func looksLikePath(value string) bool {
	if value == "." || value == ".." {
		return true
	}
	if strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "/") {
		return true
	}
	return strings.Contains(value, "/") || strings.Contains(value, "\\")
}

func indexRune(s string, sep rune) int {
	for i, r := range s {
		if r == sep {
			return i
		}
	}
	return -1
}

func BuildKitJobSpec(repoURL, gitRef, repoPath, imageName, commitSHA string) (map[string]any, error) {
	if repoURL == "" || gitRef == "" || imageName == "" || commitSHA == "" {
		return nil, errors.New("repo_url, git_ref, image_name, and commit_sha are required")
	}
	contextRef := strings.TrimSpace(repoURL) + "#" + strings.TrimSpace(gitRef)
	repoPath = strings.TrimSpace(repoPath)
	if repoPath != "" && repoPath != "." && repoPath != "/" {
		repoPath = strings.TrimPrefix(repoPath, "./")
		repoPath = strings.TrimPrefix(repoPath, "/")
		if repoPath != "" {
			contextRef += ":" + repoPath
		}
	}
	shortSHA := commitSHA
	if len(shortSHA) > 8 {
		shortSHA = shortSHA[:8]
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name": "buildkit-" + shortSHA,
			"labels": map[string]string{
				"app":        "langopen-builder",
				"build.sha":  commitSHA,
				"build.repo": repoURL,
			},
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"restartPolicy":    "Never",
					"runtimeClassName": "kata-qemu",
					"containers": []map[string]any{{
						"name":  "buildkit",
						"image": "moby/buildkit:rootless",
						"command": []string{
							"buildctl-daemonless.sh",
						},
						"args": []string{
							"build",
							"--frontend=dockerfile.v0",
							"--opt", "context=" + contextRef,
							"--opt", "filename=Dockerfile",
							"--output", "type=image,name=" + imageName + ":" + commitSHA + ",push=true",
						},
					}},
				},
			},
		},
	}, nil
}
