// Package policy evaluates the global execution flag (dry-run vs. auto) and
// the AD-group-to-access-tier RBAC rules (via OPA/Rego) before any command
// is dispatched to an agent.
package policy
