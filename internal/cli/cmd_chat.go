package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"noskill/internal/app"
	"noskill/internal/backends"
)

const chatHelp = `/help               this list
/exit               leave (Ctrl-D works too)
/clear              start a new conversation
/model <name|auto>  pin a model, or let noskill choose again
/skills <names|auto|off>  force skills, let noskill select, or turn them off
/rate good|partial|bad ["note"]   rate the last answer
/why                how the last answer was routed
/learn              turn the last answer into a skill (arrives with the learning loop, Phase 2)
End a line with \ to continue it, or wrap several lines in """.`

func cmdChat(c *cmd, args []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	var o runOptions
	o.bind(fs)
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	if c.env.StdinTTY {
		c.errf("noskill chat — /help for commands, Ctrl-D to leave\n")
	}
	var history []backends.Message
	var last *app.RunResult
	skillsOff := false
	for {
		text, err := c.readInput()
		if err == io.EOF {
			return ExitOK
		}
		if err != nil {
			return c.fail(err)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "/") {
			switch fields := strings.Fields(text); fields[0] {
			case "/exit", "/quit":
				return ExitOK
			case "/help":
				c.errf("%s\n", chatHelp)
			case "/clear":
				history, last = nil, nil
				c.errf("new conversation\n")
			case "/model":
				switch {
				case len(fields) == 1:
					c.errf("model: %s\n", orDefault(o.model, "auto"))
				case fields[1] == "auto":
					o.model = ""
					c.errf("model: auto\n")
				default:
					if m, err := a.FindModel(ctx, fields[1]); err != nil {
						c.errf("noskill: %v\n", err)
					} else {
						o.model = m.Name
						c.errf("model: %s\n", m.Name)
					}
				}
			case "/skills":
				switch {
				case len(fields) == 1:
					c.errf("skills: %s\n", skillState(o.skills, skillsOff))
				case fields[1] == "auto":
					o.skills, skillsOff = nil, false
					c.errf("skills: auto\n")
				case fields[1] == "off":
					o.skills, skillsOff = nil, true
					c.errf("skills: off\n")
				default:
					o.skills, skillsOff = fields[1:], false
					c.errf("skills: %s\n", strings.Join(fields[1:], ", "))
				}
			case "/rate":
				if last == nil || len(fields) < 2 {
					c.errf("usage: /rate good|partial|bad [note] (after an answer)\n")
					break
				}
				if _, err := a.Rate(ctx, last.RunID, fields[1], strings.Join(fields[2:], " ")); err != nil {
					c.errf("noskill: %v\n", err)
				} else {
					c.errf("rated %s\n", fields[1])
				}
			case "/why":
				if last == nil {
					c.errf("nothing yet\n")
				} else {
					c.errf("%s", last.Explanation.Text())
					c.errf("run %s, trace %s\n", last.RunID, last.TracePath)
				}
			case "/learn":
				c.errf("Turning a run into a skill arrives with the learning loop (Phase 2).\n")
			default:
				c.errf("unknown command %s (/help)\n", fields[0])
			}
			continue
		}
		history = append(history, backends.Message{Role: backends.RoleUser, Content: text})
		rr, err := o.request(history)
		if err != nil {
			c.errf("noskill: %v\n", err)
			history = history[:len(history)-1]
			continue
		}
		rr.NoSkills = skillsOff
		res, err := c.execute(ctx, a, rr)
		if err != nil && ctx.Err() != nil {
			return 130
		}
		if err != nil {
			c.fail(err)
			history = history[:len(history)-1]
			continue
		}
		last = res
		switch {
		case res.Stop == "waiting":
			c.errf("(waiting for your approval: noskill approvals approve %s)\n", res.WaitingFor)
			history = history[:len(history)-1]
		default:
			history = append(history, backends.Message{Role: backends.RoleAssistant, Content: res.Answer})
		}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func skillState(s []string, off bool) string {
	switch {
	case off:
		return "off"
	case len(s) == 0:
		return "auto"
	}
	return strings.Join(s, ", ")
}

// readInput reads one message: a line, a line continued with a trailing backslash, or a block
// between """ lines.
func (c *cmd) readInput() (string, error) {
	prompt := ""
	if c.env.StdinTTY {
		prompt = "› "
	}
	line, err := c.readLine(prompt)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(line) == `"""` {
		var b strings.Builder
		for {
			l, err := c.readLine("… ")
			if err != nil || strings.TrimSpace(l) == `"""` {
				return b.String(), nil
			}
			b.WriteString(l + "\n")
		}
	}
	var b strings.Builder
	for strings.HasSuffix(line, `\`) {
		b.WriteString(strings.TrimSuffix(line, `\`) + "\n")
		next, err := c.readLine("… ")
		if err != nil {
			return b.String(), nil
		}
		line = next
	}
	b.WriteString(line)
	return b.String(), nil
}

var _ = context.Background
var _ = fmt.Sprint
