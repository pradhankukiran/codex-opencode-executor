package runner

// BuildOpenCodeArgs constructs the exact argv for the OpenCode child (excluding the binary).
// Shape: run --agent build --model <model> [--auto] --format json --dir <dir> <prompt>
func BuildOpenCodeArgs(cfg Config) []string {
	args := []string{
		"run",
		"--agent", DefaultAgent,
		"--model", cfg.Model,
	}
	if cfg.Auto {
		args = append(args, "--auto")
	}
	args = append(args,
		"--format", "json",
		"--dir", cfg.Directory,
		cfg.Prompt,
	)
	return args
}
