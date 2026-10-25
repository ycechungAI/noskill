// Package config loads config.toml with layered precedence (CONFIG.md): built-in defaults <
// config.toml < project ./.noskill/config.toml (skills only) < NOSKILL_<SECTION>_<KEY> < CLI flags.
// Every key remembers which layer set it, for `noskill config check --explain`.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	General      General      `toml:"general"`
	Server       Server       `toml:"server"`
	Backends     []Backend    `toml:"backends"`
	Orchestrator Orchestrator `toml:"orchestrator"`
	Agent        Agent        `toml:"agent"`
	Learning     Learning     `toml:"learning"`
	Memory       Memory       `toml:"memory"`
	Traces       Traces       `toml:"traces"`
	Scheduler    Scheduler    `toml:"scheduler"`
	Approvals    Approvals    `toml:"approvals"`
	Notify       Notify       `toml:"notify"`
	Sandbox      Sandbox      `toml:"sandbox"`
	Skills       Skills       `toml:"skills"`
	Tray         Tray         `toml:"tray"`
	Update       Update       `toml:"update"`
}

type General struct {
	Home      string `toml:"home"`
	LogLevel  string `toml:"log_level"`
	Telemetry bool   `toml:"telemetry"`
}

type Server struct {
	Port                    int      `toml:"port"`
	Bind                    string   `toml:"bind"`
	AllowedHosts            []string `toml:"allowed_hosts"`
	AllowedOrigins          []string `toml:"allowed_origins"`
	Tokens                  []Token  `toml:"tokens"`
	ExposeSkills            []string `toml:"expose_skills"`
	Progress                string   `toml:"progress"`
	ClientTools             string   `toml:"client_tools"`
	ClientToolsBudgetTokens int      `toml:"client_tools_budget_tokens"`
	PassthroughPlainNames   bool     `toml:"passthrough_plain_names"`
	AllowModelManagement    bool     `toml:"allow_model_management"`
	ApprovalsInChat         bool     `toml:"approvals_in_chat"`
	ApprovalWaitSeconds     int      `toml:"approval_wait_seconds"`
}

// Token is one proxy client (PROXY.md "Security"). Token must be a "secret:NAME" reference.
type Token struct {
	Name   string   `toml:"name"`
	Token  string   `toml:"token"`
	Skills []string `toml:"skills"`
	// Admin: the client sees every approval (also the ones jobs raise), may approve ones that need
	// exec, and may read /noskill/v1/status and control jobs. The tray and the status page use it.
	Admin bool `toml:"admin"`
}

type Backend struct {
	Kind   string `toml:"kind"`
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
}

type Orchestrator struct {
	Preset          string    `toml:"preset"`
	MemoryBudgetGB  AutoFloat `toml:"memory_budget_gb"`
	MemoryBurstGB   AutoFloat `toml:"memory_burst_gb"` // ceiling when free memory allows; 0 = never grow
	DecisionModel   string    `toml:"decision_model"`
	DecisionCacheGB float64   `toml:"decision_cache_gb"`
	EmbedModel      string    `toml:"embed_model"`
	MinSwapGain     float64   `toml:"min_swap_gain"`
	HoldSeconds     int       `toml:"hold_seconds"`
	Explore         float64   `toml:"explore"`
	DefaultCtx      int       `toml:"default_ctx"`
	// AutoTune lets calibration move decision thresholds from outcomes (FR-B6c). Off by
	// default: `noskill calibration` only reports until it's turned on.
	AutoTune bool `toml:"auto_tune"`
	// Exclude lists models the router never picks, by rule or by score, even when installed and
	// the memory is free (FR-O2). `run --model` still names one on purpose.
	Exclude []string `toml:"exclude"`
	Rules   []Rule   `toml:"rules"`
}

// Rule is a user routing rule (FR-O2): rules always win unless a hard need rules the model out.
type Rule struct {
	When RuleWhen `toml:"when"`
	Use  string   `toml:"use"`
}

type RuleWhen struct {
	TaskType string   `toml:"task_type"`
	Needs    []string `toml:"needs"`
	Preset   string   `toml:"preset"` // fast | balanced | best: the rule applies only under this preset (`run --preset`)
}

type Agent struct {
	MaxSteps       int    `toml:"max_steps"`
	MaxTokens      int    `toml:"max_tokens"`
	SmallModelMode string `toml:"small_model_mode"`
	ToolStrategy   string `toml:"tool_strategy"`
}

type Learning struct {
	Enabled               bool    `toml:"enabled"`
	PromoteMode           string  `toml:"promote_mode"`
	PromoteAfter          int     `toml:"promote_after"`
	MinToolCalls          int     `toml:"min_tool_calls"`
	NoveltyThreshold      float64 `toml:"novelty_threshold"`
	IdleMinutes           int     `toml:"idle_minutes"`
	RequireAC             bool    `toml:"require_ac"`
	MaxDraftsPerNight     int     `toml:"max_drafts_per_night"`
	MaxRefinesPerNight    int     `toml:"max_refines_per_night"`
	AuthorModel           string  `toml:"author_model"`
	EvalSchedule          string  `toml:"eval_schedule"`
	RegressionSuccessDrop float64 `toml:"regression_success_drop"`
	RegressionStepsRise   float64 `toml:"regression_steps_rise"`
	LoRA                  LoRA    `toml:"lora"`
}

// LoRA is weight learning (FR-L8, ADR-0015): `noskill train`.
type LoRA struct {
	MinRuns   int               `toml:"min_runs"`   // good runs a dataset needs before `train lora` will run
	MLXLM     string            `toml:"mlx_lm"`     // the mlx-lm version uv installs
	MLXModels map[string]string `toml:"mlx_models"` // Ollama base → MLX repository (4-bit: QLoRA)
}

type Memory struct {
	AutoExtract bool `toml:"auto_extract"`
	MaxItems    int  `toml:"max_items"`
}

type Traces struct {
	RetentionDays int      `toml:"retention_days"`
	Redact        []Redact `toml:"redact"`
}

type Scheduler struct {
	MaxConcurrentJobs int      `toml:"max_concurrent_jobs"`
	MissedPolicy      string   `toml:"missed_policy"`
	DefaultTimeout    Duration `toml:"default_timeout"`
	Retries           int      `toml:"retries"`
}

type Approvals struct {
	DefaultTimeout   Duration `toml:"default_timeout"`
	DefaultOnTimeout string   `toml:"default_on_timeout"`
	Channels         []string `toml:"channels"`
}

type Notify struct {
	OS       bool      `toml:"os"`
	Ntfy     *Ntfy     `toml:"ntfy"`
	Telegram *Telegram `toml:"telegram"`
	Discord  *Discord  `toml:"discord"`
	Email    *Email    `toml:"email"`
}

type Ntfy struct {
	URL   string `toml:"url"`
	Token string `toml:"token"`
}

type Telegram struct {
	BotToken string `toml:"bot_token"`
	ChatID   string `toml:"chat_id"`
}

type Discord struct {
	Webhook string `toml:"webhook"`
}

type Email struct {
	SMTP     string `toml:"smtp"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	To       string `toml:"to"`
}

type Sandbox struct {
	Mode            string   `toml:"mode"`
	ScriptTimeout   Duration `toml:"script_timeout"`
	ScriptMemoryMB  int      `toml:"script_memory_mb"`
	ScriptMaxProcs  int      `toml:"script_max_procs"`
	ScriptMaxFileMB int      `toml:"script_max_file_mb"`
}

// Skills holds [skills] and the per-skill tables [skills.<name>]. Settings is filled by the loader
// (the TOML decoder can't mix fixed keys and free tables in one struct).
type Skills struct {
	Paths      []string                  `toml:"paths"`
	AutoUpdate bool                      `toml:"auto_update"`
	Settings   map[string]map[string]any `toml:"-"`
}

type Tray struct {
	Enabled bool `toml:"enabled"`
}

type Update struct {
	Repo    string `toml:"repo"` // owner/name of the GitHub repository releases come from; empty until the org is chosen
	Check   bool   `toml:"check"`
	Auto    bool   `toml:"auto"`
	Channel string `toml:"channel"`
}

// Defaults returns the built-in defaults: the values in CONFIG.md's "Full reference".
func Defaults() Config {
	return Config{
		General: General{Home: "auto", LogLevel: "info"},
		Server: Server{
			Port: 11435, Bind: "127.0.0.1",
			AllowedHosts: []string{}, AllowedOrigins: []string{}, Tokens: []Token{},
			ExposeSkills: []string{"research", "local-docs", "watcher"},
			Progress:     "thinking", ClientTools: "merge", ClientToolsBudgetTokens: 1500,
			PassthroughPlainNames: true, AllowModelManagement: true,
			ApprovalsInChat: true, ApprovalWaitSeconds: 120,
		},
		Backends: []Backend{{Kind: "ollama", URL: "http://127.0.0.1:11434"}},
		Orchestrator: Orchestrator{
			Preset: "balanced", MemoryBudgetGB: AutoFloat{Auto: true}, MemoryBurstGB: AutoFloat{Auto: true},
			DecisionModel: "tev1:0.8b", DecisionCacheGB: 1, EmbedModel: "nomic-embed-text",
			MinSwapGain: 0.15, HoldSeconds: 120, Explore: 0.05, DefaultCtx: 8192,
			Rules: []Rule{{When: RuleWhen{TaskType: "code"}, Use: "qwen2.5-coder:7b"}},
		},
		Agent: Agent{MaxSteps: 25, MaxTokens: 60000, SmallModelMode: "auto", ToolStrategy: "auto"},
		Learning: Learning{
			Enabled: true, PromoteMode: "ask", PromoteAfter: 3, MinToolCalls: 5, NoveltyThreshold: 0.75,
			IdleMinutes: 10, RequireAC: true, MaxDraftsPerNight: 3, MaxRefinesPerNight: 3,
			AuthorModel: "auto", EvalSchedule: "0 4 * * 0", RegressionSuccessDrop: 5, RegressionStepsRise: 0.20,
			LoRA: LoRA{MinRuns: 200, MLXLM: "0.26.0", MLXModels: map[string]string{
				"qwen3:4b":    "mlx-community/Qwen3-4B-4bit",
				"qwen3:8b":    "mlx-community/Qwen3-8B-4bit",
				"llama3.2:3b": "mlx-community/Llama-3.2-3B-Instruct-4bit",
			}},
		},
		Memory:    Memory{AutoExtract: true, MaxItems: 2000},
		Traces:    Traces{RetentionDays: 90, Redact: []Redact{{Builtin: "secrets"}, {Builtin: "emails"}}},
		Scheduler: Scheduler{MaxConcurrentJobs: 1, MissedPolicy: "once", DefaultTimeout: Duration{30 * time.Minute}, Retries: 2},
		Approvals: Approvals{DefaultTimeout: Duration{24 * time.Hour}, DefaultOnTimeout: "skip", Channels: []string{"cli", "notification", "tray"}},
		Notify:    Notify{OS: true},
		Sandbox: Sandbox{Mode: "enforce", ScriptTimeout: Duration{60 * time.Second},
			ScriptMemoryMB: 1024, ScriptMaxProcs: 64, ScriptMaxFileMB: 256},
		Skills: Skills{Paths: []string{}, Settings: map[string]map[string]any{}},
		Tray:   Tray{Enabled: true},
		Update: Update{Check: true, Channel: "stable"},
	}
}

// AutoFloat is "auto" or a number (memory_budget_gb, memory_burst_gb).
type AutoFloat struct {
	Auto  bool
	Value float64
}

func (a *AutoFloat) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		if x == "auto" {
			*a = AutoFloat{Auto: true}
			return nil
		}
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return fmt.Errorf("want \"auto\" or a number, got %q", x)
		}
		*a = AutoFloat{Value: f}
	case int64:
		*a = AutoFloat{Value: float64(x)}
	case float64:
		*a = AutoFloat{Value: x}
	default:
		return fmt.Errorf("want \"auto\" or a number, got %T", v)
	}
	return nil
}

func (a AutoFloat) String() string {
	if a.Auto {
		return "auto"
	}
	return strconv.FormatFloat(a.Value, 'f', -1, 64)
}

// Duration accepts Go duration strings ("60s", "30m", "24h").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Redact is one [traces] redact entry: a built-in class ("secrets", "emails") or { regex = "..." }.
type Redact struct {
	Builtin string
	Regex   string
}

func (r *Redact) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*r = Redact{Builtin: x}
	case map[string]any:
		s, ok := x["regex"].(string)
		if !ok || len(x) != 1 {
			return fmt.Errorf("want \"secrets\", \"emails\" or { regex = \"...\" }")
		}
		*r = Redact{Regex: s}
	default:
		return fmt.Errorf("want a string or { regex = \"...\" }, got %T", v)
	}
	return nil
}

// SecretRef returns NAME for a "secret:NAME" value.
func SecretRef(v string) (string, bool) {
	name, ok := strings.CutPrefix(v, "secret:")
	return name, ok && name != ""
}
