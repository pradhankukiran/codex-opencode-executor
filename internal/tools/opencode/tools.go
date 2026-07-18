package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
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

// toolRegistrar binds a tool name to its MCP registration function.
// Catalog order is the single source of registration order (must match fullToolOrder).
type toolRegistrar struct {
	name string
	add  func(*mcp.Server)
}

// toolCatalog returns every handoff tool in deterministic full-set order.
// Membership filtering happens in Register via Toolset.Includes.
func toolCatalog(client *Client, mgr *Manager, workspaces *workspace.Manager, opts ExecutorOptions) []toolRegistrar {
	return []toolRegistrar{
		{name: ToolHandoffHealth, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffHealth,
				Description: "Check connectivity to the configured opencode HTTP server.",
				Flags:       mcputil.ReadOnly,
			}, healthHandler(client))
		}},
		{name: ToolHandoffAgents, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffAgents,
				Description: "List opencode agents available for a directory/workspace.",
				Flags:       mcputil.ReadOnly,
			}, agentsHandler(client))
		}},
		{name: ToolHandoffModels, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffModels,
				Description: "List providers and optionally models. Each model has canonical_model (provider_id/model_id, embedded slashes kept) for handoff_create_session.model or execute model. Gateway vs direct are distinct exact selectors (e.g. vercel/xai/grok-4.5 vs xai/grok-4.5). filter: substring, glob, or regex; limit caps models (default 50).",
				Flags:       mcputil.ReadOnly,
			}, modelsHandler(client))
		}},
		{name: ToolHandoffSessions, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffSessions,
				Description: "List opencode sessions visible in a directory/workspace.",
				Flags:       mcputil.ReadOnly,
			}, sessionsHandler(client))
		}},
		{name: ToolHandoffCreateSession, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffCreateSession,
				Description: "Create a session; returns session_id for handoff_fire. Uses configured defaults unless overridden. Auto isolation: worktree for clean Git, else direct. create_directory=true creates a missing exact path as greenfield (parent must exist; existing paths refused).",
			}, createSessionHandler(client, workspaces, opts))
		}},
		{name: ToolHandoffFire, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffFire,
				Description: "Submit a prompt to an existing session and return immediately. Requires session_id from handoff_create_session. Poll with handoff_check.",
			}, fireHandler(client, mgr, workspaces, opts))
		}},
		{name: ToolHandoffExecute, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name: ToolHandoffExecute,
				Description: "Create session, submit prompt, wait (default 300s) or async=true for immediate return after submit. " +
					"Wait expiry returns status=running with session_id for handoff_check (not an error). " +
					"Profiles add server-owned suffixes without replacing the task. " +
					"idempotency_key is submission-only after session create — not whole-call retry-idempotent.",
			}, executeHandler(client, mgr, workspaces, opts))
		}},
		{name: ToolHandoffCheck, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffCheck,
				Description: "Poll session progress. Omits workspace unless include_workspace=true. final_text is terminal-only (default 1200 chars).",
				Flags:       mcputil.ReadOnly,
			}, checkHandler(client, mgr, workspaces))
		}},
		{name: ToolHandoffReview, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffReview,
				Description: "Compact workspace review: changed files, commits, diff stat, verification (no paths/metadata/diffs).",
				Flags:       mcputil.ReadOnly,
			}, reviewHandler(workspaces))
		}},
		{name: ToolHandoffCancel, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffCancel,
				Description: "Cancel active execution for a session. Idempotent when already terminal.",
				Flags:       mcputil.Destructive,
			}, cancelHandler(mgr, workspaces))
		}},
		{name: ToolHandoffWorkspace, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffWorkspace,
				Description: "Inspect the durable workspace bound to a session: changed files, commits, diff stats, verification. Greenfield is available before Git init and uses the empty tree after history exists.",
				Flags:       mcputil.ReadOnly,
			}, workspaceInspectHandler(workspaces))
		}},
		{name: ToolHandoffDiff, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffDiff,
				Description: "Bounded tracked-file Git diff vs session start (or empty tree for greenfield). Untracked names: handoff_workspace.",
				Flags:       mcputil.ReadOnly,
			}, workspaceDiffHandler(workspaces))
		}},
		{name: ToolHandoffVerify, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffVerify,
				Description: "Run executable+argv verification checks in a completed session workspace and record pass/fail. No shell expansion.",
				Flags:       mcputil.Destructive,
			}, workspaceVerifyHandler(mgr, workspaces))
		}},
		{name: ToolHandoffCleanup, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffCleanup,
				Description: "Remove an executor-owned workspace (worktree or greenfield). Clean owned workspaces remove by default; force=true discards changes/commits. Idempotent.",
				Flags:       mcputil.Destructive,
			}, workspaceCleanupHandler(mgr, workspaces))
		}},
		{name: ToolHandoffPermissionReply, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffPermissionReply,
				Description: "Reply to a pending permission request. See handoff_check for pending permissions.",
			}, permissionReplyHandler(client, workspaces))
		}},
		{name: ToolHandoffQuestionReply, add: func(s *mcp.Server) {
			mcputil.Register(s, mcputil.ToolDef{
				Name:        ToolHandoffQuestionReply,
				Description: "Reply to or reject a pending clarification question. See handoff_check for pending questions.",
			}, questionReplyHandler(client, workspaces))
		}},
	}
}

// Register adds opencode handoff tools for the given toolset to an MCP server.
func Register(s *mcp.Server, client *Client, mgr *Manager, workspaces *workspace.Manager, opts ExecutorOptions, toolset Toolset) {
	for _, tool := range toolCatalog(client, mgr, workspaces, opts) {
		if !toolset.Includes(tool.name) {
			continue
		}
		tool.add(s)
	}
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
	IncludeModels bool   `json:"include_models,omitempty" jsonschema:"Include individual models. Default false."`
	Filter        string `json:"filter,omitempty" jsonschema:"Substring, glob, or regex (e.g. 'xai/', 'openai/gpt-*-mini'). Implies include_models; providers trimmed to final models."`
	Limit         int    `json:"limit,omitempty" jsonschema:"Max models when include_models. Default 50; -1 unlimited; 0=default."`
}

func modelsHandler(client *Client) mcp.ToolHandlerFor[modelsParams, ModelsResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args modelsParams) (*mcp.CallToolResult, ModelsResult, error) {
		res, err := client.ProvidersAndModels(ctx, args.location())
		if err != nil {
			return nil, ModelsResult{}, err
		}
		return nil, shapeModelsResult(res, args), nil
	}
}

// shapeModelsResult applies include/filter/limit and, when filter is set,
// retains only provider summaries referenced by the final model list.
func shapeModelsResult(res ModelsResult, args modelsParams) ModelsResult {
	includeModels := args.IncludeModels || args.Filter != ""
	if !includeModels {
		res.Models = nil
		return res
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

	if args.Filter != "" {
		res.Providers = providersRepresentedByModels(res.Providers, res.Models)
	}

	return res
}

func providersRepresentedByModels(providers []ProviderSummary, models []ModelSummary) []ProviderSummary {
	counts := make(map[string]int, len(models))
	for _, m := range models {
		counts[m.ProviderID]++
	}
	out := make([]ProviderSummary, 0, len(counts))
	for _, p := range providers {
		n, ok := counts[p.ID]
		if !ok {
			continue
		}
		p.Models = n
		out = append(out, p)
	}
	return out
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
	Title           string `json:"title,omitempty" jsonschema:"Optional session title."`
	ParentID        string `json:"parent_id,omitempty" jsonschema:"Optional parent session ID."`
	Model           string `json:"model,omitempty" jsonschema:"Optional provider/model (e.g. 'xai/grok-4.5'). Overrides default; fixed for session lifetime."`
	ProviderID      string `json:"provider_id,omitempty" jsonschema:"Optional provider id; requires model_id; cannot combine with model."`
	ModelID         string `json:"model_id,omitempty" jsonschema:"Optional model id; requires provider_id; cannot combine with model. Fixed for session lifetime."`
	Agent           string `json:"agent,omitempty" jsonschema:"Optional agent name; overrides configured default."`
	PermissionMode  string `json:"permission_mode,omitempty" jsonschema:"Optional: inherit, ask, deny, or yolo. Overrides session default."`
	IsolationMode   string `json:"isolation_mode,omitempty" jsonschema:"auto, worktree, or none. Auto: worktree for clean Git else direct. Ignored with create_directory greenfield."`
	CreateDirectory bool   `json:"create_directory,omitempty" jsonschema:"Create missing exact directory as greenfield (parent must exist; existing paths refused including empty dirs/symlinks). Default false."`
}

func createSessionHandler(client *Client, workspaces *workspace.Manager, opts ExecutorOptions) mcp.ToolHandlerFor[createSessionParams, CreateSessionResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args createSessionParams) (*mcp.CallToolResult, CreateSessionResult, error) {
		res, err := createSession(ctx, client, workspaces, opts, args)
		return nil, res, err
	}
}

// createSession opens an optional workspace binding and creates an opencode session.
// Shared by handoff_create_session and handoff_execute so create semantics stay singular.
func createSession(ctx context.Context, client *Client, workspaces *workspace.Manager, opts ExecutorOptions, args createSessionParams) (CreateSessionResult, error) {
	req, err := opts.sessionRequest(args)
	if err != nil {
		return CreateSessionResult{}, err
	}
	var mode workspace.Mode
	if strings.TrimSpace(args.IsolationMode) != "" {
		mode, err = workspace.ParseMode(args.IsolationMode)
		if err != nil {
			return CreateSessionResult{}, err
		}
	}
	loc := args.location()
	var session Session
	var record *workspace.Record
	if workspaces == nil {
		if args.CreateDirectory {
			return CreateSessionResult{}, fmt.Errorf("create_directory requires workspace management")
		}
		session, err = client.CreateSession(ctx, loc, req)
	} else {
		created, openErr := workspaces.Open(ctx, workspace.OpenOptions{
			Directory:       loc.Directory,
			Mode:            mode,
			CreateDirectory: args.CreateDirectory,
		}, func(ctx context.Context, directory string) (string, error) {
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
		return CreateSessionResult{}, err
	}
	model := ModelRef{ProviderID: req.ProviderID, ModelID: req.ModelID}
	return CreateSessionResult{
		SessionID:      session.ID,
		Title:          session.Title,
		Model:          model.String(),
		Agent:          req.Agent,
		PermissionMode: string(req.Permission),
		Workspace:      record,
	}, nil
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
		submission, loc, err := submitFire(ctx, mgr, workspaces, opts, args)
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
			// Wait on the local job lifecycle. OpenCode session wait can return
			// while SubmitJob's prompt goroutine is still running.
			_ = mgr.Wait(waitCtx, job.SessionID)
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

// submitFire validates and submits a prompt job for an existing session.
// Shared by handoff_fire and handoff_execute so fire semantics stay singular.
func submitFire(ctx context.Context, mgr *Manager, workspaces *workspace.Manager, opts ExecutorOptions, args fireParams) (SubmitResult, Location, error) {
	if args.SessionID == "" {
		return SubmitResult{}, Location{}, fmt.Errorf("session_id is required; use handoff_create_session first")
	}
	if args.Prompt == "" {
		return SubmitResult{}, Location{}, fmt.Errorf("prompt is required")
	}
	submitOpts, err := submitOptions(args)
	if err != nil {
		return SubmitResult{}, Location{}, err
	}
	loc := sessionLocation(workspaces, args.SessionID, args.location())
	submission, err := mgr.SubmitJob(ctx, loc, args.SessionID, opts.promptRequest(args), submitOpts)
	if err != nil {
		return SubmitResult{}, Location{}, err
	}
	return submission, loc, nil
}

const (
	defaultExecuteWaitSeconds = 300
	maxExecuteWaitSeconds     = 300

	executeProfileImplementation = "implementation"
	executeProfileDiagnosis      = "diagnosis"
	executeProfileReview         = "review"
	executeProfileNone           = "none"

	executeProfileSuffixImplementation = "Follow repository instructions. Make the requested changes. Run relevant validation. Commit cohesive logical units. Finish with concise status, commits, files, tests, and blockers."
	executeProfileSuffixDiagnosis      = "Read-only: do not mutate files, git state, or configuration. Diagnose only. Report concise evidence."
	executeProfileSuffixReview         = "Read-only: do not mutate files, git state, or configuration. Review only. Report concise evidence."
)

type executeParams struct {
	createSessionParams
	Prompt         string `json:"prompt" jsonschema:"Task text. Server may append profile suffix; never replaces this."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Execution deadline seconds. 0=server default; max 86400."`
	// WaitSeconds uses a pointer so omitted (default 300) differs from explicit 0.
	WaitSeconds    *int   `json:"wait_seconds,omitempty" jsonschema:"Wait after submit 0-300s; omit=300. Use async=true for immediate return."`
	Async          bool   `json:"async,omitempty" jsonschema:"Immediate return after submit (wait_seconds=0). Poll with handoff_check."`
	MaxFinalChars  int    `json:"max_final_chars,omitempty" jsonschema:"Max final_text chars. Default 1200; max 4000."`
	Profile        string `json:"profile,omitempty" jsonschema:"implementation (default), diagnosis, review, or none (prompt unchanged)."`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Retry key for job submit after session create only; not whole-call idempotent (retry makes new session)."`
	SkipReview     bool   `json:"skip_review,omitempty" jsonschema:"Omit compact review after terminal completion."`
}

// resolveExecuteWaitSeconds maps async/wait_seconds to a concrete wait budget.
// Omitted wait_seconds defaults to 300; explicit values are bounded to 0-300.
// async forces an immediate return (0). Exposed for tests without sleeping.
func resolveExecuteWaitSeconds(async bool, waitSeconds *int) (int, error) {
	if async {
		return 0, nil
	}
	if waitSeconds == nil {
		return defaultExecuteWaitSeconds, nil
	}
	if *waitSeconds < 0 || *waitSeconds > maxExecuteWaitSeconds {
		return 0, fmt.Errorf("wait_seconds must be between 0 and %d", maxExecuteWaitSeconds)
	}
	return *waitSeconds, nil
}

func applyExecuteProfile(prompt, profile string) (string, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		profile = executeProfileImplementation
	}
	switch profile {
	case executeProfileNone:
		return prompt, nil
	case executeProfileImplementation:
		return prompt + "\n\n" + executeProfileSuffixImplementation, nil
	case executeProfileDiagnosis:
		return prompt + "\n\n" + executeProfileSuffixDiagnosis, nil
	case executeProfileReview:
		return prompt + "\n\n" + executeProfileSuffixReview, nil
	default:
		return "", fmt.Errorf("profile must be one of: implementation, diagnosis, review, none")
	}
}

// preparedExecute holds fully validated execute inputs so createSession is never
// called with invalid arguments (no leftover workspace/session on bad input).
type preparedExecute struct {
	create        createSessionParams
	prompt        string
	waitSeconds   int
	maxFinalChars int
	fire          fireParams
}

func prepareExecute(args executeParams, workspaces *workspace.Manager, opts ExecutorOptions) (preparedExecute, error) {
	if strings.TrimSpace(args.Prompt) == "" {
		return preparedExecute{}, fmt.Errorf("prompt is required")
	}
	waitSeconds, err := resolveExecuteWaitSeconds(args.Async, args.WaitSeconds)
	if err != nil {
		return preparedExecute{}, err
	}
	maxFinalChars, err := boundedLimit("max_final_chars", args.MaxFinalChars, defaultFinalTextChars, maxFinalTextChars)
	if err != nil {
		return preparedExecute{}, err
	}
	prompt, err := applyExecuteProfile(args.Prompt, args.Profile)
	if err != nil {
		return preparedExecute{}, err
	}
	if args.CreateDirectory && workspaces == nil {
		return preparedExecute{}, fmt.Errorf("create_directory requires workspace management")
	}
	if _, err := opts.sessionRequest(args.createSessionParams); err != nil {
		return preparedExecute{}, err
	}
	if strings.TrimSpace(args.IsolationMode) != "" {
		if _, err := workspace.ParseMode(args.IsolationMode); err != nil {
			return preparedExecute{}, err
		}
	}
	fire := fireParams{
		locationParams: args.locationParams,
		Prompt:         prompt,
		Agent:          args.Agent,
		IdempotencyKey: args.IdempotencyKey,
		TimeoutSeconds: args.TimeoutSeconds,
	}
	if _, err := submitOptions(fire); err != nil {
		return preparedExecute{}, err
	}
	return preparedExecute{
		create:        args.createSessionParams,
		prompt:        prompt,
		waitSeconds:   waitSeconds,
		maxFinalChars: maxFinalChars,
		fire:          fire,
	}, nil
}

func executeHandler(client *Client, mgr *Manager, workspaces *workspace.Manager, opts ExecutorOptions) mcp.ToolHandlerFor[executeParams, HandoffExecuteResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args executeParams) (*mcp.CallToolResult, HandoffExecuteResult, error) {
		prepared, err := prepareExecute(args, workspaces, opts)
		if err != nil {
			return nil, HandoffExecuteResult{}, err
		}

		created, err := createSession(ctx, client, workspaces, opts, prepared.create)
		if err != nil {
			return nil, HandoffExecuteResult{}, err
		}

		fireArgs := prepared.fire
		fireArgs.SessionID = created.SessionID
		if fireArgs.Agent == "" {
			fireArgs.Agent = created.Agent
		}
		_, loc, err := submitFire(ctx, mgr, workspaces, opts, fireArgs)
		if err != nil {
			return nil, HandoffExecuteResult{}, err
		}

		if prepared.waitSeconds > 0 {
			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(prepared.waitSeconds)*time.Second)
			defer cancel()
			waitForHandoffCheck(waitCtx, client, mgr, loc, created.SessionID)
		}

		checkArgs := checkParams{
			locationParams: locationParams{Directory: loc.Directory, Workspace: loc.Workspace},
			SessionID:      created.SessionID,
			MaxFinalChars:  prepared.maxFinalChars,
		}
		checked, err := doCheck(ctx, client, mgr, checkArgs)
		if err != nil {
			return nil, HandoffExecuteResult{}, err
		}

		out := HandoffExecuteResult{
			SessionID:          created.SessionID,
			Status:             checked.Status,
			Model:              created.Model,
			Agent:              created.Agent,
			FinalText:          checked.FinalText,
			FinalTextTruncated: checked.FinalTextTruncated,
			PendingPermissions: checked.PendingPermissions,
			PendingQuestions:   checked.PendingQuestions,
			JobError:           checked.JobError,
			Errors:             checked.Errors,
		}

		if !args.SkipReview && JobStatus(checked.Status).terminal() {
			if review, ok := loadExecuteReview(ctx, workspaces, created.SessionID); ok {
				out.Review = &review
			}
		}
		return nil, out, nil
	}
}

func loadExecuteReview(ctx context.Context, workspaces *workspace.Manager, sessionID string) (HandoffReviewResult, bool) {
	if workspaces == nil {
		return HandoffReviewResult{}, false
	}
	report, ok, err := workspaces.Inspect(ctx, sessionID)
	if err != nil || !ok {
		return HandoffReviewResult{}, false
	}
	return shapeHandoffReview(sessionID, report, 20, 10, 5), true
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
	SessionID        string `json:"session_id" jsonschema:"opencode session id."`
	Verbose          bool   `json:"verbose,omitempty" jsonschema:"Include bounded raw messages. Default omits."`
	WaitSeconds      int    `json:"wait_seconds,omitempty" jsonschema:"Wait for completion 0-300s. 0=no wait."`
	IncludeWorkspace bool   `json:"include_workspace,omitempty" jsonschema:"Attach compact workspace report. Default omits."`
	MaxFinalChars    int    `json:"max_final_chars,omitempty" jsonschema:"Max final_text chars. Default 1200; max 4000."`
}

func checkHandler(client *Client, mgr *Manager, workspaces *workspace.Manager) mcp.ToolHandlerFor[checkParams, HandoffCheckResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args checkParams) (*mcp.CallToolResult, HandoffCheckResult, error) {
		if _, err := boundedLimit("max_final_chars", args.MaxFinalChars, defaultFinalTextChars, maxFinalTextChars); err != nil {
			return nil, HandoffCheckResult{}, err
		}
		loc := sessionLocation(workspaces, args.SessionID, args.location())
		if args.WaitSeconds > 0 {
			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(min(args.WaitSeconds, 300))*time.Second)
			defer cancel()
			waitForHandoffCheck(waitCtx, client, mgr, loc, args.SessionID)
		}

		args.Directory = loc.Directory
		res, err := doCheck(ctx, client, mgr, args)
		if err == nil && args.IncludeWorkspace && workspaces != nil {
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

// waitForHandoffCheck blocks until a tracked job is truly complete for check
// purposes, or the wait context expires.
//
// Local Manager JobDone can race ahead of OpenCode's agent loop (latest assistant
// finish still "tool-calls"/"unknown"). In that case we keep polling OpenCode
// until the latest assistant turn is terminal rather than returning early.
func waitForHandoffCheck(ctx context.Context, client *Client, mgr *Manager, loc Location, sessionID string) {
	if _, tracked := mgr.Job(sessionID); !tracked {
		_ = client.Wait(ctx, loc, sessionID)
		return
	}

	_ = mgr.Wait(ctx, sessionID)
	if ctx.Err() != nil {
		return
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if job, ok := mgr.Job(sessionID); ok {
			switch job.Status {
			case JobError, JobCanceled, JobTimedOut:
				return
			}
		}
		msg, err := client.Messages(ctx, loc, sessionID)
		if err == nil {
			if isSessionFinishedJSON(msg) {
				return
			}
			// Local job may already be Done with empty messages (mocks) — no
			// OpenCode continuation signal, so waiting further is unnecessary.
			if !openCodeAssistantContinuing(msg) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func doCheck(ctx context.Context, client *Client, mgr *Manager, args checkParams) (HandoffCheckResult, error) {
	loc := args.location()
	_, tracked := mgr.Job(args.SessionID)

	res, msg, isFinished, err := checkSession(ctx, client, loc, args.SessionID)
	if err != nil {
		return HandoffCheckResult{}, err
	}

	maxFinalChars, err := boundedLimit("max_final_chars", args.MaxFinalChars, defaultFinalTextChars, maxFinalTextChars)
	if err != nil {
		return HandoffCheckResult{}, err
	}

	// No tracked job (external session or different server instance): report
	// whatever opencode says and surface any pending permissions/questions.
	if !tracked {
		res.Status = sessionStatus(isFinished)
		return shapeHandoffCheckResult(res, msg, args.Verbose, isFinished, maxFinalChars), nil
	}

	job, _ := mgr.Reconcile(args.SessionID, isFinished)
	status := job.Status
	// Authoritative completion requires a terminal OpenCode assistant finish.
	// Local JobDone from Prompt return must not stop Codex while tool-calls continue.
	if status == JobDone && !isFinished && openCodeAssistantContinuing(msg) {
		status = JobRunning
	}
	res.Status = string(status)
	res.IdempotencyKey = job.IdempotencyKey
	res.Deadline = formatDeadline(job.Deadline)
	if status != JobRunning && job.Err != nil {
		res.JobError = job.Err.Error()
	}
	return shapeHandoffCheckResult(res, msg, args.Verbose, isFinished, maxFinalChars), nil
}

type reviewParams struct {
	SessionID         string `json:"session_id" jsonschema:"opencode session id."`
	ChangedFileLimit  int    `json:"changed_file_limit,omitempty" jsonschema:"Max changed files. Default 20; max 100."`
	CommitLimit       int    `json:"commit_limit,omitempty" jsonschema:"Max commits. Default 10; max 50."`
	VerificationLimit int    `json:"verification_limit,omitempty" jsonschema:"Max recent verifications. Default 5; max 20."`
}

func reviewHandler(workspaces *workspace.Manager) mcp.ToolHandlerFor[reviewParams, HandoffReviewResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args reviewParams) (*mcp.CallToolResult, HandoffReviewResult, error) {
		if workspaces == nil {
			return nil, HandoffReviewResult{}, fmt.Errorf("workspace management is unavailable")
		}
		if strings.TrimSpace(args.SessionID) == "" {
			return nil, HandoffReviewResult{}, fmt.Errorf("session_id is required")
		}
		changedLimit, err := boundedLimit("changed_file_limit", args.ChangedFileLimit, 20, 100)
		if err != nil {
			return nil, HandoffReviewResult{}, err
		}
		commitLimit, err := boundedLimit("commit_limit", args.CommitLimit, 10, 50)
		if err != nil {
			return nil, HandoffReviewResult{}, err
		}
		verificationLimit, err := boundedLimit("verification_limit", args.VerificationLimit, 5, 20)
		if err != nil {
			return nil, HandoffReviewResult{}, err
		}
		report, ok, err := workspaces.Inspect(ctx, args.SessionID)
		if err != nil {
			return nil, HandoffReviewResult{}, err
		}
		if !ok {
			return nil, HandoffReviewResult{}, fmt.Errorf("no workspace tracked for session %s", args.SessionID)
		}
		return nil, shapeHandoffReview(args.SessionID, report, changedLimit, commitLimit, verificationLimit), nil
	}
}

func shapeHandoffReview(sessionID string, report workspace.Report, changedLimit, commitLimit, verificationLimit int) HandoffReviewResult {
	compactWorkspaceReport(&report, changedLimit, commitLimit, verificationLimit, false)
	out := HandoffReviewResult{
		SessionID:        sessionID,
		Available:        report.Available,
		Dirty:            report.Dirty,
		HasChanges:       report.HasChanges,
		ChangedFileCount: report.ChangedFileCount,
		ChangedFiles:     report.ChangedFiles,
		CommitCount:      report.CommitCount,
		Commits:          report.Commits,
		DiffStat:         report.DiffStat,
	}
	if len(report.Verification) > 0 {
		out.Verification = make([]ReviewVerification, 0, len(report.Verification))
		for _, item := range report.Verification {
			out.Verification = append(out.Verification, ReviewVerification{
				Name:     item.Name,
				Command:  item.Command,
				Status:   item.Status,
				ExitCode: item.ExitCode,
			})
		}
	}
	return out
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

// checkSession loads OpenCode session signals needed by handoff_check.
// It deliberately does not set final_text/messages; callers must run
// shapeHandoffCheckResult after status is known so running polls cannot leak
// partial assistant content into Codex context.
func checkSession(ctx context.Context, client *Client, loc Location, sessionID string) (HandoffCheckResult, json.RawMessage, bool, error) {
	if sessionID == "" {
		return HandoffCheckResult{}, nil, false, fmt.Errorf("session_id is required")
	}
	res := HandoffCheckResult{SessionID: sessionID}
	msg, msgErr := client.Messages(ctx, loc, sessionID)
	if msgErr != nil {
		fillPendingRequests(ctx, client, loc, &res)
		return HandoffCheckResult{}, nil, false, fmt.Errorf("read session %q messages: %w", sessionID, msgErr)
	}
	isFinished := isSessionFinishedJSON(msg)
	fillPendingRequests(ctx, client, loc, &res)
	return res, msg, isFinished, nil
}

func fillPendingRequests(ctx context.Context, client *Client, loc Location, res *HandoffCheckResult) {
	pending := fetchPending(ctx, client, loc, res.SessionID)
	res.PendingPermissions = pending.Permissions
	res.PendingQuestions = pending.Questions
	res.Errors = append(res.Errors, pending.Errors...)
}

func isSessionFinishedJSON(raw json.RawMessage) bool {
	finish, ok := latestAssistantFinish(raw)
	if !ok {
		return false
	}
	return isTerminalFinish(finish)
}
