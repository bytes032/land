// Package config is a repository's .land.yaml: its trunk, policies, box settings and the commands it attaches to land's
// stages. The core never names a repository's commands; everything repository-specific arrives through this file.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the configuration's name at the repository root.
const File = ".land.yaml"

// Config is a repository's .land.yaml.
type Config struct {
	Version string  `yaml:"version"` // the pinned land version, bare semver
	Trunk   string  `yaml:"trunk"`
	Message Message `yaml:"message"`
	Gate    Gate    `yaml:"gate"`
	Box     Box     `yaml:"box"`
	Stages  Stages  `yaml:"stages"`
}

// Message is what a landing's commit message must hold.
type Message struct {
	// Require are regular expressions the message must match, each matched line by line ((?m)).
	Require []string `yaml:"require"`
}

// Gate is how CI's gate treats a commit with no verdict.
type Gate struct {
	Timeout    Duration `yaml:"timeout"`
	Unverified string   `yaml:"unverified"` // "fail" or "skip"
}

// Box is the repository's box settings.
type Box struct {
	Type string            `yaml:"type"` // the Hetzner server type
	Env  map[string]string `yaml:"env"`  // set before every command on the box
}

// Stages are the commands a repository attaches to land's stages. setup, prepare, warm and review are one command
// string each; check and regenerate are named entries.
type Stages struct {
	Setup      string       `yaml:"setup"`
	Prepare    string       `yaml:"prepare"`
	Check      Checks       `yaml:"check"`
	Regenerate []Regenerate `yaml:"regenerate"`
	Warm       string       `yaml:"warm"`
	Review     string       `yaml:"review"`
}

// Check is one named check of the check stage.
type Check struct {
	Name string
	Run  string `yaml:"run"`
	// Paths are prefixes: when set, the check runs only if a changed path starts with one of them.
	Paths []string `yaml:"paths"`
	// Ignore are suffixes: the check is skipped when the change is non-empty and every changed path ends with one.
	Ignore []string `yaml:"ignore"`
}

// Checks are the check stage's entries in the order the file lists them, the order they run in.
type Checks []Check

// UnmarshalYAML reads a mapping of check names to checks, keeping its order. Each check is decoded as strictly as the
// file: a key no check has is an error.
func (c *Checks) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: stages.check is a mapping of check names to checks", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		var check Check
		if err := strict(node.Content[i+1], &check); err != nil {
			return fmt.Errorf("stages.check.%s: %w", node.Content[i].Value, err)
		}
		check.Name = node.Content[i].Value
		*c = append(*c, check)
	}
	return nil
}

// strict decodes node into v refusing keys v has no field for, which node.Decode alone doesn't: a decoder's KnownFields
// doesn't reach a custom UnmarshalYAML's node.
func strict(node *yaml.Node, v any) error {
	data, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(v)
}

// Regenerate kinds.
const (
	Lockfile = "lockfile" // rerun when the commit changes it; a conflict stops land; stays in the review key
	Derived  = "derived"  // rerun when changed or conflicted; a conflict takes the trunk's side; out of the review key
)

// Regenerate is a file a command rewrites, and its kind.
type Regenerate struct {
	Kind string `yaml:"kind"`
	Path string `yaml:"path"` // a file, or a directory ("gen" or "gen/"), as git's pathspec names it
	Run  string `yaml:"run"`
}

// Duration is a time.Duration written as Go writes one ("30m").
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	d.Duration = parsed
	return nil
}

// Parse reads a .land.yaml's bytes, refusing keys land doesn't know, so a misspelled or removed key is an error
// rather than silently ignored. It doesn't validate the values; Load does.
func Parse(data []byte) (*Config, error) {
	var c Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &c, nil
}

// Load reads and validates root's .land.yaml.
func Load(root string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(root, File))
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	return c, c.validate()
}

// semver is a bare semantic version: no "v", no quotes.
var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// validate checks the required keys and their values, naming the key at fault.
func (c *Config) validate() error {
	if err := c.check(); err != nil {
		return fmt.Errorf("%s: %w", File, err)
	}
	return nil
}

func (c *Config) check() error {
	switch {
	case c.Version == "":
		return errors.New("version is required: the land version this commit pins, such as 0.1.0")
	case !semver.MatchString(c.Version):
		return fmt.Errorf("version %q isn't a bare semantic version such as 0.1.0", c.Version)
	case c.Trunk == "":
		return errors.New("trunk is required: the branch every landing moves forward")
	case c.Gate.Timeout.Duration <= 0:
		return errors.New("gate.timeout is required: a positive duration such as 30m")
	case c.Gate.Unverified != "fail" && c.Gate.Unverified != "skip":
		return fmt.Errorf("gate.unverified is %q; it must be fail or skip", c.Gate.Unverified)
	case c.Box.Type == "":
		return errors.New("box.type is required: the Hetzner server type boxes are created with, such as cpx42")
	case len(c.Stages.Check) == 0:
		return errors.New("stages.check needs at least one named check")
	}
	for i, pattern := range c.Message.Require {
		if _, err := regexp.Compile("(?m)" + pattern); err != nil {
			return fmt.Errorf("message.require[%d]: %w", i, err)
		}
	}
	seen := map[string]bool{}
	for _, check := range c.Stages.Check {
		switch {
		case check.Name == "":
			return errors.New("stages.check: a check has an empty name")
		case seen[check.Name]:
			return fmt.Errorf("stages.check.%s is listed twice", check.Name)
		case check.Run == "":
			return fmt.Errorf("stages.check.%s.run is required", check.Name)
		}
		seen[check.Name] = true
	}
	for i, r := range c.Stages.Regenerate {
		switch {
		case r.Kind != Lockfile && r.Kind != Derived:
			return fmt.Errorf("stages.regenerate[%d].kind is %q; it must be %s or %s", i, r.Kind, Lockfile, Derived)
		case r.Path == "":
			return fmt.Errorf("stages.regenerate[%d].path is required", i)
		case r.Run == "":
			return fmt.Errorf("stages.regenerate[%d].run is required", i)
		}
	}
	return nil
}

// RepoName is the repository's name, <owner>.<name>, from root's origin URL: github.com/acme/api is acme.api. It names
// the state directory, the build root on the box and the snapshot label.
func RepoName(ctx context.Context, root string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("the origin remote's URL: %w", err)
	}
	return repoName(strings.TrimSpace(string(out)))
}

func repoName(remote string) (string, error) {
	path := remote
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" {
		path = u.Path
	} else if _, after, ok := strings.Cut(remote, ":"); ok { // git@github.com:owner/name.git
		path = after
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(path, ".git"), "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return "", errors.New("origin's URL names no owner and repository: " + remote)
	}
	return parts[len(parts)-2] + "." + parts[len(parts)-1], nil
}

// StateDir is land's state directory for a repository (claims, review records), under the user's config directory.
func StateDir(repo string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "land", repo), nil
}
