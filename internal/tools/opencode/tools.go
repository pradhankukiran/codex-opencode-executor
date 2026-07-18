package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pradhankukiran/codex-opencode-executor/internal/tools/mcputil"
	"github.com/pradhankukiran/codex-opencode-executor/internal/workspace"
)

const maxJobTimeoutSeconds = 24 * 60 * 60

const (
	maxDiffChars                  = 100_000
	maxVerificationTimeoutSeconds = 60 * 60
)

// Register adds opencode handoff tools to an MCP server.
func Register(s *mcp.Server, client *Client, mgr *Manager, workspaces *workspace.Manager, opts ExecutorOptions) {
	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_health",
		Description: "Check connectivity to the configured opencode HTTP server. Call this if other handoff tools return connection or authentication errors.",
		Flags:       mcputil.ReadOnly,
	}, healthHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_agents",
		Description: "List opencode agents available for a directory/workspace.",
		Flags:       mcputil.ReadOnly,
	}, agentsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_models",
		Description: "List opencode providers and optionally models. Supports substring, glob (e.g. 'openai/gpt-*-mini'), or regex filtering and a result limit to avoid cluttering context with large provider catalogs (e.g. OpenRouter).",
		Flags:       mcputil.ReadOnly,
	}, modelsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_sessions",
		Description: "List opencode sessions visible in a directory/workspace.",
		Flags:       mcputil.ReadOnly,
	}, sessionsHandler(client))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_create_session",
		Description: "Create a new opencode session and return its session_id. Configured model, agent, permission, and workspace-isolation defaults are used unless overridden. Auto isolation uses a worktree for clean Git projects and runs directly otherwise. Use the returned session_id with handoff_fire.",
	}, createSessionHandler(client, workspaces, opts))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_fire",
		Description: "Submit a prompt to an existing opencode session and return immediately. The configured default agent is used unless overridden. Requires a session_id from handoff_create_session. Use handoff_check with the session_id to poll progress.",
	}, fireHandler(client, mgr, workspaces, opts))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_check",
		Description: "Poll progress for a session_id returned by handoff_fire, or inspect sessions for pending permissions/questions.",
		Flags:       mcputil.ReadOnly,
	}, checkHandler(client, mgr, workspaces))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_cancel",
		Description: "Cancel active opencode execution for a session. Cancellation is idempotent for jobs already in a terminal state.",
		Flags:       mcputil.Destructive,
	}, cancelHandler(mgr, workspaces))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_workspace",
		Description: "Inspect the durable workspace bound to an opencode session, including changed files, commits, diff statistics, and recorded verification checks.",
		Flags:       mcputil.ReadOnly,
	}, workspaceInspectHandler(workspaces))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_diff",
		Description: "Read a bounded tracked-file Git diff for a session workspace relative to its starting commit. Untracked file names are available from handoff_workspace.",
		Flags:       mcputil.ReadOnly,
	}, workspaceDiffHandler(workspaces))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_verify",
		Description: "Run explicit executable-and-argv verification checks in a completed session workspace and durably record structured pass/fail results. Commands run directly without implicit shell expansion.",
		Flags:       mcputil.Destructive,
	}, workspaceVerifyHandler(mgr, workspaces))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_cleanup",
		Description: "Remove an executor-owned worktree after execution. Clean worktrees are removed by default; force=true is required to discard changes or commits.",
		Flags:       mcputil.Destructive,
	}, workspaceCleanupHandler(mgr, workspaces))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_permission_reply",
		Description: "Reply to an opencode permission request for a session. Use handoff_check to see pending permissions.",
	}, permissionReplyHandler(client, workspaces))

	mcputil.Register(s, mcputil.ToolDef{
		Name:        "handoff_question_reply",
		Description: "Reply to or reject an opencode clarification question for a session. Use handoff_check to see pending questions.",
	}, questionReplyHandler(client, workspaces))
}

type locationParams struct {
	Directory string `json:"directory,omitempty" jsonschema:"Project directory for opencode location scoping."`
	Workspace string `json:"workspace,omitempty" jsonschema:"Optional opencode workspace identifier."`
}

func (p locationParams) location() Location {
	return Location(p)
}

func sessionLocation(workspaces *workspace.Manager, sessionID string, fallback Location) Location {
	if workspaces != nil {
		fallback.Directory = workspaces.Resolve(sessionID, fallback.Directory)
	}
	return fallback
}

func healthHandler(client *Client) mcp.ToolHandlerFor[struct{}, HealthResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, HealthResult, error) {
		data, err := client.Health(ctx)
		if err != nil {
			return nil, HealthResult{OK: false, BaseURL: client.BaseURL(), Message: err.Error()}, nil
		}
		return nil, HealthResult{OK: true, BaseURL: client.BaseURL(), Data: data, Message: "opencode server is reachable"}, nil
	}
}

func agentsHandler(client *Client) mcp.ToolHandlerFor[locationParams, AgentsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args locationParams) (*mcp.CallToolResult, AgentsResult, error) {
		agents, err := client.Agents(ctx, args.location())
		if err != nil {
			return nil, AgentsResult{}, err
		}
		return nil, AgentsResult{Agents: agents}, nil
	}
}

type modelsParams struct {
	locationParams
	IncludeModels bool   `json:"include_models,omitempty" jsonschema:"If true, includes the list of individual models. Defaults to false."`
	Filter        string `json:"filter,omitempty" jsonschema:"Optional substring, glob, or regex to filter models (e.g., 'xai/', 'openai/gpt-*-mini'). Implies include_models=true."`
	Limit         int    `json:"limit,omitempty" jsonschema:"Maximum number of models to return when include_models is true. Defaults to 50; pass -1 for no limit. Zero is treated as the default."`
}

func modelsHandler(client *Client) mcp.ToolHandlerFor[modelsParams, ModelsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args modelsParams) (*mcp.CallToolResult, ModelsResult, error) {
		res, err := client.ProvidersAndModels(ctx, args.location())
		if err != nil {
			return nil, ModelsResult{}, err
		}

		includeModels := args.IncludeModels || args.Filter != ""
		if !includeModels {
			res.Models = nil
			return nil, res, nil
		}

		if args.Filter != "" {
			var filtered []ModelSummary
			for _, m := range res.Models {
				if matchModel(args.Filter, m) {
					filtered = append(filtered, m)
				}
			}
			res.Models = filtered
		}

		limit := 50
		if args.Limit != 0 {
			limit = args.Limit
		}

		if limit > 0 && len(res.Models) > limit {
			res.Models = res.Models[:limit]
		}

		return nil, res, nil
	}
}

// matchModel reports whether m matches filter using substring, then glob
// (path.Match), then regex — in that order. Regex is only attempted when
// neither substring nor glob matches, so a glob like "openai/gpt-*-mini" is
// never re-interpreted as a regex. Substring matching means a bare "xai/"
// matches all xai models without needing the "xai/*" glob form.
func matchModel(filter string, m ModelSummary) bool {
	if filter == "" {
		return true
	}

	fullID := m.ProviderID + "/" + m.ID

	if strings.Contains(fullID, filter) || strings.Contains(m.ID, filter) || strings.Contains(m.Name, filter) {
		return true
	}

	if matched, _ := path.Match(filter, fullID); matched {
		return true
	}
	if matched, _ := path.Match(filter, m.ID); matched {
		return true
	}

	if re, err := regexp.Compile(filter); err == nil {
		if re.MatchString(fullID) || re.MatchString(m.ID) || re.MatchString(m.Name) {
			return true
		}
	}

	return false
}

func sessionsHandler(client *Client) mcp.ToolHandlerFor[SessionsRequest, SessionsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args SessionsRequest) (*mcp.CallToolResult, SessionsResult, error) {
		res, err := client.Sessions(ctx, args)
		if err != nil {
			return nil, SessionsResult{}, err
		}
		return nil, res, nil
	}
}

type createSessionParams struct {
	locationParams
	Title          string `json:"title,omitempty" jsonschema:"Optional session title."`
	ParentID       string `json:"parent_id,omitempty" jsonschema:"Optional parent session ID."`
	Model          string `json:"model,omitempty" jsonschema:"Optional model in provider/model form (e.g. 'xai/grok-4.5'). Overrides the configured default. Model is fixed for the lifetime of the session."`
	ProviderID     string `json:"provider_id,omitempty" jsonschema:"Optional model provider id for compatibility. Must be used with model_id and cannot be combined with model."`
	ModelID        string `json:"model_id,omitempty" jsonschema:"Optional model id for compatibility. Must be used with provider_id and cannot be combined with model. Model is fixed for the lifetime of the session."`
	Agent          string `json:"agent,omitempty" jsonschema:"Optional opencode agent name. Overrides the configured default agent."`
	PermissionMode string `json:"permission_mode,omitempty" jsonschema:"Optional permission mode: inherit, ask, deny, or yolo. Overrides the configured default for this session."`
	IsolationMode  string `json:"isolation_mode,omitempty" jsonschema:"Optional workspace isolation: auto, worktree, or none. Auto uses a worktree for clean Git projects and runs directly otherwise."`
}

func createSessionHandler(client *Client, workspaces *workspace.Manager, opts ExecutorOptions) mcp.ToolHandlerFor[createSessionParams, CreateSessionResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args createSessionParams) (*mcp.CallToolResult, CreateSessionResult, error) {
		req, err := opts.sessionRequest(args)
		if err != nil {
			return nil, CreateSessionResult{}, err
		}
		var mode workspace.Mode
		if strings.TrimSpace(args.IsolationMode) != "" {
			mode, err = workspace.ParseMode(args.IsolationMode)
			if err != nil {
				return nil, CreateSessionResult{}, err
			}
		}
		loc := args.location()
		var session Session
		var record *workspace.Record
		if workspaces == nil {
			session, err = client.CreateSession(ctx, loc, req)
		} else {
			created, openErr := workspaces.Open(ctx, loc.Directory, mode, func(ctx context.Context, directory string) (string, error) {
				loc.Directory = directory
				session, err = client.CreateSession(ctx, loc, req)
				return session.ID, err
			})
			err = openErr
			if openErr == nil {
				record = &created
			}
		}
		if err != nil {
			return nil, CreateSessionResult{}, err
		}
		model := ModelRef{ProviderID: req.ProviderID, ModelID: req.ModelID}
		return nil, CreateSessionResult{
			SessionID:      session.ID,
			Title:          session.Title,
			Model:          model.String(),
			Agent:          req.Agent,
			PermissionMode: string(req.Permission),
			Workspace:      record,
		}, nil
	}
}

type fireParams struct {
	locationParams
	SessionID      string `json:"session_id" jsonschema:"Session id returned by handoff_create_session."`
	Prompt         string `json:"prompt" jsonschema:"Task to delegate to opencode."`
	Agent          string `json:"agent,omitempty" jsonschema:"Optional opencode agent name."`
	Verbose        bool   `json:"verbose,omitempty" jsonschema:"Include raw messages/context returned by opencode."`
	WaitSeconds    int    `json:"wait_seconds,omitempty" jsonschema:"Max seconds to wait for completion (0-300). 0 = fire and return immediately."`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional retry key scoped to this session. Reusing it with the same request returns the existing job; reusing it with different input is rejected."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Execution deadline in seconds. Zero uses the server default. Maximum 86400 seconds."`
}

func fireHandler(client *Client, mgr *Manager, workspaces *workspace.Manager, opts ExecutorOptions) mcp.ToolHandlerFor[fireParams, HandoffFireResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args fireParams) (*mcp.CallToolResult, HandoffFireResult, error) {
		if args.SessionID == "" {
			return nil, HandoffFireResult{}, fmt.Errorf("session_id is required; use handoff_create_session first")
		}
		if args.Prompt == "" {
			return nil, HandoffFireResult{}, fmt.Errorf("prompt is required")
		}

		submitOpts, err := submitOptions(args)
		if err != nil {
			return nil, HandoffFireResult{}, err
		}
		loc := sessionLocation(workspaces, args.SessionID, args.location())
		submission, err := mgr.SubmitJob(ctx, loc, args.SessionID, opts.promptRequest(args), submitOpts)
		if err != nil {
			return nil, HandoffFireResult{}, err
		}

		job := submission.Job
		res := HandoffFireResult{
			SessionID:       job.SessionID,
			Status:          string(job.Status),
			IdempotencyKey:  job.IdempotencyKey,
			Duplicate:       submission.Duplicate,
			Deadline:        formatDeadline(job.Deadline),
			PromptMessageID: job.PromptMessageID,
		}

		if args.WaitSeconds > 0 {
			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(min(args.WaitSeconds, 300))*time.Second)
			defer cancel()
			_ = client.Wait(waitCtx, loc, job.SessionID)
			if current, ok := mgr.Job(job.SessionID); ok {
				res.Status = string(current.Status)
				res.PromptMessageID = current.PromptMessageID
			}
		}

		pending := fetchPending(ctx, client, loc, job.SessionID)
		res.PendingPermissions = pending.Permissions
		res.PendingQuestions = pending.Questions
		res.Errors = pending.Errors

		if submission.Duplicate {
			res.Message = fmt.Sprintf("duplicate submission; returning existing job with status %s", job.Status)
		} else if len(res.PendingPermissions) > 0 || len(res.PendingQuestions) > 0 {
			res.Message = "prompt submitted; pending action; use handoff_check,handoff_permission_reply,handoff_question_reply"
		} else {
			res.Message = "prompt submitted; use handoff_check with this session_id to monitor progress"
		}

		return nil, res, nil
	}
}

func submitOptions(args fireParams) (SubmitOptions, error) {
	if args.TimeoutSeconds < 0 {
		return SubmitOptions{}, fmt.Errorf("timeout_seconds must not be negative")
	}
	if args.TimeoutSeconds > maxJobTimeoutSeconds {
		return SubmitOptions{}, fmt.Errorf("timeout_seconds must not exceed %d", maxJobTimeoutSeconds)
	}
	return SubmitOptions{
		IdempotencyKey: args.IdempotencyKey,
		Timeout:        time.Duration(args.TimeoutSeconds) * time.Second,
	}, nil
}

type checkParams struct {
	locationParams
	SessionID   string `json:"session_id" jsonschema:"opencode session id."`
	Verbose     bool   `json:"verbose,omitempty" jsonschema:"Include raw messages/context returned by opencode."`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Max seconds to wait for completion (0-300). 0 = no wait."`
}

func checkHandler(client *Client, mgr *Manager, workspaces *workspace.Manager) mcp.ToolHandlerFor[checkParams, HandoffCheckResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args checkParams) (*mcp.CallToolResult, HandoffCheckResult, error) {
		loc := sessionLocation(workspaces, args.SessionID, args.location())
		if args.WaitSeconds > 0 {
			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(min(args.WaitSeconds, 300))*time.Second)
			defer cancel()
			// Prefer the local job store's completion signal for tracked jobs.
			// OpenCode's session wait can return while the executor job is still
			// running (e.g. during prompt submission), which made wait_seconds a no-op.
			if _, tracked := mgr.Job(args.SessionID); tracked {
				_ = mgr.Wait(waitCtx, args.SessionID)
			} else {
				_ = client.Wait(waitCtx, loc, args.SessionID)
			}
		}

		args.Directory = loc.Directory
		res, err := doCheck(ctx, client, mgr, args)
		if err == nil && workspaces != nil {
			report, ok, inspectErr := workspaces.Inspect(ctx, args.SessionID)
			if inspectErr != nil {
				res.WorkspaceError = inspectErr.Error()
			} else if ok {
				compactWorkspaceReport(&report, 100, 20, 5, false)
				res.Workspace = &report
			}
		}
		return nil, res, err
	}
}

func doCheck(ctx context.Context, client *Client, mgr *Manager, args checkParams) (HandoffCheckResult, error) {
	loc := args.location()
	_, tracked := mgr.Job(args.SessionID)

	res, isFinished, err := checkSession(ctx, client, loc, args.SessionID, args.Verbose)
	if err != nil {
		return HandoffCheckResult{}, err
	}

	// No tracked job (external session or different server instance): report
	// whatever opencode says and surface any pending permissions/questions.
	if !tracked {
		res.Status = sessionStatus(isFinished)
		return res, nil
	}

	job, _ := mgr.Reconcile(args.SessionID, isFinished)
	res.Status = string(job.Status)
	res.IdempotencyKey = job.IdempotencyKey
	res.Deadline = formatDeadline(job.Deadline)
	if job.Err != nil {
		res.JobError = job.Err.Error()
	}
	return res, nil
}

type cancelParams struct {
	locationParams
	SessionID string `json:"session_id" jsonschema:"opencode session id to cancel."`
}

func cancelHandler(mgr *Manager, workspaces *workspace.Manager) mcp.ToolHandlerFor[cancelParams, HandoffCancelResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args cancelParams) (*mcp.CallToolResult, HandoffCancelResult, error) {
		result, err := mgr.Cancel(ctx, sessionLocation(workspaces, args.SessionID, args.location()), args.SessionID)
		if err != nil {
			return nil, HandoffCancelResult{}, err
		}
		message := "session execution was already in a terminal state"
		if result.Aborted {
			message = "session execution canceled"
		} else if result.Job.Status == JobCanceled {
			message = "opencode reported no active execution; session is marked canceled"
		}
		return nil, HandoffCancelResult{
			SessionID: result.Job.SessionID,
			Status:    string(result.Job.Status),
			Aborted:   result.Aborted,
			Message:   message,
		}, nil
	}
}

type workspaceInspectParams struct {
	SessionID                 string `json:"session_id" jsonschema:"opencode session id."`
	ChangedFileLimit          int    `json:"changed_file_limit,omitempty" jsonschema:"Maximum changed files to return. Defaults to 100; maximum 1000."`
	CommitLimit               int    `json:"commit_limit,omitempty" jsonschema:"Maximum commits to return. Defaults to 50; maximum 200."`
	VerificationLimit         int    `json:"verification_limit,omitempty" jsonschema:"Maximum recent verification results to return. Defaults to 10; maximum 50."`
	IncludeVerificationOutput bool   `json:"include_verification_output,omitempty" jsonschema:"Include bounded stdout/stderr recorded for verification checks."`
}

func workspaceInspectHandler(workspaces *workspace.Manager) mcp.ToolHandlerFor[workspaceInspectParams, WorkspaceInspectResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args workspaceInspectParams) (*mcp.CallToolResult, WorkspaceInspectResult, error) {
		if workspaces == nil {
			return nil, WorkspaceInspectResult{}, fmt.Errorf("workspace management is unavailable")
		}
		changedLimit, err := boundedLimit("changed_file_limit", args.ChangedFileLimit, 100, 1000)
		if err != nil {
			return nil, WorkspaceInspectResult{}, err
		}
		commitLimit, err := boundedLimit("commit_limit", args.CommitLimit, 50, 200)
		if err != nil {
			return nil, WorkspaceInspectResult{}, err
		}
		verificationLimit, err := boundedLimit("verification_limit", args.VerificationLimit, 10, 50)
		if err != nil {
			return nil, WorkspaceInspectResult{}, err
		}
		report, ok, err := workspaces.Inspect(ctx, args.SessionID)
		if err != nil {
			return nil, WorkspaceInspectResult{}, err
		}
		if !ok {
			return nil, WorkspaceInspectResult{}, fmt.Errorf("no workspace tracked for session %s", args.SessionID)
		}
		compactWorkspaceReport(&report, changedLimit, commitLimit, verificationLimit, args.IncludeVerificationOutput)
		return nil, WorkspaceInspectResult{SessionID: args.SessionID, Workspace: report}, nil
	}
}

type workspaceDiffParams struct {
	SessionID string `json:"session_id" jsonschema:"opencode session id."`
	MaxChars  int    `json:"max_chars,omitempty" jsonschema:"Maximum diff characters to return. Defaults to 20000; maximum 100000."`
}

func workspaceDiffHandler(workspaces *workspace.Manager) mcp.ToolHandlerFor[workspaceDiffParams, workspace.Diff] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args workspaceDiffParams) (*mcp.CallToolResult, workspace.Diff, error) {
		if workspaces == nil {
			return nil, workspace.Diff{}, fmt.Errorf("workspace management is unavailable")
		}
		limit, err := boundedLimit("max_chars", args.MaxChars, 20_000, maxDiffChars)
		if err != nil {
			return nil, workspace.Diff{}, err
		}
		diff, err := workspaces.Diff(ctx, args.SessionID, limit)
		return nil, diff, err
	}
}

type workspaceVerifyParams struct {
	SessionID      string                        `json:"session_id" jsonschema:"opencode session id."`
	Checks         []workspace.VerificationCheck `json:"checks" jsonschema:"Verification checks. Each command is an executable plus an argv array; shell syntax is not interpreted."`
	TimeoutSeconds int                           `json:"timeout_seconds,omitempty" jsonschema:"Overall verification timeout in seconds. Defaults to 600; maximum 3600."`
}

func workspaceVerifyHandler(mgr *Manager, workspaces *workspace.Manager) mcp.ToolHandlerFor[workspaceVerifyParams, WorkspaceVerifyResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args workspaceVerifyParams) (*mcp.CallToolResult, WorkspaceVerifyResult, error) {
		if workspaces == nil {
			return nil, WorkspaceVerifyResult{}, fmt.Errorf("workspace management is unavailable")
		}
		if err := requireInactiveJob(mgr, args.SessionID, "verify"); err != nil {
			return nil, WorkspaceVerifyResult{}, err
		}
		timeout, err := boundedLimit("timeout_seconds", args.TimeoutSeconds, 600, maxVerificationTimeoutSeconds)
		if err != nil {
			return nil, WorkspaceVerifyResult{}, err
		}
		verifyCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		results, err := workspaces.Verify(verifyCtx, args.SessionID, args.Checks)
		if err != nil {
			return nil, WorkspaceVerifyResult{}, err
		}
		passed := len(results) > 0
		for _, result := range results {
			passed = passed && result.Status == "passed"
		}
		return nil, WorkspaceVerifyResult{SessionID: args.SessionID, Passed: passed, Results: results}, nil
	}
}

type workspaceCleanupParams struct {
	SessionID string `json:"session_id" jsonschema:"opencode session id."`
	Force     bool   `json:"force,omitempty" jsonschema:"Discard uncommitted changes and commits. Defaults to false."`
}

func workspaceCleanupHandler(mgr *Manager, workspaces *workspace.Manager) mcp.ToolHandlerFor[workspaceCleanupParams, workspace.CleanupResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args workspaceCleanupParams) (*mcp.CallToolResult, workspace.CleanupResult, error) {
		if workspaces == nil {
			return nil, workspace.CleanupResult{}, fmt.Errorf("workspace management is unavailable")
		}
		if err := requireInactiveJob(mgr, args.SessionID, "clean up"); err != nil {
			return nil, workspace.CleanupResult{}, err
		}
		result, err := workspaces.Cleanup(ctx, args.SessionID, args.Force)
		return nil, result, err
	}
}

func requireInactiveJob(mgr *Manager, sessionID, action string) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("session_id is required")
	}
	if mgr == nil {
		return nil
	}
	if job, ok := mgr.Job(sessionID); ok && !job.Status.terminal() {
		return fmt.Errorf("cannot %s workspace while session %s has an active job", action, sessionID)
	}
	return nil
}

func boundedLimit(name string, value, defaultValue, maximum int) (int, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	if value == 0 {
		return defaultValue, nil
	}
	if value > maximum {
		return 0, fmt.Errorf("%s must not exceed %d", name, maximum)
	}
	return value, nil
}

func compactWorkspaceReport(report *workspace.Report, changedLimit, commitLimit, verificationLimit int, includeVerificationOutput bool) {
	if len(report.ChangedFiles) > changedLimit {
		report.ChangedFiles = report.ChangedFiles[:changedLimit]
	}
	if len(report.Commits) > commitLimit {
		report.Commits = report.Commits[:commitLimit]
	}
	if len(report.Verification) > verificationLimit {
		report.Verification = report.Verification[len(report.Verification)-verificationLimit:]
	}
	if !includeVerificationOutput {
		for index := range report.Verification {
			report.Verification[index].Output = ""
		}
	}
}

func formatDeadline(deadline time.Time) string {
	if deadline.IsZero() {
		return ""
	}
	return deadline.UTC().Format(time.RFC3339Nano)
}

// sessionStatus maps the isFinished flag to a status string for sessions that
// are not tracked by this server instance.
func sessionStatus(isFinished bool) string {
	if isFinished {
		return string(JobDone)
	}
	return string(JobRunning)
}

type permissionReplyParams struct {
	locationParams
	SessionID string `json:"session_id" jsonschema:"opencode session id."`
	Reply     string `json:"reply" jsonschema:"permission reply value, for example once, always, reject, or deny depending on opencode API."`
	Message   string `json:"message,omitempty" jsonschema:"Optional explanation."`
}

func permissionReplyHandler(client *Client, workspaces *workspace.Manager) mcp.ToolHandlerFor[permissionReplyParams, PermissionReplyResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args permissionReplyParams) (*mcp.CallToolResult, PermissionReplyResult, error) {
		loc := sessionLocation(workspaces, args.SessionID, args.location())
		reqs, err := client.SessionPermissionRequests(ctx, loc, args.SessionID)
		if err != nil {
			return nil, PermissionReplyResult{}, err
		}
		if len(reqs) == 0 {
			return nil, PermissionReplyResult{}, fmt.Errorf("no pending permission requests found for session %s", args.SessionID)
		}
		requestID := ""
		if idVal, ok := reqs[0]["id"].(string); ok {
			requestID = idVal
		} else if idVal, ok := reqs[0]["requestID"].(string); ok {
			requestID = idVal
		} else if idVal, ok := reqs[0]["request_id"].(string); ok {
			requestID = idVal
		}
		if requestID == "" {
			return nil, PermissionReplyResult{}, fmt.Errorf("could not extract request ID from pending permission")
		}

		raw, err := client.PermissionReply(ctx, loc, args.SessionID, requestID, args.Reply, args.Message)
		if err != nil {
			return nil, PermissionReplyResult{}, err
		}

		res := PermissionReplyResult{OK: true, Data: raw}
		pending := fetchPending(ctx, client, loc, args.SessionID)
		res.PendingPermissions = pending.Permissions
		res.PendingQuestions = pending.Questions
		res.Errors = pending.Errors
		return nil, res, nil
	}
}

type pendingRequests struct {
	Permissions []RequestSummary
	Questions   []RequestSummary
	Errors      []string
}

func fetchPending(ctx context.Context, client *Client, loc Location, sessionID string) pendingRequests {
	var out pendingRequests
	perms, err := client.Permissions(ctx, loc, sessionID)
	if err == nil {
		out.Permissions = summarizeRequests(perms, "permission", sessionID)
	} else {
		out.Errors = append(out.Errors, fmt.Sprintf("get permission requests: %s", err))
	}
	questions, err := client.Questions(ctx, loc, sessionID)
	if err == nil {
		out.Questions = summarizeRequests(questions, "question", sessionID)
	} else {
		out.Errors = append(out.Errors, fmt.Sprintf("get questions: %s", err))
	}
	return out
}

type questionReplyParams struct {
	locationParams
	SessionID string     `json:"session_id" jsonschema:"opencode session id."`
	Answers   [][]string `json:"answers,omitempty" jsonschema:"Answer selections: each inner array is selected labels for one question."`
	Reject    bool       `json:"reject,omitempty" jsonschema:"Reject the question instead of answering it."`
}

func questionReplyHandler(client *Client, workspaces *workspace.Manager) mcp.ToolHandlerFor[questionReplyParams, QuestionReplyResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args questionReplyParams) (*mcp.CallToolResult, QuestionReplyResult, error) {
		loc := sessionLocation(workspaces, args.SessionID, args.location())
		reqs, err := client.SessionQuestionRequests(ctx, loc, args.SessionID)
		if err != nil {
			return nil, QuestionReplyResult{}, err
		}
		if len(reqs) == 0 {
			return nil, QuestionReplyResult{}, fmt.Errorf("no pending question requests found for session %s", args.SessionID)
		}
		requestID := ""
		if idVal, ok := reqs[0]["id"].(string); ok {
			requestID = idVal
		} else if idVal, ok := reqs[0]["requestID"].(string); ok {
			requestID = idVal
		} else if idVal, ok := reqs[0]["request_id"].(string); ok {
			requestID = idVal
		}
		if requestID == "" {
			return nil, QuestionReplyResult{}, fmt.Errorf("could not extract request ID from pending question")
		}

		raw, err := client.QuestionReply(ctx, loc, args.SessionID, requestID, args.Reject, args.Answers)
		if err != nil {
			return nil, QuestionReplyResult{}, err
		}

		res := QuestionReplyResult{OK: true, Data: raw}
		pending := fetchPending(ctx, client, loc, args.SessionID)
		res.PendingPermissions = pending.Permissions
		res.PendingQuestions = pending.Questions
		res.Errors = pending.Errors
		return nil, res, nil
	}
}

func checkSession(ctx context.Context, client *Client, loc Location, sessionID string, verbose bool) (HandoffCheckResult, bool, error) {
	if sessionID == "" {
		return HandoffCheckResult{}, false, fmt.Errorf("session_id is required")
	}
	res := HandoffCheckResult{SessionID: sessionID}
	var isFinished bool
	msg, msgErr := client.Messages(ctx, loc, sessionID)
	if msgErr == nil {
		limit := 3
		if verbose {
			limit = 6
		}
		res.Messages = summarizeMessages(msg, limit)
		res.FinalText = truncateText(firstText(msg), 4000)
		isFinished = isSessionFinishedJSON(msg)
	}
	ctxData, ctxErr := client.Context(ctx, loc, sessionID)
	if ctxErr == nil {
		if res.FinalText == "" {
			res.FinalText = truncateText(firstText(ctxData), 4000)
		}
	}
	fillPendingRequests(ctx, client, loc, &res)
	if msgErr != nil && ctxErr != nil {
		return HandoffCheckResult{}, false, fmt.Errorf("read session %q messages: %w; context: %w", sessionID, msgErr, ctxErr)
	}
	return res, isFinished, nil
}

func fillPendingRequests(ctx context.Context, client *Client, loc Location, res *HandoffCheckResult) {
	pending := fetchPending(ctx, client, loc, res.SessionID)
	res.PendingPermissions = pending.Permissions
	res.PendingQuestions = pending.Questions
	res.Errors = append(res.Errors, pending.Errors...)
}

func isSessionFinishedJSON(raw json.RawMessage) bool {
	// Try v2 format: messages are objects with an "info" wrapper.
	var v2 []struct {
		Info struct {
			Role   string  `json:"role"`
			Finish *string `json:"finish"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &v2); err == nil {
		for _, msg := range slices.Backward(v2) {
			if msg.Info.Role == "assistant" {
				return msg.Info.Finish != nil && *msg.Info.Finish != ""
			}
		}
	}
	// Try flat format (instance route): role and finish are top-level fields.
	var flat []struct {
		Role   string  `json:"role"`
		Finish *string `json:"finish"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil {
		for _, msg := range slices.Backward(flat) {
			if msg.Role == "assistant" {
				return msg.Finish != nil && *msg.Finish != ""
			}
		}
	}
	return false
}
