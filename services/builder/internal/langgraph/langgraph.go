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
	Graphs       map[string]string `json:"graphs"`
	Dependencies []string          `json:"dependencies"`
	Env          any               `json:"env,omitempty"`
}

type ValidationResult struct {
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
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
	res := ValidationResult{Valid: true}
	cfg, err := Parse(configPath)
	if err != nil {
		return ValidationResult{Valid: false, Errors: []string{err.Error()}}
	}
	repoAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		return ValidationResult{Valid: false, Errors: []string{fmt.Sprintf("resolve repo root: %v", err)}}
	}

	if len(cfg.Graphs) == 0 {
		res.Valid = false
		res.Errors = append(res.Errors, "graphs is required and cannot be empty")
	}
	for name, target := range cfg.Graphs {
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
		file := filepath.Clean(strings.TrimSpace(target[:i]))
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
		}
	}

	if len(cfg.Dependencies) == 0 {
		res.Valid = false
		res.Errors = append(res.Errors, "dependencies must include at least one directory")
	}
	for _, dep := range cfg.Dependencies {
		cleanDep := filepath.Clean(strings.TrimSpace(dep))
		if cleanDep == "" {
			res.Valid = false
			res.Errors = append(res.Errors, "dependency path cannot be empty")
			continue
		}
		depPath := filepath.Join(repoAbs, cleanDep)
		if filepath.IsAbs(cleanDep) || !isPathWithinRoot(repoAbs, depPath) {
			res.Valid = false
			res.Errors = append(res.Errors, fmt.Sprintf("dependency path must be within repo root: %q", dep))
			continue
		}
		info, statErr := os.Stat(depPath)
		if statErr != nil {
			res.Valid = false
			res.Errors = append(res.Errors, fmt.Sprintf("dependency directory not found: %s", depPath))
			continue
		}
		if !info.IsDir() {
			res.Valid = false
			res.Errors = append(res.Errors, fmt.Sprintf("dependency path is not a directory: %s", depPath))
			continue
		}
		requirementsPath := filepath.Join(depPath, "requirements.txt")
		requirementsInfo, statErr := os.Stat(requirementsPath)
		if statErr != nil {
			res.Valid = false
			res.Errors = append(res.Errors, fmt.Sprintf("requirements.txt missing in dependency path: %s", depPath))
			continue
		}
		if requirementsInfo.IsDir() {
			res.Valid = false
			res.Errors = append(res.Errors, fmt.Sprintf("requirements.txt path is a directory in dependency path: %s", depPath))
		}
	}
	if !res.Valid && len(res.Errors) == 0 {
		res.Errors = append(res.Errors, "validation failed")
	}
	return res
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
