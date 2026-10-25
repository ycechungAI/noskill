package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"noskill/internal/platform"
)

// TaskTypes are the router's task types (ORCHESTRATOR.md "Classification").
var TaskTypes = []string{"chat", "code", "reasoning", "math", "research", "summarise", "extract", "translate", "creative", "vision", "plan"}

// Needs are the hard needs a rule can match on.
var Needs = []string{"vision", "web", "tools", "context"}

// SecretChecker reports whether a secret exists in the OS keychain, without reading its value.
type SecretChecker interface {
	Has(name string) (bool, error)
}

// Check validates values (`noskill config check`). secrets may be nil to skip the keychain check.
func Check(l *Loaded, secrets SecretChecker) []Issue {
	c := l.Config
	issues := append([]Issue{}, l.Issues...)
	add := func(level, key, f string, a ...any) {
		issues = append(issues, Issue{Level: level, Key: key, Msg: fmt.Sprintf(f, a...)})
	}
	enum := func(key, v string, allowed ...string) {
		if !slices.Contains(allowed, v) {
			add("error", key, "%q is not one of %s", v, strings.Join(allowed, " | "))
		}
	}
	between := func(key string, v, lo, hi float64) {
		if v < lo || v > hi {
			add("error", key, "%v is outside %v–%v", v, lo, hi)
		}
	}

	enum("general.log_level", c.General.LogLevel, "debug", "info", "warn", "error")

	between("server.port", float64(c.Server.Port), 1, 65535)
	enum("server.progress", c.Server.Progress, "thinking", "think_tags", "content", "off")
	enum("server.client_tools", c.Server.ClientTools, "merge", "client_only", "server_only")
	between("server.client_tools_budget_tokens", float64(c.Server.ClientToolsBudgetTokens), 0, 1e6)
	between("server.approval_wait_seconds", float64(c.Server.ApprovalWaitSeconds), 0, 3600)
	if ip := net.ParseIP(c.Server.Bind); ip == nil && c.Server.Bind != "localhost" {
		add("error", "server.bind", "%q is not an IP address", c.Server.Bind)
	} else if !IsLoopbackBind(c.Server.Bind) && len(c.Server.Tokens) == 0 {
		add("error", "server.bind", "binding to %s exposes noskill beyond this machine: add at least one [[server.tokens]] entry (PROXY.md)", c.Server.Bind)
	}
	names := map[string]bool{}
	for i, t := range c.Server.Tokens {
		key := fmt.Sprintf("server.tokens[%d]", i)
		if t.Name == "" {
			add("error", key, "every token needs a name (shown in traces)")
		} else if names[t.Name] {
			add("error", key, "duplicate token name %q", t.Name)
		}
		names[t.Name] = true
		secretRef(add, key+".token", t.Token, true)
	}

	if len(c.Backends) == 0 {
		add("warn", "backends", "no backends: run `noskill init`")
	}
	for i, b := range c.Backends {
		key := fmt.Sprintf("backends[%d]", i)
		enum(key+".kind", b.Kind, "ollama", "openai", "mlx", "vllm")
		if u, err := url.Parse(b.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("error", key+".url", "%q is not an http(s) URL", b.URL)
		}
		secretRef(add, key+".api_key", b.APIKey, false)
	}

	o := c.Orchestrator
	enum("orchestrator.preset", o.Preset, "fast", "balanced", "best")
	if !o.MemoryBudgetGB.Auto {
		between("orchestrator.memory_budget_gb", o.MemoryBudgetGB.Value, 1, 4096)
	}
	if !o.MemoryBurstGB.Auto {
		between("orchestrator.memory_burst_gb", o.MemoryBurstGB.Value, 0, 4096)
	}
	between("orchestrator.decision_cache_gb", o.DecisionCacheGB, 0, 64)
	between("orchestrator.min_swap_gain", o.MinSwapGain, 0, 1)
	between("orchestrator.hold_seconds", float64(o.HoldSeconds), 0, 86400)
	between("orchestrator.explore", o.Explore, 0, 1)
	between("orchestrator.default_ctx", float64(o.DefaultCtx), 2048, 1<<20)
	for i, m := range o.Exclude {
		if strings.TrimSpace(m) == "" {
			add("error", fmt.Sprintf("orchestrator.exclude[%d]", i), "a model name is empty")
		}
	}
	for i, r := range o.Rules {
		key := fmt.Sprintf("orchestrator.rules[%d]", i)
		if r.Use == "" {
			add("error", key+".use", "a rule needs a model in `use`")
		}
		if r.When.TaskType == "" && len(r.When.Needs) == 0 && r.When.Preset == "" {
			add("error", key+".when", "a rule needs task_type, needs or preset")
		}
		if r.When.Preset != "" && !slices.Contains([]string{"fast", "balanced", "best"}, r.When.Preset) {
			add("error", key+".when.preset", "%q is not fast, balanced or best", r.When.Preset)
		}
		if r.When.TaskType != "" && !slices.Contains(TaskTypes, r.When.TaskType) {
			add("warn", key+".when.task_type", "%q isn't a built-in task type (%s)", r.When.TaskType, strings.Join(TaskTypes, ", "))
		}
		for _, n := range r.When.Needs {
			if !slices.Contains(Needs, n) {
				add("error", key+".when.needs", "%q is not one of %s", n, strings.Join(Needs, ", "))
			}
		}
	}

	between("agent.max_steps", float64(c.Agent.MaxSteps), 1, 500)
	between("agent.max_tokens", float64(c.Agent.MaxTokens), 1000, 1e7)
	enum("agent.small_model_mode", c.Agent.SmallModelMode, "auto", "on", "off")
	enum("agent.tool_strategy", c.Agent.ToolStrategy, "auto", "native", "constrained", "prompted")

	enum("learning.promote_mode", c.Learning.PromoteMode, "ask", "shadow", "auto")
	between("learning.novelty_threshold", c.Learning.NoveltyThreshold, 0, 1)
	if len(strings.Fields(c.Learning.EvalSchedule)) != 5 {
		add("error", "learning.eval_schedule", "%q is not a 5-field cron expression", c.Learning.EvalSchedule)
	}

	for i, r := range c.Traces.Redact {
		key := fmt.Sprintf("traces.redact[%d]", i)
		if r.Regex != "" {
			if _, err := regexp.Compile(r.Regex); err != nil {
				add("error", key, "bad regex: %v", err)
			}
		} else if r.Builtin != "secrets" && r.Builtin != "emails" {
			add("error", key, "%q is not \"secrets\", \"emails\" or { regex = \"...\" }", r.Builtin)
		}
	}
	if !slices.ContainsFunc(c.Traces.Redact, func(r Redact) bool { return r.Builtin == "secrets" }) {
		add("warn", "traces.redact", "\"secrets\" is off: secret values can end up in traces")
	}

	between("scheduler.max_concurrent_jobs", float64(c.Scheduler.MaxConcurrentJobs), 1, 64)
	enum("scheduler.missed_policy", c.Scheduler.MissedPolicy, "skip", "once")
	if c.Approvals.DefaultOnTimeout != "skip" {
		add("error", "approvals.default_on_timeout", "must be \"skip\": an approval that times out never runs its action")
	}
	for _, ch := range c.Approvals.Channels {
		enum("approvals.channels", ch, "cli", "notification", "tray")
	}

	if n := c.Notify.Ntfy; n != nil {
		secretRef(add, "notify.ntfy.token", n.Token, false)
	}
	if t := c.Notify.Telegram; t != nil {
		secretRef(add, "notify.telegram.bot_token", t.BotToken, true)
	}
	if d := c.Notify.Discord; d != nil {
		secretRef(add, "notify.discord.webhook", d.Webhook, true)
	}
	if e := c.Notify.Email; e != nil {
		secretRef(add, "notify.email.password", e.Password, true)
	}

	enum("sandbox.mode", c.Sandbox.Mode, "enforce", "warn")
	if c.Sandbox.Mode == "warn" {
		add("warn", "sandbox.mode", "\"warn\" runs scripts unsandboxed: they can read your files and reach your network")
	}
	between("sandbox.script_memory_mb", float64(c.Sandbox.ScriptMemoryMB), 64, 1<<20)
	between("sandbox.script_max_procs", float64(c.Sandbox.ScriptMaxProcs), 1, 4096)
	between("sandbox.script_max_file_mb", float64(c.Sandbox.ScriptMaxFileMB), 1, 1<<20)

	for _, p := range c.Skills.Paths {
		ep, _ := platform.ExpandHome(p)
		if fi, err := os.Stat(ep); err != nil || !fi.IsDir() {
			add("warn", "skills.paths", "%s is not a folder", p)
		}
	}
	enum("update.channel", c.Update.Channel, "stable", "beta")

	if secrets != nil {
		for _, name := range SecretNames(c) {
			ok, err := secrets.Has(name)
			if err != nil {
				add("warn", "", "secret:%s: can't check the keychain: %v", name, err)
			} else if !ok {
				add("error", "", "secret:%s is referenced but not stored: run `noskill secrets set %s`", name, name)
			}
		}
	}
	return issues
}

// secretRef checks that a credential is a "secret:NAME" reference, never a value in the file.
func secretRef(add func(level, key, f string, a ...any), key, v string, required bool) {
	if v == "" {
		if required {
			add("error", key, "missing; use \"secret:NAME\" and `noskill secrets set NAME`")
		}
		return
	}
	if _, ok := SecretRef(v); !ok {
		add("error", key, "looks like a plain credential: store it with `noskill secrets set NAME` and write \"secret:NAME\" here")
	}
}

// SecretNames lists every secret the config references.
func SecretNames(c Config) []string {
	var out []string
	addRef := func(v string) {
		if n, ok := SecretRef(v); ok && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, t := range c.Server.Tokens {
		addRef(t.Token)
	}
	for _, b := range c.Backends {
		addRef(b.APIKey)
	}
	if n := c.Notify.Ntfy; n != nil {
		addRef(n.Token)
	}
	if t := c.Notify.Telegram; t != nil {
		addRef(t.BotToken)
	}
	if d := c.Notify.Discord; d != nil {
		addRef(d.Webhook)
	}
	if e := c.Notify.Email; e != nil {
		addRef(e.Password)
	}
	for _, s := range c.Skills.Settings {
		for _, v := range s {
			if str, ok := v.(string); ok {
				addRef(str)
			}
		}
	}
	return out
}

// IsLoopbackBind reports whether bind only listens on this machine.
func IsLoopbackBind(bind string) bool {
	if bind == "localhost" {
		return true
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

// HasErrors reports whether any issue is an error.
func HasErrors(issues []Issue) bool {
	return slices.ContainsFunc(issues, func(i Issue) bool { return i.Level == "error" })
}
