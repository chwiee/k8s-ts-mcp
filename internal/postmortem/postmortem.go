// Package postmortem renders a plain-text incident summary from an
// audit.Entry: what happened, what was tried, and how it ended. It is
// deterministic template rendering over already-redacted audit data — no
// LLM call, no data beyond what the audit trail actually recorded, so it
// can never narrate a detail (or leak a secret) that wasn't really there.
package postmortem

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"

	"github.com/chwiee/k8s-ts-mcp/internal/audit"
)

//go:embed postmortem.md.tmpl
var rawTemplate string

var tmpl = template.Must(template.New("postmortem").Funcs(template.FuncMap{
	"statusOf": statusOf,
	"inc":      func(i int) int { return i + 1 },
}).Parse(rawTemplate))

// Render produces the plain-text postmortem for a single audit entry.
func Render(e audit.Entry) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, e); err != nil {
		return "", fmt.Errorf("rendering postmortem for entry %s: %w", e.ID, err)
	}
	return buf.String(), nil
}

func statusOf(e audit.Entry) string {
	switch {
	case e.Decision.DryRun:
		return "Não executado (dry-run) — comandos retornados ao usuário"
	case e.Result == nil:
		return "Execução não solicitada ou negada pela política"
	case e.Result.Resolved:
		return "Resolvido"
	default:
		return "Não resolvido após a escada de ações — necessária intervenção manual"
	}
}
