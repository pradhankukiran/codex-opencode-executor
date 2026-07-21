package runner

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultModel          = "xai/grok-4.5"
	DefaultAgent          = "build"
	DefaultTimeout        = 30 * time.Minute
	DefaultMaxFinalChars  = 1200
	HardMaxFinalChars     = 4000
	OpenCodeEnvModel      = "CODEX_OPENCODE_MODEL"
	defaultOpenCodeBinary = "opencode"
)

// Config holds validated runner options.
type Config struct {
	Directory     string
	Prompt        string
	Model         string
	Auto          bool
	Timeout       time.Duration
	OpenCode      string
	LogDir        string
	MaxFinalChars int
}

// ParseArgs parses CLI flags and prompt input.
// argv should be os.Args[1:]. stdin is used when no positional prompt is given.
func ParseArgs(argv []string, stdin io.Reader, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	fs := flag.NewFlagSet("codex-opencode-runner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		directory     string
		model         string
		auto          = true
		timeout       time.Duration
		opencodePath  string
		logDir        string
		maxFinalChars int
	)

	fs.StringVar(&directory, "directory", "", "project directory to run against (required)")
	fs.StringVar(&model, "model", "", "provider/model override (env: CODEX_OPENCODE_MODEL)")
	fs.BoolVar(&auto, "auto", true, "auto-approve OpenCode permissions (default true; pass --auto=false to opt out)")
	fs.DurationVar(&timeout, "timeout", DefaultTimeout, "maximum run duration")
	fs.StringVar(&opencodePath, "opencode", defaultOpenCodeBinary, "OpenCode executable path or name")
	fs.StringVar(&logDir, "log-dir", "", "directory for event and stderr logs (default: user cache)")
	fs.IntVar(&maxFinalChars, "max-final-chars", DefaultMaxFinalChars, "maximum final_text characters in the result JSON")

	if err := fs.Parse(argv); err != nil {
		return Config{}, err
	}

	if strings.TrimSpace(directory) == "" {
		return Config{}, errors.New("required flag: --directory")
	}
	canonical, err := resolveDirectory(directory)
	if err != nil {
		return Config{}, err
	}

	prompt, err := readPrompt(fs.Args(), stdin)
	if err != nil {
		return Config{}, err
	}

	resolvedModel := strings.TrimSpace(model)
	if resolvedModel == "" {
		resolvedModel = strings.TrimSpace(getenv(OpenCodeEnvModel))
	}
	if resolvedModel == "" {
		resolvedModel = DefaultModel
	}

	if maxFinalChars <= 0 {
		maxFinalChars = DefaultMaxFinalChars
	}
	if maxFinalChars > HardMaxFinalChars {
		maxFinalChars = HardMaxFinalChars
	}

	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	opencodePath = strings.TrimSpace(opencodePath)
	if opencodePath == "" {
		opencodePath = defaultOpenCodeBinary
	}

	if logDir == "" {
		logDir, err = defaultLogDir()
		if err != nil {
			return Config{}, err
		}
	}
	logDir, err = filepath.Abs(logDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve log-dir: %w", err)
	}

	return Config{
		Directory:     canonical,
		Prompt:        prompt,
		Model:         resolvedModel,
		Auto:          auto,
		Timeout:       timeout,
		OpenCode:      opencodePath,
		LogDir:        logDir,
		MaxFinalChars: maxFinalChars,
	}, nil
}

func readPrompt(args []string, stdin io.Reader) (string, error) {
	if len(args) > 0 {
		prompt := strings.TrimSpace(strings.Join(args, " "))
		if prompt == "" {
			return "", errors.New("prompt is empty")
		}
		return prompt, nil
	}
	if stdin == nil {
		return "", errors.New("prompt is empty")
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read prompt from stdin: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", errors.New("prompt is empty")
	}
	return prompt, nil
}

func resolveDirectory(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve directory: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("directory does not exist: %s", abs)
		}
		return "", fmt.Errorf("stat directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", abs)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize directory: %w", err)
	}
	return canonical, nil
}

func defaultLogDir() (string, error) {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "codex-opencode-runner", "logs"), nil
	}
	return filepath.Join(os.TempDir(), "codex-opencode-runner", "logs"), nil
}

// DefaultLockDir returns the directory used for per-project locks.
func DefaultLockDir() (string, error) {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "codex-opencode-runner", "locks"), nil
	}
	return filepath.Join(os.TempDir(), "codex-opencode-runner", "locks"), nil
}
